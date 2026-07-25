// Package cli assembles the Cobra command tree for `mu`. Charm's fang wraps the
// root at Execute time to style help/errors in the house visual language.
package cli

import (
	"encoding/json"
	"io"
	"os"

	"charm.land/lipgloss/v2"

	"github.com/charmbracelet/fang"
	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/modules"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// onHPC reports whether mu is running on an HPC login/compute node ($BC_HOST set, or the
// MU_SYSTEM override), matching the shell platform seam in init.sh.
func onHPC() bool {
	return os.Getenv("BC_HOST") != "" || os.Getenv("MU_SYSTEM") == "hpc"
}

// writeJSON prints v as indented JSON to stdout — the shared `--json` output path.
func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// HelpColorScheme themes fang's help/usage in the house language: ANSI colors
// (theme-aware, matching render's tables) instead of fang's truecolor default, with
// the title in cyan (ANSI 6) to match render.StatusTable titles. Chrome only —
// meaning still rides on glyphs, not color.
func HelpColorScheme(c lipgloss.LightDarkFunc) fang.ColorScheme {
	s := fang.AnsiColorScheme(c)
	s.Title = lipgloss.Color("6") // cyan — matches table titles
	return s
}

// HouseError replaces fang's black-on-red inverted "ERROR" badge with the house error
// tier line (glyph + message, Plain-aware) — the inverted status pill violates the
// color policy. It writes via render.Err to stderr (the same sink fang passes as w),
// generalizing the render.Err + os.Exit precedent in cp.go across all commands.
func HouseError(_ io.Writer, _ fang.Styles, err error) {
	// A code-only error (codeErr) carries an exit code but no message — the command
	// already rendered its own failure line, so skip an empty red error line here.
	if msg := err.Error(); msg != "" {
		render.Err(msg)
	}
}

// verbosityFlag is a boolean-style persistent flag (-v / -q) that moves the global render level
// the moment it's parsed — before any RunE runs — so verbosity is set independently of the
// PersistentPreRun chain, which Cobra runs only the deepest link of. Its zero prints "false"
// and its Type is "bool", so it reads and renders as an ordinary switch.
type verbosityFlag struct{ to render.Level }

func (verbosityFlag) String() string { return "false" }
func (verbosityFlag) Type() string   { return "bool" }
func (f verbosityFlag) Set(s string) error {
	if s == "true" {
		render.Verbosity = f.to
	}
	return nil
}

// Root builds the top-level `mu` command with all subcommand trees attached.
func Root() *cobra.Command {
	// sshfs is local-only, so name it in the blurb only off an HPC node (see below).
	blurb := "HPC toolkit: SSH/rsync helpers, queue and process views, and the git signwip\n" +
		"workflow. Run a command below, or `mu <command> --help`."
	if !onHPC() {
		blurb = "HPC toolkit: SSH/rsync helpers, sshfs mounts, queue and process views, and\n" +
			"the git signwip workflow. Run a command below, or `mu <command> --help`."
	}
	root := &cobra.Command{
		Use:   "mu",
		Short: "mayhl_utils — HPC toolkit",
		Long:  blurb,
		// fang/main own error + usage printing; RunE returns bare errors for
		// exit codes without Cobra also dumping usage on a runtime failure.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.Version = muVersion()                      // `mu --version` (fang) reports the build-stamped version
	setHelpTitle(root, "mayhl_utils — HPC toolkit") // for the intercepted house root help
	root.PersistentFlags().BoolVar(&render.PlainFlag, "plain", false,
		"borderless, tab-aligned tables (auto when piped; overrides MU_RENDER)")
	// -v / -q set the global output level for any command (persistent = inherited by every
	// child). -v reveals each command's verbose extras; -q trims to results + warnings + errors.
	root.PersistentFlags().VarP(verbosityFlag{render.LevelVerbose}, "verbose", "v",
		"show per-command detail (job chatter, cp per-file output, doctor per-check detail)")
	// --quiet is long-only: -q is already --queue on job sub / tunnel / project submit, and a
	// duplicate shorthand panics pflag when the persistent flag merges into those commands.
	root.PersistentFlags().Var(verbosityFlag{render.LevelQuiet}, "quiet",
		"results, warnings, and errors only — suppress info headers and gray detail")
	root.PersistentFlags().Lookup("verbose").NoOptDefVal = "true"
	root.PersistentFlags().Lookup("quiet").NoOptDefVal = "true"
	root.MarkFlagsMutuallyExclusive("verbose", "quiet")
	root.AddCommand(cpCmd(), tarCmd(), hpcCmd(), setupCmd(), logCmd(), doctorCmd(), psCmd(), jobCmd(), pathCmd(), configCmd(), wrapCmd())
	// sshfs mounts a remote dir onto the LOCAL workstation via fuse — inapplicable on an
	// HPC login node (already on the box, no fuse-t), so register it local-only. Mirrors
	// the shell seam, where the hcd/hmt front-doors live in platform/local.sh.
	if !onHPC() {
		root.AddCommand(sshfsCmd())
	}
	// Opt-in modules (MU_MODULES): core stays always-on; new modules register only
	// when listed, so nothing existing changes for a user who hasn't opted in.
	if modules.Enabled("git") {
		root.AddCommand(gitCmd())
	}
	if modules.Enabled("project") {
		root.AddCommand(projectCmd(), archiveCmd())
	}
	// Recording (asciinema → svg); opt-in with its mise tier (MU_MODULES∋cast). Both machines
	// — HPC-specific tutorials record on the login node (capture is an interactive shell).
	if modules.Enabled("cast") {
		root.AddCommand(castCmd())
	}
	// shell-init and completion moved under `setup`; keep them reachable at the root
	// as HIDDEN aliases so existing rc lines (`eval "$(mu shell-init)"`, `mu completion
	// zsh`) don't break. Cobra's default completion command stays functional, just
	// hidden from the root menu.
	root.AddCommand(hidden(shellInitCmd()))
	root.AddCommand(hidden(rpathCompleteCmd())) // __rpath: data source for the zsh remote-path completer
	root.CompletionOptions.HiddenDefaultCmd = true
	// House help on every subcommand: each direct child gets the house renderer and its
	// subtree inherits it (Cobra resolves the nearest SetHelpFunc). fang overrides only
	// the ROOT's help func at Execute, so `mu --help` stays fang-styled by necessity;
	// wrapping the children keeps every `mu <cmd> --help` in the house language.
	for _, c := range root.Commands() {
		wrapHelp(c)
	}
	// Advertise that shell front-doors exist, so they're discoverable from the top level.
	// mps/mlog are portable; the sshfs h* set is local-only, so only list it off-HPC. A
	// command's own shortcuts live in its `mu <command> --help`.
	shortcuts := [][2]string{
		{"mps", "list local processes (mu ps; -i = picker)"},
		{"mlog", "view the event log (mu log)"},
	}
	if !onHPC() {
		shortcuts = append(
			shortcuts,
			[2]string{"hcd <name>", "mount + cd into an sshfs dir (mu sshfs)"},
			[2]string{"hmt <name>…", "mount sshfs dirs, no cd (mu sshfs mount)"},
			[2]string{"hls", "list sshfs mounts (mu sshfs list)"},
		)
	}
	setHelpShortcuts(root, shortcuts...)
	setHelpShortcutsNote(root, "Many commands have a short shell front-door that saves typing — a few below; "+
		"a command's own set shows in its `mu <command> --help`.")
	return root
}

// withUse re-verbs a command for mounting under a second parent — the same leaf builder
// is called again (Cobra needs a distinct instance) and its Use word is overridden. Used
// for the doctor reverse aliases (`mu setup doctor` ⇔ `mu doctor setup`).
func withUse(c *cobra.Command, use string) *cobra.Command {
	c.Use = use
	return c
}

// hidden marks a command hidden (kept functional, dropped from help) — for the
// root-level aliases of commands whose home is now a submodule.
func hidden(c *cobra.Command) *cobra.Command {
	c.Hidden = true
	return c
}
