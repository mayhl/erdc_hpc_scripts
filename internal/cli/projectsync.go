package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/project"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/rsync"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// tierSpec is one syncable SHARED-zone tier: its path under the project root and the
// remote root it lands under. Placement is reproducibility-driven: irreproducible
// source-of-truth (raw) → $HOME; bulky-reproducible data (sim-ready, processed) →
// $WORKDIR, purge-tolerant because rebuildable.
type tierSpec struct {
	name       string // --tier selector token
	rel        string // path under the project root
	remoteRoot string // "$WORKDIR" or "$HOME" — resolved on the target before rsync
}

// syncTiers is the registry of selectable tiers. Default (no --tier) ships only the
// sim-ready data — cross-cluster consistent, the daily 80%. processed and raw are
// opt-in via --tier (raw never auto-ships: local source-of-truth by default).
var syncTiers = []tierSpec{
	{name: "sim", rel: "simulations/data", remoteRoot: "$WORKDIR"},
	{name: "processed", rel: "data/processed", remoteRoot: "$WORKDIR"},
	{name: "raw", rel: "data/raw", remoteRoot: "$HOME"},
}

// defaultTiers names the tiers shipped when --tier is omitted.
var defaultTiers = []string{"sim"}

// resolveTiers maps --tier selector tokens to their specs in registry order, rejecting
// unknown tokens. An empty selection falls back to defaultTiers.
func resolveTiers(sel []string) ([]tierSpec, error) {
	if len(sel) == 0 {
		sel = defaultTiers
	}
	want := map[string]bool{}
	for _, s := range sel {
		want[s] = true
	}
	var out []tierSpec
	for _, t := range syncTiers {
		if want[t.name] {
			out = append(out, t)
			delete(want, t.name)
		}
	}
	if len(want) > 0 {
		bad := make([]string, 0, len(want))
		for s := range want {
			bad = append(bad, s)
		}
		sort.Strings(bad)
		valid := make([]string, len(syncTiers))
		for i, t := range syncTiers {
			valid[i] = t.name
		}
		return nil, fmt.Errorf("unknown tier(s): %s (valid: %s)", strings.Join(bad, ", "), strings.Join(valid, ", "))
	}
	return out, nil
}

// mtimeWindowSec is the mtime skew tolerance for the differs check: files matching
// on size and within this many seconds count as identical, absorbing FS-granularity
// and timezone-second noise between the laptop and the cluster. The coarse hours/days
// "dest ~3d older" hint is a later phase; the default check is just cheap size + mtime
// (--checksum opts into a full compare, impractical by default on 10-50 GB data).
const mtimeWindowSec = 2

// syncResult is one tier's classification: what a push would create (new) vs the
// existing files that differ (update — skipped by the additive default).
type syncResult struct {
	rel        string
	localAbs   string
	remoteRoot string
	dest       string   // resolved absolute remote dir (for the storage pre-flight's quota match)
	newPaths   []string // src-relative paths of the new files (their local bytes = the push size)
	updates    []string
}

// syncTier is one tier resolved for this run: rel is the $HOME-relative mirror path
// (its position under $WORKDIR/$HOME), localAbs the local source dir, remoteRoot the
// shell-var root ("$WORKDIR"/"$HOME") the tier lands under.
type syncTier struct{ rel, localAbs, remoteRoot string }

// projSyncOpts carries one `mu project sync` invocation's resolved flags and args.
type projSyncOpts struct {
	node     string
	path     string // optional subtree to narrow to (relative to cwd); "" = whole tiers
	tierSel  []string
	force    bool     // overwrite differing files (loud) instead of skipping them
	checksum bool     // classify by full checksum, not size+mtime (opt-in, expensive)
	verify   bool     // after the push, sha256 each pushed file end-to-end (opt-in, reads both ends)
	exclude  []string // extra excludes, stacked on the built-in junk set
	yes      bool
	dryRun   bool
	verbose  bool
}

// builtinExcludes are the junk patterns `mu project sync` always drops — editor and OS
// cruft, rsync's own partial dir, transient temp/lock files — so they never count as
// syncable data. This layer operates on the gitignored data tiers; it is independent of
// .gitignore.
var builtinExcludes = []string{".DS_Store", ".rsync-partial", "*.swp", "*.tmp", "*.lock"}

// syncExcludes stacks the caller's --exclude patterns on the built-in junk set. Classify
// and transfer must pass the identical list, or classify would count an excluded file as
// new while the transfer skips it.
func syncExcludes(user []string) []string {
	out := make([]string, 0, len(builtinExcludes)+len(user))
	out = append(out, builtinExcludes...)
	return append(out, user...)
}

// narrowTier resolves an optional path argument to a single tier. The path (relative to
// cwd) must live under one of the syncable tiers, whose remoteRoot it inherits — this is
// how a sync targets one dataset/subtree instead of a whole tier. The mirror model does
// the rest: HomeRel names the subtree's position, so it lands at the same relative spot.
func narrowTier(root, path string) (syncTier, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return syncTier{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return syncTier{}, err
	}
	if !fi.IsDir() {
		return syncTier{}, fmt.Errorf("%s is not a directory (narrow to a dataset/subtree)", path)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return syncTier{}, fmt.Errorf("%s is outside the project (%s)", path, root)
	}
	for _, t := range syncTiers {
		if rel == t.rel || strings.HasPrefix(rel, t.rel+string(os.PathSeparator)) {
			hrel, herr := project.HomeRel(abs)
			if herr != nil {
				return syncTier{}, herr
			}
			return syncTier{rel: hrel, localAbs: abs, remoteRoot: t.remoteRoot}, nil
		}
	}
	rels := make([]string, len(syncTiers))
	for i, t := range syncTiers {
		rels[i] = t.rel
	}
	return syncTier{}, fmt.Errorf("%s is not under a syncable tier (%s)", rel, strings.Join(rels, ", "))
}

// fleetAuto is the sentinel --fleet takes with no value (pflag NoOptDefVal): resolve
// the fleet from the project's markers/.mu-fleet rather than an explicit list.
const fleetAuto = "@auto"

// projectSyncCmd is `mu project sync <node>`: the production-run data path. It pushes
// the project's SHARED-zone run-dependency data (simulations/data) to a cluster's
// $WORKDIR at the same $HOME-relative path, additively — new files transfer, existing
// files are never overwritten (add-only house rule). Differing files are reported and
// skipped, not resolved. Distinct from submit-iterate's disposable case staging.
func projectSyncCmd() *cobra.Command {
	var yes, dryRun, force, checksum, verify bool
	var tierSel, exclude []string
	var fleet string
	c := &cobra.Command{
		Use:   "sync <node> [path]",
		Short: "Push shared run-dependency data (simulations/data) to a cluster's $WORKDIR.",
		Long: "Push the project's shared-zone data — simulations/data, the sim-ready\n" +
			"model-format inputs runs depend on — to the target's $WORKDIR at the same\n" +
			"$HOME-relative path. The production-run data path, distinct from submit's\n" +
			"disposable case staging.\n\n" +
			"An optional path narrows the push to one dataset/subtree under a tier; without\n" +
			"one, every selected --tier that exists locally is pushed.\n\n" +
			"--fleet fans the same push out to every node the project touches — the union of\n" +
			"its .mu-node markers, a .mu-fleet override, or an explicit --fleet=a,b list.\n" +
			"Shared data is consistent-everywhere, so no node-lock applies; each node runs the\n" +
			"full push and a failure on one doesn't strand the rest.\n\n" +
			"Additive and add-only: a dry itemized pass classifies each file as new (dest\n" +
			"absent), identical, or differing (size or mtime); the real transfer pushes only\n" +
			"the new ones (rsync --ignore-existing). Differing files are listed and SKIPPED —\n" +
			"shared data is never silently overwritten. Detection is cheap size + mtime, not\n" +
			"a full checksum. Shows the plan and confirms before transferring.",
		Args: cobra.RangeArgs(0, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			o := projSyncOpts{tierSel: tierSel, force: force, checksum: checksum, verify: verify, exclude: exclude, yes: yes, dryRun: dryRun, verbose: render.IsVerbose()}
			if fleet != "" {
				// Fleet mode fans out over the node set; a positional is the optional path.
				if len(args) > 0 {
					o.path = args[0]
				}
				return projectSyncFleet(fleet, o)
			}
			if len(args) == 0 {
				return usageErr("a <node> is required (or --fleet to fan out to the project's fleet)")
			}
			o.node = args[0]
			if len(args) > 1 {
				o.path = args[1]
			}
			return projectSync(o)
		},
	}
	setHelpArgs(
		c,
		[2]string{"<node>", "cluster to push shared data to"},
		[2]string{"[path]", "narrow the push to one dataset/subtree under a tier"},
	)
	f := c.Flags()
	f.BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	f.BoolVarP(&dryRun, "dry-run", "n", false, "classify and show the plan without transferring")
	f.StringSliceVar(&tierSel, "tier", nil, "tiers to sync: sim, processed, raw (default: sim; raw → $HOME)")
	f.BoolVarP(&force, "force", "f", false, "overwrite differing files instead of skipping them (loud)")
	f.BoolVarP(&checksum, "checksum", "c", false, "compare by full checksum, not size+mtime (opt-in; reads both ends)")
	f.BoolVar(&verify, "verify", false, "after the push, sha256 each pushed file end-to-end and confirm the remote matches (opt-in; reads both ends)")
	f.StringArrayVar(&exclude, "exclude", nil, "extra rsync exclude pattern (repeatable; stacks on the built-in junk set)")
	f.StringVar(&fleet, "fleet", "", "fan out to every node the project touches (bare = .mu-node markers or .mu-fleet; =a,b for an explicit list)")
	f.Lookup("fleet").NoOptDefVal = fleetAuto
	c.ValidArgsFunction = func(cmd *cobra.Command, args []string, tc string) ([]string, cobra.ShellCompDirective) {
		if cmd.Flags().Changed("fleet") {
			return nil, cobra.ShellCompDirectiveDefault // fleet mode: only an optional path
		}
		switch len(args) {
		case 0:
			return hpc.CompleteNode(tc), cobra.ShellCompDirectiveNoFileComp
		case 1:
			return nil, cobra.ShellCompDirectiveDefault // path arg: default file completion
		default:
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
	}
	c.AddCommand(projectSyncPullCmd())
	c.AddCommand(projectSyncStatusCmd())
	c.AddCommand(projectSyncPruneCmd())
	return c
}

// projectSyncFleet fans the SHARED-zone push out to every node the project touches.
// The node set comes from an explicit --fleet=a,b list, else project.Fleet (the
// .mu-fleet override or the .mu-node marker union). Shared data is consistent-
// everywhere, so — unlike submit — no node-lock applies; it may go to any node. Each
// node runs the full syncShared push; a per-node failure is reported and the fan-out
// continues, so one unreachable cluster doesn't strand the rest.
func projectSyncFleet(spec string, o projSyncOpts) error {
	root, err := project.FindRoot(".")
	if err != nil {
		return usageErr("%s", err)
	}

	var nodes []string
	var source string
	if spec == fleetAuto {
		nodes, source, err = project.Fleet(root)
		if err != nil {
			return usageErr("%s", err)
		}
		if len(nodes) == 0 {
			return usageErr("no fleet to fan out to — the project declares no %s markers; add one, a %s file, or pass --fleet=<nodes>", project.AffinityFile, project.FleetFile)
		}
	} else {
		nodes = splitFleet(spec)
		source = "flag"
		if len(nodes) == 0 {
			return usageErr("--fleet lists no nodes")
		}
	}

	render.Info(fmt.Sprintf("Fan out shared data → %d node(s): %s", len(nodes), strings.Join(nodes, ", ")))
	render.Detail("project: " + root)
	render.Detail("fleet:   " + source)

	var failed []string
	for _, n := range nodes {
		render.Info("── " + n + " ──")
		no := o
		no.node = n
		if err := syncShared(root, no); err != nil {
			render.Warn(fmt.Sprintf("%s: %s", n, err))
			failed = append(failed, n)
		}
	}
	if len(failed) > 0 {
		return runErr("fan-out incomplete — %d of %d node(s) failed: %s", len(failed), len(nodes), strings.Join(failed, ", "))
	}
	render.OK(fmt.Sprintf("fanned out shared data to %d node(s)", len(nodes)))
	return nil
}

// splitFleet parses a --fleet=a,b,c list into node tokens, dropping blanks and
// surrounding whitespace.
func splitFleet(spec string) []string {
	var out []string
	for _, s := range strings.Split(spec, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// projectSync resolves the project (from cwd), classifies every shared tier that
// exists locally, presents one combined plan, and — unless a dry run, and after a
// single confirm — pushes the new files additively. One pass classifies, a separate
// additive pass transfers, so the report and the write never disagree.
func projectSync(o projSyncOpts) error {
	root, err := project.FindRoot(".")
	if err != nil {
		return usageErr("%s", err)
	}
	return syncShared(root, o)
}

// anchorNote surfaces headless mode and the primary-cluster anchor in a sync plan.
// In headless mode the primary cluster is the hub peers fan out FROM, so pushing
// shared data from a non-anchor source risks diverging from it — we warn but proceed,
// the same soft guard-not-whitelist shape as the case node-lock and the default-mode
// laptop-hub warning. Silent off headless (the laptop-hub default needs no note).
func anchorNote() {
	if !config.Headless() {
		return
	}
	primary := config.PrimaryCluster()
	if primary == "" {
		render.Detail("headless: on (no primary anchor — set [project] primary in config.toml)")
		return
	}
	render.Detail("headless: on (primary " + primary + ")")
	if self, _ := currentCluster(); self != "" && self != primary {
		render.Warn(fmt.Sprintf("shared data is being pushed from %s, not the anchor %s — fan out from the anchor to avoid divergence", self, primary))
	}
}

// syncShared pushes the project's SHARED-zone tiers for an already-resolved root — the
// callable core behind `mu project sync` (root from cwd) and submit --clean's data leg
// (root from the case's project). Additive + add-only, identical on both paths.
func syncShared(root string, o projSyncOpts) error {
	// A path argument narrows to one subtree (mutually exclusive with --tier, which
	// selects whole tiers); otherwise take every selected tier that exists locally.
	var tiers []syncTier
	if o.path != "" {
		if len(o.tierSel) > 0 {
			return usageErr("--tier and a path argument are mutually exclusive")
		}
		t, nerr := narrowTier(root, o.path)
		if nerr != nil {
			return usageErr("%s", nerr)
		}
		tiers = []syncTier{t}
	} else {
		specs, serr := resolveTiers(o.tierSel)
		if serr != nil {
			return usageErr("%s", serr)
		}
		// rel is the $HOME-relative path (the mirror position under $WORKDIR/$HOME);
		// t.rel is where the tier sits under the project root.
		for _, t := range specs {
			abs := filepath.Join(root, t.rel)
			if fi, e := os.Stat(abs); e != nil || !fi.IsDir() {
				continue
			}
			rel, rerr := project.HomeRel(abs)
			if rerr != nil {
				return usageErr("%s", rerr)
			}
			tiers = append(tiers, syncTier{rel: rel, localAbs: abs, remoteRoot: t.remoteRoot})
		}
		if len(tiers) == 0 {
			looked := make([]string, len(specs))
			for i, t := range specs {
				looked[i] = t.rel
			}
			render.Info("no shared-zone data to sync under " + root + " (looked for: " + strings.Join(looked, ", ") + ")")
			return nil
		}
	}

	target, err := hpc.Resolve(o.node)
	if err != nil {
		return usageErr("%s", err)
	}

	render.Info("Sync shared data → " + o.node)
	render.Detail("project: " + root)
	anchorNote()

	if err := hpc.EnsureTicket(); err != nil {
		return runErr("%s", err)
	}

	// Classify pass: resolve each tier's $WORKDIR dest (no mkdir — a dry compare
	// against a missing dir correctly reports every file as new) and itemize.
	results := make([]syncResult, 0, len(tiers))
	totalNew, totalUpd := 0, 0
	for _, t := range tiers {
		dest, derr := resolveRemoteDir(target, o.node, t.remoteRoot, t.rel)
		if derr != nil {
			return derr
		}
		newPaths, updates, cerr := classifySync(target, t.localAbs, dest, o.checksum, syncExcludes(o.exclude))
		if cerr != nil {
			return runErr("%s: classify %s: %s", o.node, t.rel, cerr)
		}
		results = append(results, syncResult{rel: t.rel, localAbs: t.localAbs, remoteRoot: t.remoteRoot, dest: dest, newPaths: newPaths, updates: updates})
		totalNew += len(newPaths)
		totalUpd += len(updates)
		render.Detail(fmt.Sprintf("tier:    %s → %s/%s  (%d new, %d differ)", t.rel, t.remoteRoot, t.rel, len(newPaths), len(updates)))
	}

	// Advisory disk-quota pre-flight: how much the new files add vs each destination
	// filesystem's headroom. Never blocks — soft warn only.
	storagePreflight(o.node, results)

	if totalUpd > 0 {
		if o.force {
			render.Warn(fmt.Sprintf("%d file(s) differ on %s and will be OVERWRITTEN (--force)", totalUpd, o.node))
		} else {
			render.Warn(fmt.Sprintf("%d file(s) differ on %s and will be SKIPPED (add-only — new version = new filename; --force to overwrite)", totalUpd, o.node))
		}
		listUpdates(results)
		// raw is the strictest tier (acquired source-of-truth): flag an overwrite of it
		// extra loud. It is the sole $HOME-rooted tier, so the root identifies it.
		if o.force {
			for _, res := range results {
				if res.remoteRoot == "$HOME" && len(res.updates) > 0 {
					render.Warn("--force is overwriting data/raw, the acquired source-of-truth — the strictest tier")
					break
				}
			}
		}
	}

	pushN := totalNew
	if o.force {
		pushN += totalUpd
	}
	if pushN == 0 {
		if totalUpd == 0 {
			render.OK(o.node + ": already in sync")
		} else {
			render.Info("nothing new to push (differing files skipped)")
		}
		return nil
	}
	if o.force {
		render.Detail(fmt.Sprintf("plan:    push %d new, overwrite %d differing", totalNew, totalUpd))
	} else {
		render.Detail(fmt.Sprintf("plan:    push %d new file(s), skip %d differing", totalNew, totalUpd))
	}

	if o.dryRun {
		render.Info("dry run — nothing transferred")
		return nil
	}
	if !o.yes {
		prompt := fmt.Sprintf("push %d new file(s) to %s?", totalNew, o.node)
		if o.force && totalUpd > 0 {
			prompt = fmt.Sprintf("push %d new + OVERWRITE %d differing file(s) on %s?", totalNew, totalUpd, o.node)
		}
		if !confirm("%s", prompt) {
			render.Info("aborted")
			return nil
		}
	}

	// Transfer pass: one push per tier that has files to move — new files always,
	// plus the differs when --force is set.
	for _, res := range results {
		move := len(res.newPaths)
		if o.force {
			move += len(res.updates)
		}
		if move == 0 {
			continue
		}
		if err := pushTier(target, res, o); err != nil {
			return err
		}
	}
	msg := fmt.Sprintf("synced %d file(s) → %s", pushN, o.node)
	render.OK(msg)
	render.EventOK("project", msg)

	// Opt-in end-to-end integrity check: rsync already verifies each file it transfers,
	// so this is the paranoid confirmation for critical pushes — an independent sha256
	// on both ends. Advisory: a mismatch warns loudly but the transfer already happened.
	// Its digests feed the manifest below, so a verified push records sha256 for free.
	var digests map[string]string
	if o.verify {
		digests = verifyPushed(target, o.node, results, o.force)
	}

	// Record what landed into each tier's remote manifest (source commit + per-file
	// size/mtime, plus sha256 when verified) so the staged data self-describes its
	// version. Advisory — the push already succeeded.
	writeManifest(target, root, o.node, results, o, digests)
	return nil
}

// pushTier transfers one tier to its remote dest. The default is additive — only new
// files move (rsync --ignore-existing). With --force it also overwrites differing files;
// that path builds the rsync args by hand so the env layer's -u (skip receiver-newer)
// can't leave a flagged differ in place — force must overwrite everything classify
// reported, including a remote-newer file, so the report and the write never disagree.
func pushTier(target string, res syncResult, o projSyncOpts) error {
	dest, err := ensureRemoteDir(target, o.node, res.remoteRoot, res.rel)
	if err != nil {
		return err
	}
	src := res.localAbs + "/"
	dst := target + ":" + dest + "/"
	label := "sync " + o.node + " " + res.rel

	excludes := syncExcludes(o.exclude)
	var args []string
	if o.force {
		transport := hpc.AmbientTransport(target)
		// -a only (no -u): overwrite regardless of transfer direction. --partial-dir
		// matches rsync.partialDir, keeping resumable partials out of the dest tree.
		args = []string{"-a", "--partial-dir=.rsync-partial"}
		if o.checksum {
			args = append(args, "-c")
		}
		for _, ex := range excludes {
			args = append(args, "--exclude", ex)
		}
		args = append(args, "-e", transport, src, dst)
	} else {
		args = rsync.BuildArgs(src, dst, rsync.Opts{PartialDir: true, Checksum: o.checksum, Exclude: excludes, Ropt: []string{"--ignore-existing"}, Transport: hpc.AmbientTransport(target)})
	}
	code, _ := rsync.Run(args, label, o.verbose)
	if code != 0 {
		render.EventErr("project", fmt.Sprintf("%s FAILED (rsync exit %d)", label, code))
		return codeErr(code)
	}
	return nil
}

// pullRel is the sole pull tier: run output lives under results/, rooted remotely at
// $WORKDIR (the case's cluster owns it), pulled back to the same $HOME-relative path.
const pullRel = "results"

// projectSyncPullCmd is `mu project sync pull <node> [path]`: the reverse of the push. It
// brings a project's run output (results/) back from a cluster's $WORKDIR to the same
// $HOME-relative path locally. The conflict model flips — the remote is authoritative, so
// a differing file overwrites the local copy, EXCEPT one that is newer locally, which is
// listed and skipped (--force pulls it anyway); local work is never silently discarded.
func projectSyncPullCmd() *cobra.Command {
	var yes, dryRun, force, checksum bool
	var exclude []string
	c := &cobra.Command{
		Use:   "pull <node> [path]",
		Short: "Pull run output (results/) back from a cluster's $WORKDIR.",
		Long: "Bring the project's results/ — run output computed on the cluster — back from\n" +
			"the target's $WORKDIR to the same $HOME-relative path locally. The reverse of\n" +
			"the push data path.\n\n" +
			"An optional path narrows the pull to one subtree of results/.\n\n" +
			"The remote is authoritative: new files are pulled and files newer on the cluster\n" +
			"overwrite the local copy. A file that is NEWER locally is listed and SKIPPED —\n" +
			"local work is never silently discarded; --force pulls it anyway. Detection is\n" +
			"cheap size + mtime, not a full checksum. Shows the plan and confirms first.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			var path string
			if len(args) > 1 {
				path = args[1]
			}
			return projectSyncPull(projSyncOpts{node: args[0], path: path, force: force, checksum: checksum, exclude: exclude, yes: yes, dryRun: dryRun, verbose: render.IsVerbose()})
		},
	}
	setHelpArgs(
		c,
		[2]string{"<node>", "cluster to pull results from"},
		[2]string{"[path]", "narrow the pull to one subtree of results/"},
	)
	f := c.Flags()
	f.BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	f.BoolVarP(&dryRun, "dry-run", "n", false, "classify and show the plan without transferring")
	f.BoolVarP(&force, "force", "f", false, "overwrite local-newer files instead of skipping them (loud)")
	f.BoolVarP(&checksum, "checksum", "c", false, "compare by full checksum, not size+mtime (opt-in; reads both ends)")
	f.StringArrayVar(&exclude, "exclude", nil, "extra rsync exclude pattern (repeatable; stacks on the built-in junk set)")
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

// projectSyncPull resolves the pull target (results/ or a subtree, whose local dir need
// not exist yet), verifies the remote has data, classifies by direction, presents the
// plan, and — after a confirm — pulls the new and remote-newer files. Local-newer files
// are skipped unless --force. Classify and transfer share the same explicit -u policy, so
// the report and the write never disagree.
func projectSyncPull(o projSyncOpts) error {
	root, err := project.FindRoot(".")
	if err != nil {
		return usageErr("%s", err)
	}

	// The pull tier is results/ (or a subtree of it); the local dir need not exist yet.
	rel := pullRel
	localAbs := filepath.Join(root, pullRel)
	if o.path != "" {
		abs, aerr := filepath.Abs(o.path)
		if aerr != nil {
			return usageErr("%s", aerr)
		}
		r, rerr := filepath.Rel(root, abs)
		if rerr != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
			return usageErr("%s is outside the project (%s)", o.path, root)
		}
		if r != pullRel && !strings.HasPrefix(r, pullRel+string(os.PathSeparator)) {
			return usageErr("%s is not under results/ (pull is results-only)", r)
		}
		rel, localAbs = r, abs
	}
	hrel, herr := project.HomeRel(localAbs)
	if herr != nil {
		return usageErr("%s", herr)
	}

	target, err := hpc.Resolve(o.node)
	if err != nil {
		return usageErr("%s", err)
	}

	render.Info("Pull results ← " + o.node)
	render.Detail("project: " + root)

	if err := hpc.EnsureTicket(); err != nil {
		return runErr("%s", err)
	}

	src, serr := resolveRemoteDir(target, o.node, "$WORKDIR", hrel)
	if serr != nil {
		return serr
	}
	exists, eerr := remoteDirExists(target, src)
	if eerr != nil {
		return runErr("%s: check remote results: %s", o.node, eerr)
	}
	if !exists {
		render.Info(fmt.Sprintf("nothing to pull — %s not present on %s", rel, o.node))
		return nil
	}

	newN, overwrite, localNewer, cerr := classifyPull(target, src, localAbs, o.checksum, syncExcludes(o.exclude))
	if cerr != nil {
		return runErr("%s: classify %s: %s", o.node, rel, cerr)
	}
	render.Detail(fmt.Sprintf("tier:    %s ← $WORKDIR/%s  (%d new, %d overwrite, %d local-newer)", rel, hrel, newN, len(overwrite), len(localNewer)))

	if len(localNewer) > 0 {
		if o.force {
			render.Warn(fmt.Sprintf("%d file(s) are newer locally than on %s and will be OVERWRITTEN (--force)", len(localNewer), o.node))
		} else {
			render.Warn(fmt.Sprintf("%d file(s) are newer locally than on %s and will be SKIPPED (--force to pull and overwrite)", len(localNewer), o.node))
		}
		listPaths("local-newer", localNewer)
	}

	pullN := newN + len(overwrite)
	if o.force {
		pullN += len(localNewer)
	}
	if pullN == 0 {
		if len(localNewer) == 0 {
			render.OK(o.node + ": already in sync")
		} else {
			render.Info("nothing to pull (local-newer files skipped)")
		}
		return nil
	}
	if o.force {
		render.Detail(fmt.Sprintf("plan:    pull %d new, overwrite %d remote-newer + %d local-newer", newN, len(overwrite), len(localNewer)))
	} else {
		render.Detail(fmt.Sprintf("plan:    pull %d new, overwrite %d (remote authoritative), skip %d local-newer", newN, len(overwrite), len(localNewer)))
	}

	if o.dryRun {
		render.Info("dry run — nothing transferred")
		return nil
	}
	if !o.yes {
		if !confirm("pull %d file(s) from %s?", pullN, o.node) {
			render.Info("aborted")
			return nil
		}
	}

	label := "pull " + o.node + " " + rel
	if err := pullResults(target, src, localAbs, label, o); err != nil {
		return err
	}
	msg := fmt.Sprintf("pulled %d file(s) ← %s", pullN, o.node)
	render.OK(msg)
	render.EventOK("project", msg)
	return nil
}

// remoteDirExists reports whether abs is a directory on target, using a shell test that
// exits 0 either way so a "not a dir" answer isn't mistaken for a connection failure.
func remoteDirExists(target, abs string) (bool, error) {
	out, err := hpc.RemoteExec(target, fmt.Sprintf(`[ -d %s ] && printf yes || printf no`, shell.Quote(abs)))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

// classifyPull runs two dry itemized pulls (remote → local) to split the differences by
// direction, which a single itemize can't reveal. Pass A (no -u) lists every would-pull
// file; pass B (-u) lists those the receiver isn't newer for — new files and remote-newer
// differs (the overwrite set). Paths in A but not B are the ones newer locally. Both passes
// hand-build args with an explicit -u toggle, so the split never depends on the user-
// overridable env rsync opts.
func classifyPull(target, srcAbs, destLocal string, checksum bool, excludes []string) (newN int, overwrite, localNewer []string, err error) {
	_, diffA, err := pullItemize(target, srcAbs, destLocal, false, checksum, excludes)
	if err != nil {
		return 0, nil, nil, err
	}
	newN, diffB, err := pullItemize(target, srcAbs, destLocal, true, checksum, excludes)
	if err != nil {
		return 0, nil, nil, err
	}
	overwrite = diffB
	inB := make(map[string]bool, len(diffB))
	for _, p := range diffB {
		inB[p] = true
	}
	for _, p := range diffA {
		if !inB[p] {
			localNewer = append(localNewer, p)
		}
	}
	return newN, overwrite, localNewer, nil
}

// pullItemize runs one dry itemized pull (remote srcAbs → local destLocal) and returns the
// new-file count and the differing-file paths. update adds -u (skip receiver-newer);
// checksum swaps the size+mtime quick-check for a full compare. Args are hand-built (not
// BuildArgs) so -u is explicit, never inherited from the env opts.
func pullItemize(target, srcAbs, destLocal string, update, checksum bool, excludes []string) (newN int, differs []string, err error) {
	transport := hpc.AmbientTransport(target)
	args := []string{"-a", "-i", "-n", fmt.Sprintf("--modify-window=%d", mtimeWindowSec)}
	if update {
		args = append(args, "-u")
	}
	if checksum {
		args = append(args, "-c")
	}
	for _, ex := range excludes {
		args = append(args, "--exclude", ex)
	}
	args = append(args, "-e", transport, target+":"+srcAbs+"/", destLocal+"/")
	cmd := exec.Command("rsync", args...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, rerr := cmd.Output()
	if rerr != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = rerr.Error()
		}
		return 0, nil, fmt.Errorf("%s", msg)
	}
	np, d := classifyItemize(out)
	return len(np), d, nil
}

// pullResults transfers results/ from the remote src to the local dst, creating the local
// dir first. Args are hand-built with an explicit -u (dropped under --force) so local-newer
// files are protected regardless of the env opts — the classify report and this write must
// agree. --force overwrites them.
func pullResults(target, remoteSrc, localDst, label string, o projSyncOpts) error {
	if err := os.MkdirAll(localDst, 0o755); err != nil {
		return runErr("staging local dir: %s", err)
	}
	transport := hpc.AmbientTransport(target)
	args := []string{"-a", "--partial-dir=.rsync-partial"}
	if !o.force {
		args = append(args, "-u")
	}
	if o.checksum {
		args = append(args, "-c")
	}
	for _, ex := range syncExcludes(o.exclude) {
		args = append(args, "--exclude", ex)
	}
	args = append(args, "-e", transport, target+":"+remoteSrc+"/", localDst+"/")
	code, _ := rsync.Run(args, label, o.verbose)
	if code != 0 {
		render.EventErr("project", fmt.Sprintf("%s FAILED (rsync exit %d)", label, code))
		return codeErr(code)
	}
	return nil
}

// resolveRemoteDir returns the absolute path of <remoteRoot>/<rel> on target (remoteRoot
// is a shell var ref, "$WORKDIR" or "$HOME") without creating it — the classify pass
// needs the path, not the directory (rsync's dry compare against a missing dest reports
// every file as new, which is correct). Resolving to an absolute path here keeps the
// root var out of rsync's remote arg, where shell-expansion is fragile.
func resolveRemoteDir(target, node, remoteRoot, rel string) (string, error) {
	qrel := shell.Quote(rel)
	out, err := hpc.RemoteExec(target, fmt.Sprintf(`printf '%%s' "%s"/%s`, remoteRoot, qrel))
	if err != nil {
		return "", runErr("%s: resolve %s: %s", node, remoteRoot, err)
	}
	dest := strings.TrimSpace(out)
	if dest == "" || dest == "/"+rel {
		return "", runErr("%s: %s is unset on the target", node, remoteRoot)
	}
	return dest, nil
}

// ensureRemoteDir resolves <remoteRoot>/<rel> and mkdir -p's it, returning the absolute
// path — rsync creates only the final dest dir, not a missing chain, so the parent must
// exist before the transfer.
func ensureRemoteDir(target, node, remoteRoot, rel string) (string, error) {
	qrel := shell.Quote(rel)
	out, err := hpc.RemoteExec(target, fmt.Sprintf(`mkdir -p "%s"/%s && cd "%s"/%s && pwd`, remoteRoot, qrel, remoteRoot, qrel))
	if err != nil {
		return "", runErr("%s: staging dir: %s", node, err)
	}
	dest := strings.TrimSpace(out)
	if dest == "" {
		return "", runErr("%s: staging dir: empty %s resolution", node, remoteRoot)
	}
	return dest, nil
}

// classifySync runs a dry itemized compare of srcAbs against the remote destAbs and
// splits the would-transfer files into new (dest absent) and update (dest exists,
// differs). Identical files never itemize. The compare is a cheap size + mtime quick-
// check by default; checksum=true opts into a full both-ends checksum (expensive on
// 10-50 GB data). It builds the rsync args directly rather than via BuildArgs so the env
// layer's -u (skip files newer on the receiver) can't hide a remote-newer file from the
// differs report; the real transfer, which does inherit the env layer, only ever pushes
// new files anyway (unless --force, which bypasses the env layer too).
func classifySync(target, srcAbs, destAbs string, checksum bool, excludes []string) (newPaths, updates []string, err error) {
	transport := hpc.AmbientTransport(target)
	args := []string{"-a", "-i", "-n", fmt.Sprintf("--modify-window=%d", mtimeWindowSec)}
	if checksum {
		args = append(args, "-c") // full checksum compare, not size+mtime (opt-in, expensive)
	}
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
		return nil, nil, fmt.Errorf("%s", msg)
	}
	newPaths, updates = classifyItemize(out)
	return newPaths, updates, nil
}

// classifyItemize splits the raw output of `rsync --itemize-changes -n` into new and
// differing regular files, returning each set's src-relative paths. A transferred file
// itemizes with leading '<' (sent — the push case) or '>' (received — a pull); all-'+'
// attribute flags mean newly created (new), anything else means it exists and differs
// (update). Non-file lines and attr-only ('.') / dir-create ('c') lines are ignored.
func classifyItemize(out []byte) (newPaths, updates []string) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // long paths
	for sc.Scan() {
		item, path, ok := parseItemize(sc.Text())
		if !ok || item[1] != 'f' || (item[0] != '<' && item[0] != '>') { // regular files being transferred
			continue
		}
		if item[2:] == "+++++++++" {
			newPaths = append(newPaths, path)
		} else {
			updates = append(updates, path)
		}
	}
	return newPaths, updates
}

// parseItemize splits an `rsync --itemize-changes` line into its 11-char flag field
// and the path. ok=false for anything that isn't an itemized change (blank lines,
// stats, messages). The flag field is "YXcstpoguax": Y=update type, X=file type, the
// rest per-attribute (+ = newly created).
func parseItemize(line string) (item, path string, ok bool) {
	if len(line) < 13 || line[11] != ' ' {
		return "", "", false
	}
	item, path = line[:11], line[12:]
	switch item[1] { // file type must be a known code
	case 'f', 'd', 'L', 'D', 'S':
		return item, path, true
	}
	return "", "", false
}

// listUpdates prints the differing files (capped) so the skip is auditable, not
// silent — the add-only rule is "refuse + list, never resolve".
func listUpdates(results []syncResult) {
	const cap_ = 20
	shown := 0
	for _, res := range results {
		for _, u := range res.updates {
			if shown >= cap_ {
				render.Detail(fmt.Sprintf("  ... and %d more", countUpdates(results)-cap_))
				return
			}
			render.Detail("  differs: " + u)
			shown++
		}
	}
}

func countUpdates(results []syncResult) int {
	n := 0
	for _, res := range results {
		n += len(res.updates)
	}
	return n
}

// listPaths prints a capped, labelled list of paths so a skip/overwrite is auditable, not
// silent — the same "refuse + list" rule the push side applies to differing files.
func listPaths(label string, paths []string) {
	const cap_ = 20
	for i, p := range paths {
		if i >= cap_ {
			render.Detail(fmt.Sprintf("  ... and %d more", len(paths)-cap_))
			return
		}
		render.Detail("  " + label + ": " + p)
	}
}
