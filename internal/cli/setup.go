package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"

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
		return root.GenZshCompletion(w)
	case "fish":
		return root.GenFishCompletion(w, true)
	default:
		return fmt.Errorf("unsupported shell %q (want bash, zsh, or fish)", shell)
	}
}
