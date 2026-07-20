package cli

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// jobTunnelCmd is `mu job tunnel`: the compute-node tunnel flow in one verb —
// submit a service script (or adopt a running job with --job), poll the
// scheduler until it runs and its node is known, then hold an ssh -L tunnel
// through the login node until Ctrl-C (compute nodes aren't reachable from the
// workstation; the login node relays). -I instead allocates an interactive
// shell (qsub -I / salloc) under a real tty.
func jobTunnelCmd() *cobra.Command {
	var node, jobID, account, walltime, name string
	var sel queueSel
	var port, localPort int
	var yes, interactive, foreground bool
	var wait, poll time.Duration
	c := &cobra.Command{
		Use:   "tunnel [script]",
		Short: "Submit a job and tunnel a port to its compute node.",
		Long: "The service-tunnel flow: submit <script> (something that serves a port —\n" +
			"jupyter, a dashboard) on the target cluster, wait until the scheduler reports\n" +
			"it running and names its node, then open localhost:<-l> → <node>:<-p> through\n" +
			"the login node and hold it until Ctrl-C — the job keeps running; reattach\n" +
			"with --job <id>, which also adopts any already-submitted job instead of\n" +
			"submitting. One held connection carries the whole flow. For an interactive\n" +
			"shell on a compute node see `mu job shell`. Front-door: `mtunnel`.\n\n" +
			"-i opens the tunnel form instead: script/job, queue, account, the ports and\n" +
			"the run mode (background/foreground) as editable fields, pre-seeded from the\n" +
			"flags and config, with the queue enum backed by the cluster's queue list. The\n" +
			"usual preview + confirm still follows.\n\n" +
			"    mu job tunnel ~/serve.sh -N hpc1 -p 8888\n" +
			"    mu job tunnel --job 4501 -N hpc1 -p 8888 -l 9999",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			script := ""
			if len(args) == 1 {
				script = args[0]
			}
			if interactive {
				if !render.Interactive() {
					return usageErr("mu job tunnel -i needs a terminal (stdin is not a tty)")
				}
				if node == "" {
					return usageErr("needs -N <cluster> — the tunnel runs from the workstation")
				}
				// The form's queue fetch is remote — get the ticket BEFORE the TUI owns the terminal.
				if err := hpc.EnsureTicket(); err != nil {
					return runErr("%s", err)
				}
				if account == "" {
					account = config.AccountFor(node)
				}
				f, ok, err := tunnelForm(node, node, script, jobID, account, walltime, &sel, port, localPort, foreground)
				if err != nil {
					return err
				}
				if !ok {
					render.Info("aborted")
					return nil
				}
				// The form validated the exclusivity and the port; its queue is now literal.
				script, jobID, account, walltime = f.Script, f.JobID, f.Account, f.Walltime
				port, localPort = f.Port, f.LocalPort
				foreground = f.Foreground
				sel = queueSel{queue: f.Queue}
				return jobTunnel(node, script, jobID, account, walltime, &sel, port, localPort, name, foreground, yes, wait, poll)
			}
			if script == "" && jobID == "" {
				return usageErr("tunnel needs a <script> to submit or --job <id> (or -i for the form)")
			}
			if script != "" && jobID != "" {
				return usageErr("<script> and --job are exclusive — submit or adopt, not both")
			}
			if port == 0 {
				return usageErr("needs -p <port> — the service port on the compute node")
			}
			// An unnamed -l stays 0 so pickLocalPort can start at the service port and walk up
			// when it's taken. Defaulting it here would forge a port the user never named, and
			// a NAMED port is refused rather than moved — so the busy case died at the refusal.
			return jobTunnel(node, script, jobID, account, walltime, &sel, port, localPort, name, foreground, yes, wait, poll)
		},
	}
	setHelpArgs(c, [2]string{"[script]", "service script: a local path is pushed, a remote path submitted as-is"})
	f := c.Flags()
	f.StringVarP(&node, "node", "N", "", "cluster to target (required off an HPC login node)")
	f.StringVar(&jobID, "job", "", "adopt this already-submitted job instead of submitting")
	f.IntVarP(&port, "port", "p", 0, "service port on the compute node")
	f.IntVarP(&localPort, "local", "l", 0, "local port to listen on (default: --port, or the next free port above it)")
	f.StringVarP(&account, "account", "A", "", "allocation to charge (overrides the cluster's config default)")
	addQueueSelFlags(c, &sel)
	f.StringVarP(&walltime, "walltime", "t", "", "how long to hold the job: HH:MM:SS or a duration (10m, 1h, 1.5h); default: config interactive_walltime")
	f.StringVarP(&name, "name", "J", "", "job name (default: an opaque mu-<id>)")
	f.BoolVar(&foreground, "fg", false, "hold the tunnel in the foreground (default: background — mu exits once it's up)")
	f.BoolVarP(&interactive, "interactive", "i", false, "edit the tunnel in a form (fields pre-seeded from flags + config, live queue list)")
	f.BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	f.DurationVar(&wait, "wait", 15*time.Minute, "give up if the job isn't running by then")
	f.DurationVar(&poll, "poll", 5*time.Second, "scheduler poll interval while waiting")
	completeNodeFlag(c)
	c.AddCommand(tunnelLsCmd(), tunnelReattachCmd(), tunnelCloseCmd())
	return c
}

// shellAlloc holds the flags shared by `mu job shell` and `mu job harness open`: both request
// the same interactive allocation, and only the wrapping (bare vs inside tmux) differs.
type shellAlloc struct {
	node, account, walltime string
	nodes                   int
	sel                     queueSel
	interactive             bool
}

func addShellAllocFlags(c *cobra.Command, o *shellAlloc) {
	c.Flags().StringVarP(&o.node, "node", "N", "", "cluster to target (required off an HPC login node)")
	c.Flags().StringVarP(&o.account, "account", "A", "", "allocation to charge (overrides the cluster's config default)")
	addQueueSelFlags(c, &o.sel)
	c.Flags().StringVarP(&o.walltime, "walltime", "t", "", "how long to hold the session: HH:MM:SS or a duration (10m, 1h, 1.5h); default: config interactive_walltime")
	c.Flags().IntVarP(&o.nodes, "nodes", "n", 1, "nodes to allocate (PBS select chunk / SLURM -N)")
	c.Flags().BoolVarP(&o.interactive, "interactive", "i", false, "pick the queue, account and walltime in a form (queue enum from the cluster's queue list)")
	completeNodeFlag(c)
}

// runShellAlloc runs the -i form (when asked) then hands off to the interactive allocation. dir,
// when set (harness open --dir), submits the allocation from that directory so the pane lands
// there; `mu job shell` passes "".
func runShellAlloc(o *shellAlloc, dir string) error {
	if o.interactive {
		if !render.Interactive() {
			return usageErr("needs a terminal (stdin is not a tty)")
		}
		label, _, _, _, _, err := queueTargetCtx(o.node, userSel{})
		if err != nil {
			return err
		}
		if o.node != "" { // the form's queue fetch is remote — ticket BEFORE the TUI takes the terminal
			if err := hpc.EnsureTicket(); err != nil {
				return runErr("%s", err)
			}
		}
		if o.account == "" {
			o.account = config.AccountFor(label)
		}
		q, acct, wall, n, ok, err := shellForm(o.node, label, o.account, o.walltime, o.nodes, &o.sel)
		if err != nil {
			return err
		}
		if !ok {
			render.Info("aborted")
			return nil
		}
		o.account, o.walltime, o.nodes = acct, wall, n
		o.sel = queueSel{queue: q} // the form's pick is literal — don't re-resolve a class flag
	}
	return jobInteractive(o.node, o.account, o.walltime, dir, o.nodes, &o.sel)
}

// jobShellCmd is `mu job shell`: an interactive allocation on a compute node —
// the scheduler's own qsub -I / salloc under a real tty (RemoteExec is tty-less
// by design, so this path builds its own `ssh -t`). The tunnel's sibling: shell
// = you on the node, tunnel = a service's port. FUTURE: -p adds a tunnel to the
// allocated node once the scheduler names it (the mux makes that composable).
func jobShellCmd() *cobra.Command {
	var o shellAlloc
	c := &cobra.Command{
		Use:   "shell",
		Short: "Open an interactive allocation on a compute node (qsub -I / salloc).",
		Long: "Request an interactive allocation through the target's scheduler and hand you\n" +
			"the shell on the compute node, tty and all. Exiting the shell releases the\n" +
			"allocation. For tunnelling a service's port instead, see `mu job tunnel`; to run\n" +
			"the allocation inside a tmux you can drive from a script, see `mu job harness open`.\n" +
			"Front-door: `mshell`.\n\n" +
			"-i picks the queue and account in a form, its queue enum backed by the\n" +
			"cluster's queue list — the way to see what you can actually allocate on.\n\n" +
			"    mu job shell -N hpc1 --debug",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runShellAlloc(&o, "")
		},
	}
	addShellAllocFlags(c, &o)
	return c
}

// jobTunnel is the script-mode pipeline over ONE held connection: open an ssh
// ControlMaster (the single auth), submit and wait as channels on it, then add
// the port-forward to the same live connection and hold until Ctrl-C. The
// login node sees one session for the whole flow.
func jobTunnel(node, script, jobID, account, walltime string, sel *queueSel, port, localPort int, name string, foreground, yes bool, wait, poll time.Duration) error {
	if node == "" {
		return usageErr("needs -N <cluster> — the tunnel runs from the workstation")
	}
	// Bind the local end BEFORE anything remote: a doomed port shouldn't cost a submit. When
	// -l was given it must be that port or an error; otherwise mu picks the first free one at
	// or above the remote port, so the URL stays predictable.
	localPort, err := pickLocalPort(localPort, port)
	if err != nil {
		return err
	}
	scheduler := config.SchedulerFor(node)
	adapter := queue.For(scheduler)
	if adapter == nil {
		return errNoScheduler(node)
	}
	startedAt := time.Now()
	target, err := hpc.Resolve(node)
	if err != nil {
		return usageErr("%s", err)
	}
	if account == "" {
		account = config.AccountFor(node)
	}
	queue_, wall := "", ""
	if jobID == "" { // adopt mode never submits — don't resolve (or live-fetch) a queue for it
		// The bare default counts: a site whose queue rides --qos has no usable scheduler
		// default, so submitting without one is refused outright (Invalid qos specification).
		if queue_, err = sel.resolve(node, node, true); err != nil {
			return err
		}
		// A tunnel is a HELD session: it lives exactly as long as its job, so the config
		// default applies — unless the script speaks for itself.
		dflt := ""
		if mayInjectWalltime(node, script) {
			if dflt, err = interactiveWalltime(node); err != nil {
				return err
			}
		}
		debugMax := (sel.debug || sel.dbg) && mayInjectWalltime(node, script)
		if wall, err = resolveWalltime(node, queue_, walltime, dflt, debugMax); err != nil {
			return err
		}
	}
	part, qos := submitTarget(node, queue_)
	id := newTunnelID()
	// The job wears mu-<id> — no port, no "tunnel" — so a cluster-wide qstat leaks nothing.
	// -J still overrides for someone who wants a name of their own; the id remains the handle.
	if name == "" {
		name = jobName(id)
	}
	// Hand the job its port so the service and the forward agree by construction — the trap
	// otherwise is a script that hardcodes one number while -p names another.
	opts := queue.SubmitOpts{
		Account: account, Queue: part, QOS: qos, Walltime: wall, Name: name,
		Env: map[string]string{"MU_PORT": strconv.Itoa(port)},
	}
	// A LOCAL script is pushed to the cluster and submitted from there — so `~/serve.sh` names
	// the file on YOUR disk, the way tab-completion already resolved it — while a bare remote
	// path is submitted as-is. The staged path is deterministic from the id, so the submit
	// command below is honest before the file is actually written (which needs the mux).
	push := jobID == "" && isLocalScript(script)
	remoteScript := script
	if push {
		remoteScript = stagedPath(id)
	}
	submitCmd := adapter.SubmitCmd(remoteScript, opts)

	render.Info(fmt.Sprintf("Tunnel job → %s (%s)", node, scheduler))
	if jobID == "" {
		if push {
			render.Verbose(fmt.Sprintf("script:  %s → %s (push)", script, remoteScript))
		} else {
			render.Verbose("script:  " + remoteScript)
		}
		render.Detail("command: " + submitCmd)
	} else {
		render.Detail("job:     " + jobID)
	}
	render.Verbose(fmt.Sprintf("tunnel:  localhost:%d → <node>:%d, one held connection", localPort, port))
	if !foreground {
		render.Verbose("mode:    background — mu exits once it's up; close with `mu job tunnel close`")
	}
	if !yes {
		if !confirm("connect + tunnel on %s?", node) {
			render.Info("aborted")
			return nil
		}
	}
	if err := hpc.EnsureTicket(); err != nil {
		return runErr("%s", err)
	}

	mux, err := hpc.OpenSession(target, hpc.SessionOpts{Persist: !foreground, ID: tunnelSockID(node, localPort)})
	if err != nil {
		return runErr("%s: connect: %s", node, err)
	}
	// A backgrounded master must SURVIVE mu's exit — that's the point. But it must NOT survive a
	// FAILED setup: an error after the master is up (bad submit, never-runs, forward refused)
	// would otherwise orphan the master + its control socket, and the next run trips over the
	// stale socket. So tear it down on every exit EXCEPT the successful background return, which
	// arms keepMux just before it returns. Foreground always closes here.
	keepMux := false
	defer func() {
		if !keepMux {
			mux.Close()
		}
	}()
	// Ctrl-C anywhere in the flow: closing the mux collapses whatever leg is
	// in flight (its channel dies with the master) — flows unwind naturally.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	aborted := make(chan struct{})
	go func() {
		<-sig
		close(aborted)
		mux.Close()
	}()

	if push {
		// Write the local script to its staged path over the master we already hold — before
		// submit, so a failed push aborts cleanly (keepMux stays false → the master is torn down).
		if _, err := writeStaged(mux.Run, script, id); err != nil {
			return tunnelErr(aborted, "%s: %s", node, err)
		}
		render.OK("pushed " + script + " → " + remoteScript)
	}

	if jobID == "" {
		out, err := mux.Run(submitCmd)
		if err != nil {
			return tunnelErr(aborted, "%s: submit: %s", node, err)
		}
		if jobID = queue.ParseSubmitID(scheduler, out); jobID == "" {
			return runErr("%s: submit returned no job id:\n%s", node, strings.TrimSpace(out))
		}
		render.OK("submitted " + jobID)
	}

	host, err := waitRunning(mux.Run, adapter, scheduler, jobID, wait, poll)
	if err != nil {
		return tunnelErr(aborted, "%s", err)
	}
	runningAt := time.Now()
	render.OK(fmt.Sprintf("job %s running on %s", jobID, host))

	if err := mux.Forward(localPort, host, port); err != nil {
		return runErr("adding the forward: %s", err)
	}

	rec := tunnelRec{
		ID: id, System: node, Job: jobID, Host: host, Target: target, Sock: mux.Sock(),
		LocalPort: localPort, RemotePort: port, Walltime: wall, Script: remoteScript, Staged: push,
		Started: startedAt, Running: runningAt,
	}
	if err := saveTunnel(rec); err != nil {
		// The tunnel is up; we just can't track it. Say so rather than tear down working work.
		render.Warn(fmt.Sprintf("tunnel is up but not recorded (%v) — `close` won't find it; Ctrl-C or `mdel %s`", err, jobID))
	}

	if !foreground {
		keepMux = true // setup succeeded — the master now outlives mu; `close` tears it down later
		render.OK(fmt.Sprintf("tunnel up: %s → %s:%d (background; close with `mu job tunnel close %s`)", rec.URL(), host, port, id))
		return nil
	}
	render.OK(fmt.Sprintf("tunnel up: %s → %s:%d (Ctrl-C to close)", rec.URL(), host, port))
	defer forgetTunnel(rec) // foreground: the tunnel dies with this process, so its record should too
	select {
	case <-aborted:
		render.Info("tunnel closed")
		return nil
	case err := <-mux.Death():
		return runErr("connection to %s dropped: %v — resume with --job %s", node, err, jobID)
	}
}

// tunnelErr maps an error after a Ctrl-C to a clean abort (the mux teardown
// makes the in-flight leg fail — that's the teardown working, not a failure).
func tunnelErr(aborted <-chan struct{}, format string, a ...any) error {
	select {
	case <-aborted:
		render.Info("aborted")
		return nil
	default:
		return runErr(format, a...)
	}
}

// waitRunning blocks until the job reports running AND names its node — a job
// can sit in R for a beat before exec_host/BatchHost appears. The poll loop
// runs ON the login node inside ONE ssh session (laptop-side polling would be
// a fresh connection + Kerberos handshake per tick — connection churn a login
// node logs; the local qstat it runs instead is the cheap, intended kind of
// polling). Ctrl-C drops the ssh; the remote loop dies with its terminal.
func waitRunning(capture func(string) (string, error), a queue.Adapter, scheduler, jobID string, wait, poll time.Duration) (string, error) {
	render.Info(fmt.Sprintf("waiting for %s to run (one login-node session, up to %s; Ctrl-C to give up) …", jobID, wait))
	// running / finished markers in the raw detail, per scheduler — matched
	// remotely so the loop needs no mu on the far side; the final detail is
	// parsed HERE by the normal parser.
	runRe, doneRe := `job_state = R`, `job_state = [CEF]`
	hostRe := `exec_host`
	if scheduler == "slurm" {
		runRe, doneRe = `JobState=RUNNING`, `JobState=\(COMPLETED\|FAILED\|CANCELLED\|TIMEOUT\)`
		hostRe = `BatchHost=`
	}
	detail := a.DetailCmd([]string{jobID})
	script := fmt.Sprintf(
		`_d=$((SECONDS+%d)); while [ $SECONDS -lt $_d ]; do `+
			`_o=$(%s 2>/dev/null); `+
			`if printf '%%s' "$_o" | grep -q '%s' && printf '%%s' "$_o" | grep -q '%s'; then printf '%%s' "$_o"; exit 0; fi; `+
			`if printf '%%s' "$_o" | grep -q '%s'; then printf '%%s' "$_o"; exit 0; fi; `+
			`sleep %d; done; echo MU_WAIT_TIMEOUT`,
		int(wait.Seconds()), detail, runRe, hostRe, doneRe, max(1, int(poll.Seconds())),
	)
	out, err := capture(script)
	if err != nil {
		return "", runErr("wait for %s: %s", jobID, err)
	}
	if strings.Contains(out, "MU_WAIT_TIMEOUT") {
		return "", runErr("job %s not running after %s — check the queue (`minfo %s`), then retry with --job %s", jobID, wait, jobID, jobID)
	}
	for _, d := range queue.ParseDetails(scheduler, out) {
		if d.State == "running" && d.ExecHost != "" {
			return d.ExecHost, nil
		}
		if d.State == "complete" || d.State == "exiting" {
			return "", runErr("job %s already finished — nothing to tunnel to", jobID)
		}
	}
	return "", runErr("job %s: could not read a running state from the scheduler detail", jobID)
}

// jobInteractive replaces mu with `ssh -t <login> <qsub -I|salloc>` — the
// scheduler's own interactive allocation under a real tty (RemoteExec is
// tty-less by design, so this path builds its own ssh).
func jobInteractive(node, account, walltime, dir string, nodes int, sel *queueSel) error {
	label, scheduler, _, _, _, err := queueTargetCtx(node, userSel{})
	if err != nil {
		return err
	}
	adapter := queue.For(scheduler)
	if adapter == nil {
		return errNoScheduler(label)
	}
	target, err := hpc.Resolve(label)
	if err != nil {
		return usageErr("%s", err)
	}
	if err := hpc.EnsureTicket(); err != nil {
		return runErr("%s", err)
	}
	if account == "" {
		account = config.AccountFor(label)
	}
	queue_, err := sel.resolve(node, label, true)
	if err != nil {
		return err
	}
	// No script here at all — the session IS the job, so the config default always applies.
	dflt, err := interactiveWalltime(label)
	if err != nil {
		return err
	}
	wall, err := resolveWalltime(node, queue_, walltime, dflt, sel.debug || sel.dbg)
	if err != nil {
		return err
	}
	part, qos := submitTarget(label, queue_)
	// An interactive session has no script to declare its resources, so mu must — and a PBS
	// site rejects `qsub -I` outright without a select chunk ("Please include number of nodes
	// with -l select"). One node unless asked otherwise; ncpus/mpiprocs come from the
	// machine's cores_per_node, because the same sites that demand the chunk demand it FULL.
	if nodes < 1 {
		nodes = 1
	}
	// Name it mu-<id>, same as a tunnel — a bland handle in the queue rather than the
	// scheduler's anonymous default (or a leaked "interactive"). No registry entry: an
	// interactive shell dies with its terminal, so there's nothing to track or close.
	// --dir (harness open): the adapter injects the cd where its scheduler allows — SLURM as
	// salloc's command so the compute shell lands in <dir>; PBS as a submit-dir prefix (qsub -I
	// takes no command, so it can only set PBS_O_WORKDIR — the shell still opens in $HOME).
	icmd := adapter.InteractiveCmd(queue.SubmitOpts{
		Account: account, Queue: part, QOS: qos, Walltime: wall, Name: jobName(newTunnelID()),
		Nodes: nodes, CoresPerNode: queueCPN(label, queue_),
	}, dir)
	render.Info(fmt.Sprintf("interactive allocation on %s: %s", label, icmd))
	ssh := config.SSHCommand()
	// `bash -lc`, exactly as RemoteExec does it: ssh runs the command in the user's LOGIN
	// SHELL WITHOUT LOGIN SEMANTICS, so /etc/profile.d never runs and the scheduler isn't on
	// PATH — a PBS site answers `command not found: qsub`. (SLURM happened to work only
	// because salloc was already on the default PATH there.) The login shell must be bash,
	// not the user's zsh: the site's profile scripts are written for it.
	//
	// -q silences the client's pre-auth banner (the consent notice); the MOTD and the profile
	// noise come down the pty instead, and allocView drops those.
	cmd := exec.Command(ssh, "-q", "-t", target, "bash -lc "+shell.Quote(icmd))
	view := newAllocView(os.Stdout)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, view, os.Stderr
	// ssh -t puts THIS terminal in raw mode, where a bare \n doesn't return the cursor to
	// column 0 — without this every house line below staircases to the right.
	render.SetCRLF(true)
	defer render.SetCRLF(false)
	err = cmd.Run()
	view.flush()
	if err != nil && !strings.Contains(err.Error(), "signal:") {
		return runErr("interactive session: %s", err)
	}
	return nil
}

// preflightAlloc re-runs jobInteractive's cheap, network-free gate (target + scheduler
// resolution) in the OUTER process, before `mu job harness open` wraps the allocation in tmux.
// Without it a missing/misconfigured scheduler fails INSIDE the pane, which tmux closes the
// instant the inner exits — so the error flashes and vanishes and the owner sees a silent no-op.
// Failing here surfaces it on the owner's own terminal. jobInteractive still validates (it also
// serves the un-wrapped `mu job shell`), so this is a deliberate echo, not the only check.
func preflightAlloc(o *shellAlloc) error {
	label, scheduler, _, _, _, err := queueTargetCtx(o.node, userSel{})
	if err != nil {
		return err
	}
	if queue.For(scheduler) == nil {
		return errNoScheduler(label)
	}
	// Resolve the queue here too, with the REAL -q/flags, so a bad queue_flag or a qos-site with
	// no default fails on the owner's own terminal — not inside the pane, which tmux closes the
	// instant the inner exits (the error would flash and vanish). The -i form picks its queue
	// interactively in the pane, so only validate the flag itself for it.
	if o.interactive {
		if err := checkQueueFlag(label); err != nil {
			return err
		}
	} else if _, err := o.sel.resolve(o.node, label, true); err != nil {
		return err
	}
	if _, err := hpc.Resolve(label); err != nil {
		return usageErr("%s", err)
	}
	return nil
}

// preflightLogin is the login-harness counterpart: loginInteractive's node + target gate, run in
// the outer process so a bad --node fails loud instead of dying in the pane that closes on exit.
func preflightLogin(node string) error {
	if node == "" {
		return usageErr("needs -N <cluster> — the login harness runs from the workstation")
	}
	if _, err := hpc.Resolve(node); err != nil {
		return usageErr("%s", err)
	}
	return nil
}

// harnessSocket is the fixed `-L` socket the owner's terminal and any driver share, so a second
// tmux client sees the same server the allocation runs in.
const harnessSocket = "mu-harness"

// launchHarness re-execs THIS `mu job harness {open,login}` invocation inside a tmux session on
// the shared socket, so a separate tmux client can drive the pane via `mu job harness run` while
// the owner stays attached and authenticates. mu owns only the setup — no ssh or pkinit runs on
// the driver side. The inner run carries MU_HARNESS_INNER, which the open/login verb reads to skip
// this wrap and proceed to the real session. session is the tmux name (mu-shell-<id> for compute,
// mu-login-<id> for a login node); dir, when given, pins the drive anchor now (before the pane
// exists to pwd).
func launchHarness(dir, session string) error {
	// new-session attaches THIS terminal (you authenticate in the pane), so it needs a real
	// tty — a pipe or a non-interactive prompt makes tmux fail with a cryptic "not a terminal".
	if !render.Interactive() {
		return usageErr("this harness needs a real terminal (you authenticate in the pane) — run it in your shell, not through a pipe or a non-interactive prompt")
	}
	tmuxBin, err := harnessTmux()
	if err != nil {
		return err
	}
	// Don't clobber a live session of the same name — send the owner to it instead. The full
	// session name is a valid drive id (resolveHarnessSession accepts it verbatim), so the hints
	// stay correct for both the compute and login kinds without special-casing.
	if harnessHasSession(tmuxBin, session) {
		return usageErr("harness session %q is already open — `mu job harness attach %s`", session, session)
	}
	self, err := os.Executable()
	if err != nil {
		return runErr("cannot locate the mu binary: %s", err)
	}
	// The inner command is this invocation verbatim, marked so it runs the allocation rather than
	// re-wrapping. tmux runs it through `sh -c`, so quote the binary and every argument.
	inner := "MU_HARNESS_INNER=1 exec " + shell.Quote(self)
	for _, a := range os.Args[1:] {
		inner += " " + shell.Quote(a)
	}
	if dir != "" {
		if err := writeHarnessAnchor(session, dir); err != nil {
			return runErr("cannot save anchor: %s", err)
		}
	}
	render.Info(fmt.Sprintf("harness: opening tmux session %q — drive with `mu job harness run %s <cmd>`, watch with `mu job harness attach %s`", session, session, session))
	tc := exec.Command(tmuxBin, "-L", harnessSocket, "new-session", "-s", session, inner)
	tc.Stdin, tc.Stdout, tc.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Drop TMUX so this attaches cleanly even when launched from inside another tmux: it runs on
	// its own socket, but tmux still refuses a nested ATTACH while $TMUX is set.
	tc.Env = envWithout(os.Environ(), "TMUX", "TMUX_PANE")
	return tc.Run()
}

// sessionNode sanitizes a target into a tmux-safe suffix (tmux forbids ':' and '.').
func sessionNode(node string) string {
	n := node
	if n == "" {
		n = "hpc"
	}
	return strings.NewReplacer(":", "-", ".", "-").Replace(n)
}

// harnessSession derives the COMPUTE harness session name for a target.
func harnessSession(node string) string { return "mu-shell-" + sessionNode(node) }

// harnessLoginSession derives the LOGIN-node harness session name — distinct from the compute
// name (mu-shell-<id>) so a compile-on-login and a run-on-compute harness coexist per cluster.
func harnessLoginSession(node string) string { return "mu-login-" + sessionNode(node) }

// loginInteractive opens an interactive shell on the cluster's LOGIN node over `ssh -t` — no
// scheduler, so the pane keeps the login node's internet egress (compiling, fetching a
// dependency) that a compute node lacks. The compute sibling is jobInteractive; this one drops
// the qsub -I / salloc and hands you the login shell directly. Runs inside the harness pane
// (MU_HARNESS_INNER), so you authenticate here, same as the compute path.
func loginInteractive(node, dir string) error {
	if node == "" {
		return usageErr("needs -N <cluster> — the login harness runs from the workstation")
	}
	target, err := hpc.Resolve(node)
	if err != nil {
		return usageErr("%s", err)
	}
	if err := hpc.EnsureTicket(); err != nil {
		return runErr("%s", err)
	}
	render.Info(fmt.Sprintf("login-node shell on %s (internet egress; no allocation)", target))
	ssh := config.SSHCommand()
	// No remote command: `ssh -t` runs the user's login shell interactively, so /etc/profile
	// (modules, PATH, any network proxy) is sourced — everything a compile needs. -q drops the
	// pre-auth banner; allocView cleans the MOTD/profile noise the pty carries.
	sshArgs := []string{"-q", "-t", target}
	if dir != "" {
		// --dir: land the pane IN <dir> (so attach and interactive work start there, not just
		// driven `run`), then exec the login shell so the profile is still sourced. A failed cd
		// falls back to $HOME rather than killing the pane. exec so no wrapper shell lingers.
		sshArgs = append(sshArgs, "cd "+shell.Quote(dir)+" 2>/dev/null || echo 'harness: --dir path not found on the login node — staying in $HOME' >&2; exec ${SHELL:-bash} -l")
	}
	cmd := exec.Command(ssh, sshArgs...)
	// Pass the pty straight through — NOT through allocView. allocView holds all output until
	// the scheduler announces itself (salloc:/qsub:), the HOLD→PASS trigger; a login shell has
	// no scheduler, so that tag never comes and it would swallow the prompt and echo, hanging
	// the pane. The login node's MOTD shows once, which is fine for an interactive shell.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// ssh -t puts THIS terminal in raw mode, where a bare \n doesn't return to column 0.
	render.SetCRLF(true)
	defer render.SetCRLF(false)
	if err := cmd.Run(); err != nil && !strings.Contains(err.Error(), "signal:") {
		return runErr("login session: %s", err)
	}
	return nil
}

// envWithout returns env with every VAR=... entry whose name is in drop removed.
func envWithout(env []string, drop ...string) []string {
	var out []string
	for _, e := range env {
		keep := true
		for _, d := range drop {
			if strings.HasPrefix(e, d+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return out
}
