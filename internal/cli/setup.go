package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/shellinit"
)

// setupCmd is `mu setup`: the one-time commands that set mu up in your shell —
// grouped out of the root menu (root = the everyday verbs). Bare `mu setup` shows
// help and changes nothing; `--eval <shell>` prints the combined shell-init +
// completion snippet for one-line rc wiring. `completion` and `shell-init` also
// live at the root as hidden aliases so existing rc lines keep working.
func setupCmd() *cobra.Command {
	var evalShell string
	c := &cobra.Command{
		Use:   "setup",
		Short: "Set up mu in your shell (completion + shell integration).",
		Long: "One-time commands to set mu up in your shell. To wire everything in a single\n" +
			"line, add this to your shell config (bash, zsh, or fish):\n\n" +
			"    eval \"$(mu setup --eval zsh)\"\n\n" +
			"Or use the subcommands below to print the pieces individually.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if evalShell == "" {
				return cmd.Help()
			}
			// Render completion into a buffer FIRST so an unsupported shell errors
			// before any partial output reaches stdout (which `eval` would consume).
			var comp bytes.Buffer
			if err := writeCompletion(cmd.Root(), evalShell, &comp); err != nil {
				return err
			}
			// Combined one-line wire: shell integration first, then completion.
			fmt.Print(shellinit.Generate())
			fmt.Fprintln(os.Stdout)
			_, err := os.Stdout.Write(comp.Bytes())
			return err
		},
	}
	c.Flags().StringVar(&evalShell, "eval", "", "print shell-init + completion to `eval` at rc time (bash|zsh|fish)")
	// `mu setup doctor` mirrors `mu doctor setup` — same leaf, re-verbed so both directions work.
	c.AddCommand(nodeHelpCmd(), shellInitCmd(), setupCompletionCmd(), onboardCmd(), setupPushCmd(), toolchainCmd(), syncCmd(),
		checksCmd(), setupGlyphsCmd(), withUse(doctorSetupCmd(), "doctor"))
	return c
}

// setupCompletionCmd is `mu setup completion <shell>` — the visible relocation of
// Cobra's default completion command (which stays functional at the root as a hidden
// alias, so `mu completion` still works).
func setupCompletionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:       "completion [bash|zsh|fish]",
		Short:     "Generate a shell completion script.",
		Long:      "Print the completion script for the given shell. Usually wired via\n`mu setup --eval <shell>`; this prints just the completion half.",
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"bash", "zsh", "fish"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeCompletion(cmd.Root(), args[0], os.Stdout)
		},
	}
	setHelpArgs(c, [2]string{"[bash|zsh|fish]", "shell to emit the completion script for"})
	return c
}

// writeCompletion generates the completion script for one shell from the root
// command — shared by `setup completion` and `setup --eval`.
func writeCompletion(root *cobra.Command, shell string, w io.Writer) error {
	switch shell {
	case "bash":
		return root.GenBashCompletionV2(w, true)
	case "zsh":
		if err := root.GenZshCompletion(w); err != nil {
			return err
		}
		_, err := io.WriteString(w, zshCompletionExtras())
		return err
	case "fish":
		return root.GenFishCompletion(w, true)
	default:
		return fmt.Errorf("unsupported shell %q (want bash, zsh, or fish)", shell)
	}
}

// zshCompletionExtras is the hand-written zsh completion appended to cobra's generated script.
// Cobra's _describe shows AND inserts the full completion value, so a remote-path arg lists as
// clutter of full paths; here we override those args with grouped BASENAMES via compadd -p —
// the same data (mu __rpath, riding the cached master-only probe), styled by zsh like omz's
// file completion. It wires the completer onto the `<node> <verb>` front-doors (_mu_node) and
// layers it over cobra's _mu for `mu cp`/`sshfs`. Door names and verbs are interpolated from
// the same source as the shell integration (shellinit), so the completion can never drift.
func zshCompletionExtras() string {
	compdef := ""
	if nodes := shellinit.NodeDoorNames(); len(nodes) > 0 {
		compdef = "compdef _mu_node " + strings.Join(nodes, " ")
	}
	return strings.NewReplacer(
		"__VERBS__", strings.Join(shellinit.NodeVerbs(), " "),
		"__NODES_COMPDEF__", compdef,
	).Replace(zshExtrasBody)
}

// zshExtrasBody carries no backticks (Go raw string) and no % printf traps — placeholders are
// swapped by zshCompletionExtras. Classic zsh syntax throughout so a parse edge can't bite.
const zshExtrasBody = `
# ── mu remote-path completion (basename display, grouped like omz) ───────────────────────────
# _mu_rpath <node> <dirsOnly 0|1> — complete a remote path on <node> as grouped basenames. Data
# comes from 'mu __rpath', which rides the cached, master-only probe (no master → no completion,
# never a hang). compset -P '*/' keeps the already-typed dir off the line so we match basenames.
_mu_rpath() {
  emulate -L zsh
  local node=$1 dirsonly=$2 cur=$PREFIX
  [[ $cur == (/|'~'|'$')* && $cur == */* ]] || return 1   # only anchored paths past a slash
  local dir=${cur%/*}/
  local -a entries
  entries=(${(f)"$(mu __rpath $node $dirsonly -- $dir 2>/dev/null)"})
  (( $#entries )) || return 1
  local -a dirs files
  local e
  for e in $entries; do
    if [[ $e == */ ]]; then dirs+=($e); else files+=($e); fi
  done
  compset -P '*/'
  local expl
  if (( $#dirs )); then
    _description -V mu-remote-dirs expl 'remote directory'
    compadd "$expl[@]" -S '' -- $dirs    # trailing / lives in the value; no space, so TAB descends
  fi
  if (( $#files )); then
    _description -V mu-remote-files expl 'remote file'
    compadd "$expl[@]" -- $files
  fi
  return 0
}

# _mu_node — completion for the '<node> <verb> …' front-doors (wheat, mike, …): the verb first,
# then remote paths on pull (source) and push (destination). Local args use plain file completion.
_mu_node() {
  emulate -L zsh
  local node=$words[1]
  if (( CURRENT == 2 )); then
    local -a verbs; verbs=(__VERBS__)
    _describe -t mu-node-verbs 'node verb' verbs
    return
  fi
  case $words[2] in
    pull) if (( CURRENT == 3 )); then _mu_rpath $node 0; else _files; fi ;;
    push) if (( CURRENT == 3 )); then _files; else _mu_rpath $node 1; fi ;;
    *) _default ;;
  esac
}
__NODES_COMPDEF__

# Wrap cobra's _mu so mu's OWN remote-path args render as basenames too. Any shape we don't
# recognize (flags mixed in, other verbs) falls straight through to cobra's original completer.
if (( $+functions[_mu] )); then
  functions[_mu_cobra]=$functions[_mu]
  _mu() {
    if [[ $words[2] == cp && $words[3] == (pull|push) ]]; then
      [[ $words[3] == pull && $CURRENT == 5 ]] && { _mu_rpath $words[4] 0; return }
      [[ $words[3] == push && $CURRENT == 6 ]] && { _mu_rpath $words[4] 1; return }
    fi
    [[ $words[2] == sshfs && $words[3] == add && $CURRENT == 6 ]] && { _mu_rpath $words[5] 1; return }
    _mu_cobra "$@"
  }
fi
`
