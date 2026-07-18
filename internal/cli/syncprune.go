package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/project"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/rsync"
)

// pruneDefaultTiers are the tiers a prune touches when --tier is omitted: the reproducible
// data tiers only. raw ($HOME acquired truth) is never pruned — deleting source-of-truth
// because it left the laptop is the worst-case loss, so it stays a deliberate manual act.
var pruneDefaultTiers = []string{"sim", "processed"}

// projectSyncPruneCmd is `mu project sync prune <node> [path]`: the mirror-delete side of
// the push. It removes files staged on the cluster that no longer exist locally — the
// cleanup the add-only push deliberately never does. A distinct destructive verb, sibling
// to pull/status, so a delete is never folded into the everyday push. Reproducible tiers
// only (sim, processed); raw is refused outright.
func projectSyncPruneCmd() *cobra.Command {
	var yes, dryRun bool
	var tierSel, exclude []string
	c := &cobra.Command{
		Use:   "prune <node> [path]",
		Short: "Delete staged files on a cluster that no longer exist locally (mirror-delete).",
		Long: "Delete files staged on the target that have no local counterpart — the cleanup\n" +
			"the add-only push never does. Its own verb, not a push flag, so a destructive\n" +
			"delete is never triggered by the everyday sync.\n\n" +
			"An optional path narrows the prune to one dataset/subtree under a tier; without\n" +
			"one, every selected --tier (default: sim, processed) is pruned.\n\n" +
			"Reproducible tiers only: raw (data/raw, $HOME acquired source-of-truth) is never\n" +
			"pruned — remove it by hand if ever intended.\n\n" +
			"Delete-only and safe: a dry pass lists exactly which remote files would go; the\n" +
			"real pass deletes those and transfers nothing (no push, no overwrite). A tier that\n" +
			"is empty locally is refused, never mirror-emptied on the cluster. The sync manifest\n" +
			"is preserved and updated to drop the pruned entries. Shows the plan and confirms.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			var path string
			if len(args) > 1 {
				path = args[1]
			}
			return projectSyncPrune(projSyncOpts{node: args[0], path: path, tierSel: tierSel, exclude: exclude, yes: yes, dryRun: dryRun, verbose: render.IsVerbose()})
		},
	}
	setHelpArgs(
		c,
		[2]string{"<node>", "cluster to prune staged data on"},
		[2]string{"[path]", "narrow the prune to one dataset/subtree under a tier"},
	)
	f := c.Flags()
	f.BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	f.BoolVarP(&dryRun, "dry-run", "n", false, "list what would be deleted without deleting")
	f.StringSliceVar(&tierSel, "tier", nil, "tiers to prune: sim, processed (default: both; raw is never pruned)")
	f.StringArrayVar(&exclude, "exclude", nil, "extra rsync exclude pattern (repeatable; protects matching remote files from deletion)")
	c.ValidArgsFunction = func(_ *cobra.Command, args []string, tc string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return hpc.CompleteNode(tc), cobra.ShellCompDirectiveNoFileComp
		case 1:
			return nil, cobra.ShellCompDirectiveDefault
		default:
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
	return c
}

// projectSyncPrune resolves the prunable tiers (reproducible only, raw refused), classifies
// each tier's remote-only files against the local tree, presents one combined plan, and —
// unless a dry run, and after a single confirm — deletes them and trims the manifests. A
// tier empty locally is skipped with a warning: rsync --delete against an empty sender would
// wipe the entire remote tier, which prune must never do by accident.
func projectSyncPrune(o projSyncOpts) error {
	root, err := project.FindRoot(".")
	if err != nil {
		return usageErr("%s", err)
	}

	tiers, err := resolvePruneTiers(root, o)
	if err != nil {
		return usageErr("%s", err)
	}
	if len(tiers) == 0 {
		render.Info("no reproducible-tier data to prune under " + root)
		return nil
	}

	target, err := hpc.Resolve(o.node)
	if err != nil {
		return usageErr("%s", err)
	}

	render.Info("Prune staged data → " + o.node)
	render.Detail("project: " + root)

	if err := hpc.EnsureTicket(); err != nil {
		return runErr("%s", err)
	}

	// An --exclude also protects a matching remote file from --delete, so the manifest is
	// never mistaken for extraneous data and removed (prune trims it explicitly instead).
	excludes := pruneExcludes(o.exclude)

	type pruneRes struct {
		tier    syncTier
		dest    string
		deletes []string
	}
	var plan []pruneRes
	total := 0
	for _, t := range tiers {
		// Empty-local guard: an empty or absent local tier would make --delete remove the
		// entire remote tier — almost never intended. Refuse and say why.
		local, lerr := listTierFiles(t.localAbs)
		if lerr != nil {
			return runErr("scan %s: %s", t.rel, lerr)
		}
		if len(local) == 0 {
			render.Warn(fmt.Sprintf("%s is empty locally — refusing to prune (would delete the entire remote tier); remove it by hand if intended", t.rel))
			continue
		}
		dest, derr := resolveRemoteDir(target, o.node, t.remoteRoot, t.rel)
		if derr != nil {
			return derr
		}
		exists, eerr := remoteDirExists(target, dest)
		if eerr != nil {
			return runErr("%s: check remote %s: %s", o.node, t.rel, eerr)
		}
		if !exists {
			continue // nothing staged there
		}
		dels, perr := classifyPrune(target, t.localAbs, dest, excludes)
		if perr != nil {
			return runErr("%s: classify prune %s: %s", o.node, t.rel, perr)
		}
		if len(dels) == 0 {
			continue
		}
		plan = append(plan, pruneRes{tier: t, dest: dest, deletes: dels})
		total += len(dels)
		render.Detail(fmt.Sprintf("tier:    %s → %s/%s  (%d to delete)", t.rel, t.remoteRoot, t.rel, len(dels)))
	}

	if total == 0 {
		render.OK(o.node + ": nothing to prune (remote matches local)")
		return nil
	}

	render.Warn(fmt.Sprintf("%d staged file(s) on %s are absent locally and will be DELETED", total, o.node))
	for _, pr := range plan {
		listPaths("delete", pr.deletes)
	}

	if o.dryRun {
		render.Info("dry run — nothing deleted")
		return nil
	}
	if !o.yes {
		fmt.Fprintf(os.Stderr, "DELETE %d file(s) on %s? [y/N] ", total, o.node)
		var r string
		_, _ = fmt.Scanln(&r)
		if strings.ToLower(strings.TrimSpace(r)) != "y" {
			render.Info("aborted")
			return nil
		}
	}

	for _, pr := range plan {
		if err := pruneTier(target, pr.tier, pr.dest, excludes, o); err != nil {
			return err
		}
		// Trim the manifest to match — advisory, the files are already gone.
		updatePruneManifest(target, o.node, pr.dest, pr.deletes)
	}
	msg := fmt.Sprintf("pruned %d file(s) on %s", total, o.node)
	render.OK(msg)
	render.EventOK("project", msg)
	return nil
}

// resolvePruneTiers resolves the tiers to prune, enforcing the raw exclusion. A path
// argument narrows to one tier (refused for raw); otherwise the --tier selection (default:
// sim + processed) is taken and every selected tier that exists locally is returned. raw is
// never eligible either way.
func resolvePruneTiers(root string, o projSyncOpts) ([]syncTier, error) {
	if o.path != "" {
		if len(o.tierSel) > 0 {
			return nil, fmt.Errorf("--tier and a path argument are mutually exclusive")
		}
		t, err := narrowTier(root, o.path)
		if err != nil {
			return nil, err
		}
		if t.remoteRoot == "$HOME" { // raw is the sole $HOME-rooted tier
			return nil, fmt.Errorf("data/raw is never pruned (acquired source-of-truth); delete it by hand if intended")
		}
		return []syncTier{t}, nil
	}
	sel := o.tierSel
	if len(sel) == 0 {
		sel = pruneDefaultTiers
	}
	for _, s := range sel {
		if s == "raw" {
			return nil, fmt.Errorf("data/raw is never pruned (acquired source-of-truth); delete it by hand if intended")
		}
	}
	specs, err := resolveTiers(sel)
	if err != nil {
		return nil, err
	}
	var tiers []syncTier
	for _, t := range specs {
		abs := filepath.Join(root, t.rel)
		if fi, e := os.Stat(abs); e != nil || !fi.IsDir() {
			continue
		}
		rel, rerr := project.HomeRel(abs)
		if rerr != nil {
			return nil, rerr
		}
		tiers = append(tiers, syncTier{rel: rel, localAbs: abs, remoteRoot: t.remoteRoot})
	}
	return tiers, nil
}

// pruneExcludes stacks the manifest's own basename on the standard sync excludes: an
// --exclude protects a matching receiver file from --delete, so the manifest is never
// deleted as extraneous data (prune updates it explicitly).
func pruneExcludes(user []string) []string {
	return append(syncExcludes(user), syncManifestName)
}

// classifyPrune runs a dry delete-only rsync (local srcAbs → remote destAbs) and returns
// the receiver-relative paths --delete would remove. --existing + --ignore-existing suppress
// every send, so the dry pass reports deletions alone; the excludes protect the manifest and
// junk from deletion. Args are hand-built — no push/pull semantics apply here.
func classifyPrune(target, srcAbs, destAbs string, excludes []string) (deletes []string, err error) {
	transport := hpc.AmbientTransport(target)
	args := []string{"-a", "-i", "-n", "--delete", "--existing", "--ignore-existing"}
	for _, ex := range excludes {
		args = append(args, "--exclude", ex)
	}
	args = append(args, "-e", transport, srcAbs+"/", target+":"+destAbs+"/")
	cmd := exec.Command("rsync", args...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, rerr := cmd.Output()
	if rerr != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = rerr.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return parseDeletions(out), nil
}

// parseDeletions extracts the receiver paths rsync's --delete would remove from a dry
// itemized run. Deletions itemize as `*deleting   <path>`; a trailing slash marks a
// directory removal, which is skipped — the manifest keys files, and a directory falls once
// its files are gone. Each returned path is dest-relative in slash form.
func parseDeletions(out []byte) []string {
	var dels []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // long paths
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "*deleting")
		if !ok {
			continue
		}
		p := strings.TrimSpace(rest)
		if p == "" || strings.HasSuffix(p, "/") {
			continue
		}
		dels = append(dels, p)
	}
	return dels
}

// pruneTier deletes one tier's remote-only files. --existing + --ignore-existing suppress
// every send (no create, no update), so --delete is the sole effect — prune removes, never
// pushes. The excludes protect the manifest and junk. A non-zero rsync exit fails the prune.
func pruneTier(target string, t syncTier, dest string, excludes []string, o projSyncOpts) error {
	transport := hpc.AmbientTransport(target)
	args := []string{"-a", "--delete", "--existing", "--ignore-existing"}
	for _, ex := range excludes {
		args = append(args, "--exclude", ex)
	}
	args = append(args, "-e", transport, t.localAbs+"/", target+":"+dest+"/")
	label := "prune " + o.node + " " + t.rel
	code, _ := rsync.Run(args, label, o.verbose)
	if code != 0 {
		render.EventErr("project", fmt.Sprintf("%s FAILED (rsync exit %d)", label, code))
		return codeErr(code)
	}
	return nil
}
