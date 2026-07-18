package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/archive"
	"github.com/mayhl/mayhl_utils/internal/project"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// archiveCmd is `mu archive`: the mirror-aware front-end to the site PST/TUSC
// archive command, behind the shell `archive` front-door (project module).
// Flag parsing is off — everything after the subcommand belongs to the site
// tool, not cobra.
func archiveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "archive <sub> [args...]",
		Short: "Run the site archive command against the mirror projection.",
		Long: "Wrap the PST/TUSC archive command (put/get/ls/…): the archive-side dir is\n" +
			"resolved from $PWD via `mu path archive` and injected as -C, ARCHIVE_PROBE=yes\n" +
			"turns on the native size verify, and a flagless `put` packs case material into\n" +
			"tar tiers first — one batch tar for a parent of small case leaves (under\n" +
			"[project] tar_parent_threshold), else one tar per leaf, landing at the leaf's\n" +
			"projection (…/case_a/250.tar) with the flat local name inside. An explicit -C\n" +
			"in the args passes through untouched.",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
				return cmd.Help()
			}
			archiveMasterGuard(args[0])
			return codeErr(archive.Run(args[0], args[1:]))
		},
	}
	setHelpArgs(c,
		[2]string{"<sub>", "archive subcommand (put, get, ls, …) — put gains the tar tiers"},
		[2]string{"[args...]", "passed through to the site command"})
	return c
}

// archiveMasterGuard warns when a `put` runs from a cluster that is not the project's
// designated archive master: a cross-cluster project archives to one DSRC's HSM, and
// archiving elsewhere scatters the data across the separate per-cluster archives. Scoped to
// put (the write that scatters); get/ls from the wrong cluster just find nothing. Best-
// effort and advisory (guard-not-whitelist) — silent off-HPC, outside a project, or with no
// master set; it never blocks the archive.
func archiveMasterGuard(sub string) {
	if sub != "put" {
		return
	}
	self, _ := currentCluster()
	if self == "" {
		return // off-HPC — nothing to compare
	}
	root, err := project.FindRoot(".")
	if err != nil {
		return // not inside a project
	}
	master, ok, merr := project.ArchiveMaster(root)
	if merr != nil || !ok || master == self {
		return
	}
	render.Warn(fmt.Sprintf("archiving from %s, not the archive master %s — data may scatter across per-cluster HSM archives", self, master))
}
