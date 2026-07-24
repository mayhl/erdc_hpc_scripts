package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/rsync"
)

func cpCmd() *cobra.Command {
	cp := &cobra.Command{
		Use:   "cp",
		Short: "Copy files to/from HPC nodes (rsync).",
	}
	cp.AddCommand(cpPushCmd(), cpPullCmd())
	return cp
}

func cpPushCmd() *cobra.Command {
	var o rsync.Opts
	cmd := &cobra.Command{
		Use:   "push <node> <src> [dst]",
		Short: "Copy a local path TO a node (rsync push), with a progress bar.",
		Long: "Copy a local path TO a node (rsync push), with a live progress bar.\n" +
			"With no <dst> the path lands in your home dir on the node.\n\n" + hpc.NodesHint(),
		Args:              cobra.RangeArgs(2, 3),
		ValidArgsFunction: cpPushCompletion,
		RunE: func(_ *cobra.Command, args []string) error {
			return runTransfer(true, args[0], args[1], transferDst(args, ""), o, render.IsVerbose())
		},
	}
	setHelpArgs(cmd,
		[2]string{"<node>", "target node or cluster alias from the configured inventory"},
		[2]string{"<src>", "local path to copy"},
		[2]string{"[dst]", "destination path on the node (~-relative or absolute); default: your home dir"})
	addTransferFlags(cmd, &o)
	return cmd
}

func cpPullCmd() *cobra.Command {
	var o rsync.Opts
	cmd := &cobra.Command{
		Use:   "pull <node> <src> [dst]",
		Short: "Copy a path FROM a node to local (rsync pull), with a progress bar.",
		Long: "Copy a path FROM a node TO local (rsync pull), with a live progress bar.\n" +
			"With no <dst> the path lands in the current directory.\n\n" + hpc.NodesHint(),
		Args:              cobra.RangeArgs(2, 3),
		ValidArgsFunction: cpPullCompletion,
		RunE: func(_ *cobra.Command, args []string) error {
			return runTransfer(false, args[0], args[1], transferDst(args, "."), o, render.IsVerbose())
		},
	}
	setHelpArgs(cmd,
		[2]string{"<node>", "source node or cluster alias from the configured inventory"},
		[2]string{"<src>", "remote path on the node (~-relative or absolute)"},
		[2]string{"[dst]", "local destination path; default: the current directory"})
	addTransferFlags(cmd, &o)
	return cmd
}

// transferDst is the optional 3rd arg, else the side's default: "" for a push (rsync
// reads a bare `node:` as the remote home dir) and "." for a pull (the local CWD) — the
// symmetric "no destination named → the natural landing spot on the receiving side".
func transferDst(args []string, def string) string {
	if len(args) > 2 {
		return args[2]
	}
	return def
}

// runTransfer resolves the node, ensures a Kerberos ticket, and runs rsync. For
// push the local src goes to node:dst; for pull node:src comes to local dst. It
// returns an exit-coded error so rsync's exit code propagates to the process
// (main maps it via ExitCode; fang would otherwise return 0), and an unknown-node
// error is a code-2 usage line without fang's ERROR block. The rsync line is
// code-only (progress bar + summary already printed), so nothing renders twice.
func runTransfer(push bool, node, a, b string, o rsync.Opts, verbose bool) error {
	target, err := hpc.Resolve(node)
	if err != nil {
		return usageErr("%s", err)
	}
	if err := hpc.EnsureTicket(); err != nil {
		return runErr("%s", err)
	}

	// Ride a single held ssh master so the transfer authenticates once — and so the seam is
	// ready for the flows that do more than one op over it (rsync-push then submit). If the
	// master can't open, fall back to rsync's own ssh so cp never regresses; a real
	// connection failure surfaces from rsync itself with a clearer message.
	if sess, serr := hpc.OpenSession(target, hpc.SessionOpts{}); serr == nil {
		o.Transport = sess.RsyncTransport()
		defer sess.Close()
		render.Verbose("riding a held ssh master to " + node)
	} else {
		render.Verbose(fmt.Sprintf("no held master (%v); rsync will open its own connection", serr))
	}

	src, dst, label := a, target+":"+b, "push "+node
	if !push {
		src, dst, label = target+":"+a, b, "pull "+node
	}
	code, summary := rsync.Run(rsync.BuildArgs(src, dst, o), label, verbose)
	// Durable event → events.log (log-only: the progress bar + summary already
	// handled the terminal). Skip a dry-run; it moved nothing.
	if !o.DryRun {
		if code == 0 {
			msg := label
			if summary != "" {
				msg += " — " + summary
			}
			render.EventOK("cp", msg)
		} else {
			render.EventErr("cp", fmt.Sprintf("%s FAILED (rsync exit %d)", label, code))
		}
	}
	return codeErr(code)
}

func addTransferFlags(cmd *cobra.Command, o *rsync.Opts) {
	f := cmd.Flags()
	f.BoolVarP(&o.DryRun, "dry-run", "n", false, "show what would transfer")
	f.StringArrayVar(&o.Exclude, "exclude", nil, "rsync exclude pattern (repeatable)")
	f.BoolVar(&o.ExcludeHidden, "exclude-hidden", false, "skip dotfiles/dot-dirs (adds --exclude '.*')")
	f.BoolVar(&o.Delete, "delete", false, "delete extraneous files on the destination")
	f.StringVar(&o.Bwlimit, "bwlimit", "", "rsync bandwidth limit, e.g. 10m")
	f.BoolVarP(&o.Compress, "compress", "z", false, "compress in transit (skip for pre-compressed data)")
	f.BoolVarP(&o.Checksum, "checksum", "c", false, "verify by checksum, not size+mtime")
	f.IntVar(&o.Timeout, "timeout", 0, "I/O timeout in seconds (0 = none)")
	f.BoolVar(&o.PartialDir, "partial-dir", true, "keep partials in .rsync-partial for cross-run resume (--partial-dir=false to disable)")
	f.StringArrayVar(&o.Ropt, "ropt", nil, "extra raw rsync option (repeatable)")
	// per-file output (vs the aggregate bar) rides the global -v now; no local flag
}

// cpPushCompletion: push <node> <local-src> [remote-dst]. The node completes from the
// inventory, the src is a LOCAL path (shell file completion), and the dst is a remote dir
// on the node (cached remote completer).
func cpPushCompletion(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return hpc.CompleteNode(toComplete), cobra.ShellCompDirectiveNoFileComp
	case 1:
		return nil, cobra.ShellCompDirectiveDefault // local src → shell file completion
	case 2:
		return completeRemotePath(args[0], toComplete, true) // remote dst (a directory)
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// cpPullCompletion: pull <node> <remote-src> [local-dst]. The src is a remote path on the
// node (files AND dirs, cached), the dst is a LOCAL path.
func cpPullCompletion(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return hpc.CompleteNode(toComplete), cobra.ShellCompDirectiveNoFileComp
	case 1:
		return completeRemotePath(args[0], toComplete, false) // remote src → files + dirs
	case 2:
		return nil, cobra.ShellCompDirectiveDefault // local dst
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}
