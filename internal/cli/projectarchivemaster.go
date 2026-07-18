package cli

import (
	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/project"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// projectArchiveMasterCmd is `mu project archive-master [node]`: read or set the project's
// designated archive-master — the single cluster its data is archived from. With no
// argument it shows the current setting; with a node it writes the marker; --clear removes
// it. A cross-cluster project has a separate HSM archive per DSRC, so picking one master
// keeps the project's archived data from scattering; `mu archive` warns when run elsewhere.
func projectArchiveMasterCmd() *cobra.Command {
	var clear bool
	c := &cobra.Command{
		Use:   "archive-master [node]",
		Short: "Show or set the cluster this project archives from (single HSM source-of-truth).",
		Long: "Designate the project's archive master — the one cluster whose per-DSRC HSM\n" +
			"($ARCHIVE) holds the project's archived data. A cross-cluster project archives\n" +
			"from every cluster otherwise, scattering and duplicating the data across the\n" +
			"separate per-DSRC archives.\n\n" +
			"With no argument, show the current master. With a node, write the .mu-archive\n" +
			"marker (a tracked declaration of intent, like .mu-node). --clear removes it.\n" +
			"`mu archive` reads the marker and warns when it runs from a different cluster.",
		Args: cobra.RangeArgs(0, 1),
		RunE: func(_ *cobra.Command, args []string) error {
			root, err := project.FindRoot(".")
			if err != nil {
				return usageErr("%s", err)
			}
			switch {
			case clear:
				if len(args) > 0 {
					return usageErr("--clear takes no node argument")
				}
				if err := project.ClearArchiveMaster(root); err != nil {
					return runErr("clear archive master: %s", err)
				}
				render.OK("archive master cleared")
				return nil
			case len(args) == 1:
				if err := project.SetArchiveMaster(root, args[0]); err != nil {
					return runErr("set archive master: %s", err)
				}
				render.OK("archive master set: " + args[0])
				render.Detail("marker:  " + project.ArchiveFile)
				return nil
			default:
				node, ok, aerr := project.ArchiveMaster(root)
				if aerr != nil {
					return runErr("%s", aerr)
				}
				if !ok {
					render.Info("no archive master set for this project (set one with `mu project archive-master <node>`)")
					return nil
				}
				render.Info("archive master: " + node)
				return nil
			}
		},
	}
	setHelpArgs(c, [2]string{"[node]", "cluster to set as the archive master (omit to show)"})
	c.Flags().BoolVar(&clear, "clear", false, "remove the archive-master marker")
	c.ValidArgsFunction = func(_ *cobra.Command, args []string, tc string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return hpc.CompleteNode(tc), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return c
}
