package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/project"
	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/rsync"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// projectCmd is `mu project`: verbs over the project-structure contract (submit,
// later sync/pull/runs/clone). Gated behind MU_MODULES=project while it settles.
func projectCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "project",
		Short: "Project-layer verbs: push-and-run cases, data-tier sync.",
	}
	c.AddCommand(projectSubmitCmd(), projectSyncCmd(), projectCloneCmd(), projectRunsCmd(), projectArchiveMasterCmd())
	return c
}

func projectSubmitCmd() *cobra.Command {
	var node, script, account, queue_ string
	var yes, dryRun, keep, clean, force, noSync bool
	var hours float64
	c := &cobra.Command{
		Use:   "submit <case-dir>",
		Short: "Push a case to a cluster's $WORK staging and submit it.",
		Long: "Iterate mode (default), the edit→run debug loop: rsync the case dir to the\n" +
			"target's $WORKDIR at the same $HOME-relative path (the remote git clone is\n" +
			"never touched), drop a submit-origin stamp (HEAD sha + dirty — `mu job prep`\n" +
			"folds it into run.toml), and qsub/sbatch the script from staging. Staging\n" +
			"mirrors the case dir exactly (stale files are deleted; scheduler logs from\n" +
			"prior runs are kept) — the disposable submit copy, not the authored source.\n\n" +
			"--clean, the study phase: refuse a dirty tree, push the branch through the\n" +
			"per-node remote (updateInstead refreshes the $HOME clone; `mu project clone`\n" +
			"bootstraps it), stage $HOME→$WORK on the node, submit — every run.toml then\n" +
			"carries a real reproducible sha. Pre-flight refuses a diverged remote. --clean\n" +
			"also syncs the shared input data (simulations/data) to the node first, additively,\n" +
			"so the run has its dependencies; --no-sync skips it when they're already staged.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return projectSubmit(node, args[0], script, account, queue_, yes, dryRun, render.IsVerbose(), keep, clean, force, noSync, hours)
		},
	}
	setHelpArgs(c, [2]string{"<case-dir>", "case directory to push and run (under $HOME, inside a git project)"})
	f := c.Flags()
	f.StringVarP(&node, "node", "N", "", "cluster to target (required)")
	f.StringVarP(&script, "script", "s", "run.sh", "job script inside the case dir")
	f.StringVarP(&account, "account", "A", "", "allocation to charge (overrides the cluster's config default)")
	f.StringVarP(&queue_, "queue", "q", "", "queue / partition to submit to")
	f.BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	f.BoolVarP(&dryRun, "dry-run", "n", false, "show the plan without pushing or submitting")
	// per-file rsync output (vs the aggregate bar) rides the global -v now; no local flag
	f.BoolVar(&keep, "keep-extra", false, "keep staging files the case dir no longer has (skip rsync --delete)")
	f.BoolVar(&clean, "clean", false, "commit-gated study mode: push via the per-node remote, stage $HOME→$WORK on the node")
	f.BoolVarP(&force, "force", "f", false, "override a case's node-lock (submit to a node other than its .mu-node marker)")
	f.BoolVar(&noSync, "no-sync", false, "skip the --clean SHARED-data sync (simulations/data already staged)")
	f.Float64Var(&hours, "hours", 0, "override the estimated core-hour cost for the allocation pre-flight (else parsed from the script)")
	completeNodeFlag(c)
	return c
}

// stageProtect keeps --delete away from what staging legitimately accumulates
// beyond the case dir: scheduler stdout/stderr drops from prior runs (debug
// evidence; same shapes prep's copy skips) and the submit-origin stamp.
var stageProtect = []string{
	"--filter=P *.o[0-9]*",
	"--filter=P *.e[0-9]*",
	"--filter=P slurm-*.out",
	"--filter=P /" + project.StampFile,
}

// projectSubmit is the push-and-run pipeline: resolve → preview+confirm → get
// the case into $WORK staging (iterate: laptop rsync; clean: git push + node-side
// copy) → stamp + submit.
func projectSubmit(node, caseDir, script, account, queue_ string, yes, dryRun, verbose, keep, clean, force, noSync bool, hours float64) error {
	caseAbs, err := filepath.Abs(caseDir)
	if err != nil {
		return usageErr("%s", err)
	}
	if fi, err := os.Stat(caseAbs); err != nil || !fi.IsDir() {
		return usageErr("%s is not a directory", caseDir)
	}
	if node == "" {
		return usageErr("needs --node <cluster> — submit runs from the authoring machine")
	}
	root, err := project.FindRoot(caseAbs)
	if err != nil {
		return usageErr("%s", err)
	}
	// Node-lock guard (nearest-ancestor .mu-node): a declared case belongs to one
	// node/system — refuse another, --force to override. Node-specific, NOT DSRC-wide:
	// each machine is its own $WORKDIR/scheduler, so a sibling node in the same DSRC is
	// still refused. guard-not-whitelist, like `mu node`. Checked ahead of the script
	// gate so a wrong-node submit is refused on declared intent, not deflected by an
	// unrelated setup error.
	lock, marker, locked, err := project.Affinity(caseAbs)
	if err != nil {
		return usageErr("%s", err)
	}
	if locked && lock != node {
		if !force {
			rel, _ := filepath.Rel(root, marker)
			return usageErr("case is locked to %s (%s) — submitting to %s is refused; --force to override or edit the marker", lock, rel, node)
		}
		render.Warn(fmt.Sprintf("overriding node-lock: %s marks this case for %s, submitting to %s", project.AffinityFile, lock, node))
	}
	if _, err := os.Stat(filepath.Join(caseAbs, script)); err != nil {
		return usageErr("script %s not found in %s", script, caseDir)
	}
	rel, err := project.HomeRel(caseAbs)
	if err != nil {
		return usageErr("%s", err)
	}
	target, err := hpc.Resolve(node)
	if err != nil {
		return usageErr("%s", err)
	}
	scheduler := config.SchedulerFor(node)
	adapter := queue.For(scheduler)
	if adapter == nil {
		return errNoScheduler(node)
	}
	if account == "" {
		account = config.AccountFor(node)
	}
	part, qos := submitTarget(node, queue_)
	opts := queue.SubmitOpts{Account: account, Queue: part, QOS: qos}
	submitCmd := adapter.SubmitCmd(script, opts)
	stamp := project.NewStamp(caseAbs)
	branch := ""
	if clean {
		// The study-phase gates: a real commit, a clean tree, a branch to push.
		if stamp.Commit == "" {
			return usageErr("--clean needs a commit — the run must be reproducible from a sha")
		}
		if stamp.Dirty {
			return usageErr("--clean refuses a dirty tree — commit (or submit iterate-mode) first")
		}
		if branch = gitLine(root, "branch", "--show-current"); branch == "" {
			return usageErr("%s is on a detached HEAD — check out a branch for --clean", root)
		}
	}

	mode := "iterate"
	if clean {
		mode = "clean"
	}
	render.Info(fmt.Sprintf("Submit case → %s (%s, %s)", node, scheduler, mode))
	render.Detail("case:    " + rel)
	render.Detail("stage:   $WORKDIR/" + rel)
	render.Detail("script:  " + script)
	if stamp.Commit != "" {
		render.Detail(fmt.Sprintf("origin:  %.12s dirty=%v", stamp.Commit, stamp.Dirty))
	} else {
		render.Detail("origin:  no commit yet — recorded dirty")
	}
	if d := adapter.Directives(opts); len(d) > 0 {
		render.Detail("applies: " + strings.Join(d, "  "))
	} else {
		render.Detail("applies: (scheduler defaults / script directives)")
	}
	render.Detail("command: " + submitCmd)
	if clean && !noSync {
		render.Detail("data:    sync simulations/data → $WORKDIR (add-only; --no-sync to skip)")
	}
	hoursPreflight(node, account, filepath.Join(caseAbs, script), hours, !dryRun)
	if dryRun {
		render.Info("dry run — nothing pushed or submitted")
		return nil
	}
	if !yes {
		if !confirm("push + submit to %s?", node) {
			render.Info("aborted")
			return nil
		}
	}
	if err := hpc.EnsureTicket(); err != nil {
		return runErr("%s", err)
	}

	// --clean data leg: ensure the run's SHARED input data (simulations/data) is on the
	// node before the job runs. Additive + add-only — a no-op when already present, and a
	// differing input is skipped+warned, never overwritten. yes=true: the submit confirm
	// above already covered it. --no-sync opts out (data staged by hand).
	if clean && !noSync {
		if err := syncShared(root, projSyncOpts{node: node, yes: true, verbose: verbose}); err != nil {
			return err
		}
	}

	// Leg 1: create staging and resolve $WORKDIR remotely (the laptop can't know it).
	qrel := shell.Quote(rel)
	out, err := hpc.RemoteExec(target, fmt.Sprintf(`mkdir -p "$WORKDIR"/%s && cd "$WORKDIR"/%s && pwd`, qrel, qrel))
	if err != nil {
		return runErr("%s: staging dir: %s", node, err)
	}
	stage := strings.TrimSpace(out)
	if stage == "" {
		return runErr("%s: staging dir: empty $WORKDIR resolution", node)
	}

	// Leg 2: get the case into staging — iterate rsyncs the working tree from
	// the laptop; clean pushes the branch through the per-node remote
	// (updateInstead refreshes the $HOME clone) and stages node-side.
	if clean {
		if err := cleanStage(node, target, root, branch, rel, stage, keep); err != nil {
			return err
		}
	} else {
		// Rides the ambient master the legs on either side already share, so staging
		// doesn't authenticate a connection of its own between them.
		o := rsync.Opts{Delete: !keep, PartialDir: true, Transport: hpc.AmbientTransport(target)}
		if !keep {
			o.Ropt = stageProtect
		}
		label := "stage " + node
		code, _ := rsync.Run(rsync.BuildArgs(caseAbs+"/", target+":"+stage+"/", o), label, verbose)
		if code != 0 {
			render.EventErr("project", fmt.Sprintf("%s FAILED (rsync exit %d)", label, code))
			return codeErr(code)
		}
	}

	// Leg 3: drop the stamp and submit from staging.
	out, err = hpc.RemoteExec(target, fmt.Sprintf("cd %s && printf '%%s' %s > %s && %s",
		shell.Quote(stage), shell.Quote(stamp.TOML()), project.StampFile, submitCmd))
	if err != nil {
		return runErr("%s: submit: %s", node, err)
	}
	if s := strings.TrimSpace(out); s != "" {
		render.Detail(s)
	}
	msg := "submitted " + rel + " → " + node
	render.OK(msg)
	render.EventOK("project", msg)
	return nil
}

// cleanStage is the --clean transport: divergence pre-flight (the remote must be
// an ancestor of HEAD — can't attribute which side diverged, doesn't need to),
// push through the per-node remote, then a node-side $HOME→$WORK copy of the
// case. The push refreshing the checked-out tree is the updateInstead contract
// `mu project clone` set up.
func cleanStage(node, target, root, branch, rel, stage string, keep bool) error {
	if gitLine(root, "remote", "get-url", node) == "" {
		return usageErr("no %s remote in %s — run `mu project clone %s` first", node, root, node)
	}
	env := append(os.Environ(), "GIT_SSH_COMMAND="+config.SSHCommand())
	if msg, ok := gitRun(root, env, "fetch", "-q", node, branch); !ok {
		return runErr("fetch from %s failed: %s", node, msg)
	}
	remote := gitLine(root, "rev-parse", "FETCH_HEAD")
	if remote != "" {
		if _, ok := gitRun(root, nil, "merge-base", "--is-ancestor", remote, "HEAD"); !ok {
			return usageErr("%s's clone is at %.12s, not an ancestor of HEAD — reconcile first", node, remote)
		}
	}
	if msg, ok := gitRun(root, env, "push", "-q", node, branch); !ok {
		return runErr("push to %s failed (dirty remote checkout?): %s", node, msg)
	}
	qfilters := ""
	if !keep {
		qs := make([]string, 0, len(stageProtect)+1)
		qs = append(qs, "--delete")
		for _, f := range stageProtect {
			qs = append(qs, shell.Quote(f))
		}
		qfilters = " " + strings.Join(qs, " ")
	}
	cmd := fmt.Sprintf(`rsync -a%s "$HOME"/%s/ %s/`, qfilters, shell.Quote(rel), shell.Quote(stage))
	if out, err := hpc.RemoteExec(target, cmd); err != nil {
		return runErr("%s: node-side stage: %s\n%s", node, err, strings.TrimSpace(out))
	}
	return nil
}
