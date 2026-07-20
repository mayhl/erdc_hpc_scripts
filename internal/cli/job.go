package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/job"
	"github.com/mayhl/mayhl_utils/internal/modules"
	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// jobCmd is `mu job`: helpers for HPC batch jobs that work across PBS and SLURM.
func jobCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "job",
		Short: "HPC batch-job helpers (scheduler-agnostic).",
		Long:  "Helpers for HPC batch jobs that behave the same under PBS and SLURM.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(jobEnvCmd(), jobSubCmd(), jobPrepCmd(), jobHooksCmd(), jobWatchCmd(), jobTunnelCmd(), jobShellCmd(), jobHarnessCmd(), jobCleanCmd())
	return c
}

// jobCleanCmd is `mu job clean`: reap the pushed `mu job sub` scripts on one cluster whose
// job has finished. Each `mu job sub` already does this for its own target in passing; this
// is the manual sweep for a cluster you haven't submitted to lately.
func jobCleanCmd() *cobra.Command {
	var node string
	c := &cobra.Command{
		Use:   "clean",
		Short: "Remove pushed job scripts whose job has ended.",
		Long: "`mu job sub` pushes a local script to the cluster and remembers it; this reaps\n" +
			"those staged copies once their job has left the queue (each `mu job sub` also\n" +
			"does this for its own cluster as it goes). Targets one cluster: -N <cluster>\n" +
			"off an HPC login node, else the current one.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			label, _, snapshot, _, capture, err := queueTargetCtx(node, userSel{})
			if err != nil {
				return err
			}
			if n := sweepStagedOn(node, label, snapshot, capture, true); n == 0 {
				render.Info("no staged scripts to reap on " + label)
			}
			return nil
		},
	}
	c.Flags().StringVarP(&node, "node", "N", "", "cluster to target (required off an HPC login node)")
	completeNodeFlag(c)
	return c
}

// jobPrepCmd is `mu job prep`: create this job's run dir (sibling case_a_<jobid>,
// inputs copied, run.toml provenance) and emit the shell that moves the job into it.
// stdout is CODE for eval — mu owns the failure semantics (a failed prep emits
// `exit 1`, so the job dies instead of computing in the authored case dir); all
// human-facing lines go to stderr.
func jobPrepCmd() *cobra.Command {
	var pathOnly bool
	c := &cobra.Command{
		Use:   "prep",
		Short: "Create the run dir (case copy + run.toml) — eval inside a job.",
		Long: "Give this job its own run dir: copy the submit (case) dir to a sibling named\n" +
			"<case>_<jobid> (scheduler log files excluded), drop a run.toml provenance record,\n" +
			"and print shell that exports MU_RUN_DIR and cds there — a failed prep prints\n" +
			"`exit 1` so the job aborts rather than running in the case dir. Preamble idiom:\n\n" +
			"    eval \"$(mu job env)\"\n" +
			"    eval \"$(mu job prep)\"\n" +
			"    mu job watch & trap 'kill $! 2>/dev/null' EXIT   # optional sidecar\n\n" +
			"A requeue (run dir already present) reuses it without re-copying, so partial\n" +
			"outputs survive. --path prints the run dir path and changes nothing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if pathOnly {
				dir, err := job.RunDir()
				if err != nil {
					return runErr("%s", err)
				}
				fmt.Println(dir)
				return nil
			}
			snippet, reused, err := job.Prep()
			if err != nil {
				// stdout carries the abort for the eval'ing job script; the returned
				// error covers interactive use (stderr + non-zero exit).
				fmt.Printf("echo 'mu job prep: %s' >&2\nexit 1\n", strings.ReplaceAll(err.Error(), "'", `'\''`))
				return runErr("%s", err)
			}
			if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(os.Getenv("MU_SUBMIT_DIR")+"/", home+"/") {
				render.Warn("submit dir is under $HOME — this run will write to the permanent tier (submit from the $WORKDIR copy)")
			}
			fmt.Print(snippet)
			verb := "prepared"
			if reused {
				verb = "reusing"
			}
			dir, _ := job.RunDir()
			render.OK(verb + " run dir " + dir)
			render.EventOK("job", verb+" run dir "+dir)
			return nil
		},
	}
	c.Flags().BoolVar(&pathOnly, "path", false, "print the run dir path only (no copy, no run.toml)")
	return c
}

// jobSubCmd is `mu job sub <script>`: submit a batch script to one cluster (qsub/sbatch)
// with a scheduler-neutral account/queue, after a preview + confirm. Thin by design — the
// script path is resolved ON the target; -N picks the cluster off an HPC login node, else
// the current cluster. -A overrides the cluster's config default; empty opts fall through
// to the script's own #PBS/#SBATCH directives.
func jobSubCmd() *cobra.Command {
	var node, account, walltime, name string
	var nodes int
	var hours float64
	var sel queueSel
	var yes, dryRun, interactive bool
	c := &cobra.Command{
		Use:   "sub [script]",
		Short: "Submit a batch script to one cluster (qsub/sbatch) — preview + confirm.",
		Long: "Submit a job script to one cluster, mapping -A/-q to the scheduler's flags\n" +
			"(PBS qsub / SLURM sbatch). Target: -N <cluster> off an HPC login node, else the\n" +
			"current cluster. A LOCAL script is pushed to the cluster and submitted from there;\n" +
			"a path that only exists ON the target is submitted as-is (mu says which it did).\n" +
			"-A overrides the cluster's config default; empty falls through to the script's own\n" +
			"directives.\n\n" +
			"The class flags pick the queue by node class instead of by name: --gpu/--vis/\n" +
			"--bigmem/--xfer use the cluster's config `submit_queue` entry, else resolve it\n" +
			"from the live queue list (exactly one queue of that class); --debug/--background\n" +
			"fall back to the standard queue of that name. With no -q and no class flag,\n" +
			"`submit_queue.default` applies when set.\n\n" +
			"-i opens the submit form instead: every knob as an editable field, pre-seeded\n" +
			"from the flags and config, the queue enum backed by the live queue list, and\n" +
			"walltime/nodes checked against the selected queue's limits as you type. The\n" +
			"usual preview + confirm still follows.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			script := ""
			if len(args) == 1 {
				script = args[0]
			}
			label, scheduler, snapshot, run, capture, err := queueTargetCtx(node, userSel{})
			if err != nil {
				return err
			}
			adapter := queue.For(scheduler)
			if adapter == nil {
				return errNoScheduler(label)
			}
			if account == "" {
				account = config.AccountFor(label)
			}
			var opts queue.SubmitOpts
			if interactive {
				if !render.Interactive() {
					return fmt.Errorf("mu job sub -i needs a terminal (stdin is not a tty)")
				}
				if node != "" { // the form's queue fetch is remote — get the ticket BEFORE the TUI owns the terminal
					if err := hpc.EnsureTicket(); err != nil {
						return runErr("%s", err)
					}
				}
				var ok bool
				if script, opts, ok, err = subForm(node, label, script, account, &sel, walltime, nodes, name); err != nil {
					return err
				} else if !ok {
					render.Info("aborted")
					return nil
				}
			} else {
				if script == "" {
					return usageErr("sub needs a <script> (or -i for the form)")
				}
				queueName, err := sel.resolve(node, label, true)
				if err != nil {
					return err
				}
				// A batch script declares its own walltime and a qsub -l would override it,
				// so --debug only asks for the queue's slot when the script is silent — and
				// when it isn't, mu says whether what it asks for even fits.
				debugMax := (sel.debug || sel.dbg) && mayInjectWalltime(node, script)
				if (sel.debug || sel.dbg) && !debugMax && walltime == "" {
					warnScriptWalltime(node, queueName, script)
				}
				if walltime, err = resolveWalltime(node, queueName, walltime, "", debugMax); err != nil {
					return err
				}
				q, qos := submitTarget(label, queueName)
				opts = queue.SubmitOpts{
					Account: account, Queue: q, QOS: qos,
					Walltime: walltime, Nodes: nodes, Name: name,
					CoresPerNode: queueCPN(label, queueName),
				}
			}
			// A LOCAL script is pushed to the cluster and submitted from there — so the path names
			// the file on YOUR disk (tab-completion resolves it) — while a bare remote path is
			// submitted as-is. The staged path is deterministic from the id, so `command:` below
			// is honest before the file is written; the write itself waits until after confirm.
			push := isLocalScript(script)
			stageID, remoteScript := "", script
			if push {
				stageID = newTunnelID()
				remoteScript = stagedPath(stageID)
			}
			cmd := adapter.SubmitCmd(remoteScript, opts)

			render.Info(fmt.Sprintf("Submit to %s (%s)", label, scheduler))
			if push {
				render.Verbose(fmt.Sprintf("script:  %s → %s (push)", script, remoteScript))
			} else {
				render.Verbose("script:  " + remoteScript)
			}
			if d := adapter.Directives(opts); len(d) > 0 {
				render.Verbose("applies: " + strings.Join(d, "  "))
			} else {
				render.Verbose("applies: (scheduler defaults / script directives)")
			}
			render.Detail("command: " + cmd)
			hoursPreflight(node, account, script, hours, !dryRun)
			if dryRun {
				render.Info("dry run — not submitted")
				return nil
			}
			if !yes {
				if !confirm("submit to %s?", label) {
					render.Info("aborted")
					return nil
				}
			}
			// A pushed script has to outlive its job (the scheduler may reread it at run time),
			// so mu records it and a later sweep reaps it once the job ends — which needs the
			// job id, so the push path submits via capture and parses it, rather than the
			// fire-and-forget run. Staging and submit chain into ONE remote call (`stage && sbatch`)
			// so a push-sub opens a single connection, not one to write and one to submit. A bare
			// (non-pushed) submit leaves nothing behind, so it stays on run.
			if push {
				stageCmd, serr := stageScriptCmd(script, stageID)
				if serr != nil {
					return runErr("%s", serr)
				}
				out, cerr := capture(stageCmd + " && " + cmd)
				if cerr != nil {
					return runErr("%s: submit failed: %v", label, cerr)
				}
				render.OK("pushed " + script + " → " + remoteScript)
				if s := strings.TrimSpace(out); s != "" {
					render.Detail(s)
				}
				if jobID := queue.ParseSubmitID(scheduler, out); jobID != "" {
					if err := saveStaged(stagedRec{ID: stageID, Node: node, System: label, Job: jobID, Started: time.Now()}); err != nil {
						render.Warn(fmt.Sprintf("couldn't record the staged script for cleanup: %v", err))
					}
				} else {
					render.Warn("couldn't read the job id from submit output — staged script " + remoteScript + " won't be auto-reaped; remove it by hand if unneeded")
				}
			} else if err := run(cmd); err != nil {
				return err
			}
			msg := "submitted " + script + " → " + label
			render.OK(msg)
			render.EventOK("job", msg)
			// Opportunistic cleanup: reap earlier pushes to THIS cluster whose job has ended,
			// reusing the connection we already hold. Silent and best-effort — never fail a
			// submit over a cleanup miss.
			sweepStagedOn(node, label, snapshot, capture, false)
			return nil
		},
	}
	setHelpArgs(c, [2]string{"[script]", "job script: a local path is pushed, a remote path submitted as-is (optional with -i)"})
	c.Flags().StringVarP(&node, "node", "N", "", "cluster to target (required off an HPC login node)")
	c.Flags().StringVarP(&account, "account", "A", "", "allocation to charge (overrides the cluster's config default)")
	addQueueSelFlags(c, &sel)
	c.Flags().StringVarP(&walltime, "walltime", "t", "", "walltime limit: HH:MM:SS or a duration (10m, 1h, 1.5h, 1h30m)")
	c.Flags().IntVarP(&nodes, "nodes", "n", 0, "node count (PBS select chunk / SLURM -N)")
	c.Flags().Float64Var(&hours, "hours", 0, "override the estimated core-hour cost for the allocation pre-flight (else parsed from the script)")
	c.Flags().StringVarP(&name, "name", "J", "", "job name")
	c.Flags().BoolVarP(&interactive, "interactive", "i", false, "edit the submission in a form (fields pre-seeded from flags + config, live queue list)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print the submit command without submitting")
	completeNodeFlag(c)
	return c
}

// queueSel is the queue selector shared by the submitting job verbs (sub/tunnel/shell):
// -q names a queue literally, or one class/purpose flag resolves it via the cluster's
// submit_queue config, falling back per key — see resolveSubmitQueue.
type queueSel struct {
	queue                                                       string
	gpu, vis, bigmem, himem, xfer, debug, dbg, background, back bool
}

// addQueueSelFlags registers -q plus the class/purpose selector flags on c, all
// mutually exclusive (-q stays literal-always, so a queue named "gpu" is never shadowed).
func addQueueSelFlags(c *cobra.Command, s *queueSel) {
	f := c.Flags()
	f.StringVarP(&s.queue, "queue", "q", "", "queue / partition to submit to")
	f.BoolVar(&s.gpu, "gpu", false, "submit to the GPU queue (config submit_queue.gpu, else resolved live)")
	f.BoolVar(&s.vis, "vis", false, "submit to the visualization queue (config submit_queue.vis, else resolved live)")
	f.BoolVar(&s.bigmem, "bigmem", false, "submit to the big-memory queue (config submit_queue.bigmem, else resolved live)")
	f.BoolVar(&s.himem, "himem", false, "alias for --bigmem")
	f.BoolVar(&s.xfer, "xfer", false, "submit to the transfer/archive queue (config submit_queue.xfer, else resolved live)")
	f.BoolVar(&s.debug, "debug", false, "submit to the debug queue — quick iterations (config submit_queue.debug, else the queue named 'debug')")
	f.BoolVar(&s.dbg, "dbg", false, "alias for --debug")
	f.BoolVar(&s.background, "background", false, "submit to the no-charge background queue (config submit_queue.background, else the queue named 'background')")
	f.BoolVar(&s.back, "back", false, "alias for --background")
	_ = f.MarkHidden("himem")
	_ = f.MarkHidden("dbg")
	_ = f.MarkHidden("back")
	c.MarkFlagsMutuallyExclusive("queue", "gpu", "vis", "bigmem", "himem", "xfer", "debug", "dbg", "background", "back")
}

// key maps the selected flag to its submit_queue config key, or "" when none is set.
func (s *queueSel) key() string {
	switch {
	case s.gpu:
		return "gpu"
	case s.vis:
		return "vis"
	case s.bigmem || s.himem:
		return "bigmem"
	case s.xfer:
		return "xfer"
	case s.debug || s.dbg:
		return "debug"
	case s.background || s.back:
		return "background"
	}
	return ""
}

// resolve returns the queue to target: -q verbatim, else the submit_queue resolution
// for the selected flag. bareDefault opts a flagless call into submit_queue.default —
// true for `sub` (batch default belongs there), false for tunnel/shell (interactive
// verbs stay on the scheduler default unless a flag says otherwise).
func (s *queueSel) resolve(node, label string, bareDefault bool) (string, error) {
	// A bad queue_flag misroutes even an explicit -q (-p vs --qos), so validate it first.
	if err := checkQueueFlag(label); err != nil {
		return "", err
	}
	if s.queue != "" {
		return s.queue, nil
	}
	key := s.key()
	if key == "" && !bareDefault {
		// Interactive verbs opt out of the default to ride the scheduler's — but a qos-site has
		// none, so an empty queue there is fatal rather than a fallback.
		if queueRequired(label) {
			return "", errNoDefaultQueue(label)
		}
		return "", nil
	}
	q, err := resolveSubmitQueue(node, label, key)
	if err != nil {
		return "", err
	}
	// submit_queue.default unset on a qos-site → empty → the scheduler would reject the job with
	// a raw "Invalid qos specification"; name the config fix here instead.
	if q == "" && queueRequired(label) {
		return "", errNoDefaultQueue(label)
	}
	return q, nil
}

// submitClasses maps a class-flag key to the node class the live fallback filters by.
// debug/background are absent: they name a PURPOSE tier, not a node class, so there is no
// class to filter on — they start from the conventional queue name (submitLiterals) and
// check it against the machine, see resolveLiteralQueue.
var (
	submitClasses  = map[string]string{"gpu": "GPU", "vis": "VIS", "bigmem": "BigMem", "xfer": "Xfer"}
	submitLiterals = map[string]string{"debug": "debug", "background": "background"}
)

// resolveSubmitQueue picks the submit queue for a selector-flag key (gpu/vis/bigmem/
// xfer/debug/background), or for the bare default (key ""). Order: the cluster's config
// submit_queue entry; then the standard literal name for the purpose keys; else the
// cluster's queue list filtered by class — exactly one up submittable match resolves, zero
// or several error with a submit_queue hint. node routes the fetch (""=local cluster).
func resolveSubmitQueue(node, label, key string) (string, error) {
	if key == "" {
		return config.SubmitQueueFor(label, "default"), nil
	}
	if q := config.SubmitQueueFor(label, key); q != "" {
		return q, nil
	}
	_, qs, err := cachedQueues(node)
	if err != nil {
		return "", err
	}
	if lit, ok := submitLiterals[key]; ok {
		return resolveLiteralQueue(label, key, lit, qs)
	}
	class := submitClasses[key]
	names := classQueues(label, class, qs)
	switch len(names) {
	case 1:
		render.Info(fmt.Sprintf("--%s → queue %s (from %s's queue list; pin it: submit_queue = { %s = %q })", key, names[0], label, key, names[0]))
		return names[0], nil
	case 0:
		return "", runErr("no up %s queue on %s — set submit_queue = { %s = \"<queue>\" } in config.toml, or use -q", class, label, key)
	default:
		return "", runErr("%d %s queues on %s (%s) — pick one with -q, or set submit_queue = { %s = \"<queue>\" }", len(names), class, label, strings.Join(names, ", "), key)
	}
}

// queueCPN is the cores-per-node the PBS select chunk uses for -n: the per-queue
// config override when the queue is known, else the cluster default; 0 → the adapter
// emits a bare select count.
func queueCPN(label, queueName string) int {
	if queueName != "" {
		if cpn := config.QueueCoresOverride(label, queueName); cpn > 0 {
			return cpn
		}
	}
	return config.CoresPerNodeFor(label)
}

// resolveLiteralQueue turns a purpose key (debug/background) into a queue the machine
// actually HAS. The conventional name is a convention, not a guarantee — a SLURM site may
// have no partition called "debug" at all, and `salloc -p debug` then dies with "invalid
// partition specified", which tells you nothing about what to use instead.
//
// So: take the conventional name when the machine really offers it; else the single up queue
// whose name carries the word ("cpu_debug", "debug_gpu"); else refuse, listing what the
// machine does offer. With no cached listing there is nothing to check against — send the
// literal and let the scheduler answer, as before.
func resolveLiteralQueue(label, key, literal string, qs []queue.QueueInfo) (string, error) {
	up, _ := upQueues(execQueues(qs))
	if len(up) == 0 {
		return literal, nil
	}
	var names, match []string
	for _, q := range up {
		names = append(names, q.Name)
		if q.Name == literal {
			return literal, nil
		}
		if strings.Contains(strings.ToLower(q.Name), key) {
			match = append(match, q.Name)
		}
	}
	switch len(match) {
	case 1:
		render.Info(fmt.Sprintf("--%s → queue %s (%s has no queue named %q; pin it: submit_queue = { %s = %q })",
			key, match[0], label, literal, key, match[0]))
		return match[0], nil
	case 0:
		return "", runErr("%s has no %s queue (its queues: %s) — pick one with -q, or set submit_queue = { %s = \"<queue>\" } in config.toml",
			label, key, strings.Join(names, ", "), key)
	default:
		return "", runErr("%d queues on %s look like %s (%s) — pick one with -q, or set submit_queue = { %s = \"<queue>\" }",
			len(match), label, key, strings.Join(match, ", "), key)
	}
}

// classQueues is the pure core of the live class-flag fallback: the up, submittable
// queues on label whose node class matches — the config queue_class override first,
// the name heuristic else. Input order preserved.
func classQueues(label, class string, qs []queue.QueueInfo) []string {
	up, _ := upQueues(execQueues(qs))
	var names []string
	for _, q := range up {
		if queueClass(label, q.Name) == class {
			names = append(names, q.Name)
		}
	}
	return names
}

// jobWatchCmd is `mu job watch`: the sidecar invocation mode of the model hooks —
// the preamble backgrounds it inside the allocation and it appends a progress-hook
// snapshot to <rundir>/.mu/progress every tick. Every switched-off / hook-less path
// exits 0 silently: the preamble line must never fail a job.
func jobWatchCmd() *cobra.Command {
	var interval time.Duration
	c := &cobra.Command{
		Use:   "watch",
		Short: "Tick the progress hook into .mu/progress — background inside a job.",
		Long: "Run the model's progress hook every tick and append one JSON line per run to\n" +
			".mu/progress in the run dir — progress history, stall markers (pct unchanged\n" +
			"for " + strconv.Itoa(job.StallTicks) + " ticks → a `stall` event line, `resumed` on movement), and the\n" +
			"tick stream future notifications consume. Preamble idiom:\n\n" +
			"    eval \"$(mu job env)\"\n" +
			"    eval \"$(mu job prep)\"\n" +
			"    mu job watch & trap 'kill $! 2>/dev/null' EXIT\n\n" +
			"No progress hook, or [project] job_hooks off → silent no-op, never a job\n" +
			"failure. Interval: --interval, else [project] watch_interval, else 60s.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !modules.Enabled("project") || !config.JobHooks() {
				return nil
			}
			if interval <= 0 {
				interval = config.WatchInterval()
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
			defer stop()
			if _, err := job.Watch(ctx, interval); err != nil {
				return runErr("%s", err)
			}
			return nil
		},
	}
	c.Flags().DurationVar(&interval, "interval", 0, "tick interval (default: [project] watch_interval, else 60s)")
	return c
}

// jobEnvCmd is `mu job env`: the runtime shim — normalize the active scheduler's in-job
// vars to MU_* for `eval` at the top of a job script.
func jobEnvCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "env",
		Short: "Emit normalized MU_* job vars for `eval` inside a job script.",
		Long: "Print `export MU_*` lines that normalize the active scheduler's in-job\n" +
			"variables (PBS or SLURM) to a common MU_* set, so one job script runs under\n" +
			"either. Source it at the top of a job script:\n\n" +
			"    eval \"$(mu job env)\"\n\n" +
			"then use $MU_JOBID, $MU_SUBMIT_DIR, $MU_NUM_NODES, $MU_NODEFILE, …. On SLURM,\n" +
			"$MU_NODEFILE is a hostname-per-line file expanded from the compressed nodelist,\n" +
			"so it matches PBS's $PBS_NODEFILE.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			out, err := job.Env()
			if err != nil {
				return runErr("%s", err)
			}
			fmt.Print(out)
			return nil
		},
	}
}
