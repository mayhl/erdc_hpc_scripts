package cli

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
)

func hpcQueueCmd() *cobra.Command {
	var node, userList string
	var allUsers, jsonOut, local, fleet, all, start, interactive bool
	c := &cobra.Command{
		Use:   "queue",
		Short: "Render a scheduler queue (PBS qstat / SLURM squeue) as a house table.",
		Long: "Show cluster jobs as one normalized house table, regardless of scheduler\n" +
			"(PBS qstat / SLURM squeue). Three WHERE scopes select how wide to look\n" +
			"(orthogonal to WHO: default you, -u alice,bob specific users, -a everyone):\n\n" +
			"    -l --local         current cluster only, run locally — no ssh (default on HPC)\n" +
			"    -f --fleet         the `fleet` node list, else active clusters (default off HPC)\n" +
			"    -e --all-systems   every distinct queue: the fleet plus one node per cluster\n" +
			"                       not already in it (incl. inactive)\n\n" +
			"Bare `mstat` resolves by location: on a login node it's --local; off HPC,\n" +
			"where there's no local scheduler, it's --fleet. --fleet/--all-systems fan out\n" +
			"concurrently with a per-node timeout, tagging each job with a System\n" +
			"column; an unreachable system degrades to a warning, never a hang:\n" +
			"    mstat                          # local on HPC, fleet off it\n" +
			"    mstat -f -a                    # the fleet, all users\n" +
			"    mstat -e -a                    # every system, all users\n\n" +
			"--node fetches one cluster over remote-exec (qstat vs squeue from its\n" +
			"configured scheduler); with neither --node nor a scope flag, a listing piped\n" +
			"on stdin is parsed (scheduler auto-detected) — the test/pipe-your-own seam:\n" +
			"    mu hpc queue --node hpc1\n" +
			"    hpc1 squeue | mu hpc queue",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			who, err := mustUserSel(userList, allUsers)
			if err != nil {
				return err
			}
			if interactive {
				if fleet || all {
					return mstatInteractiveCollate(all, who)
				}
				return mstatInteractive(node, who)
			}
			var jobs []queue.Job
			var down []string
			var label string
			var hooksCh <-chan map[string]string // launched BEFORE the snapshot, concurrent with it
			var fleetProg map[string]string      // collate paths: "label/id"-keyed, fetched per target
			switch {
			case node != "":
				label = node
				hooksCh = fetchHookProgress(node, false)
				jobs, err = fetchJobs(node, who)
			case all:
				label, jobs, fleetProg, down, err = collateJobs(allSystemsScope(), "all", who)
			case fleet:
				label, jobs, fleetProg, down, err = collateJobs(fleetScope(), "fleet", who)
			case local:
				// Explicit --local: current cluster, run locally. Off-HPC this errors
				// (no scheduler here) rather than silently widening.
				hooksCh = fetchHookProgress("", true)
				label, jobs, err = fetchJobsLocal(who)
			case !term.IsTerminal(os.Stdin.Fd()):
				var data []byte
				data, err = io.ReadAll(os.Stdin)
				if err == nil {
					jobs = queue.Parse(string(data))
				}
			default:
				// Bare mstat, no pipe → resolve by location: on a login node run the
				// current cluster locally; off HPC (no local scheduler) fall to fleet.
				if self, _ := currentCluster(); self != "" {
					hooksCh = fetchHookProgress("", true)
					label, jobs, err = fetchJobsLocal(who)
				} else {
					label, jobs, fleetProg, down, err = collateJobs(fleetScope(), "fleet", who)
				}
			}
			if err != nil {
				return err
			}
			if jsonOut {
				if err := writeJSON(jobs); err != nil {
					return err
				}
			} else {
				rows := toJobRows(jobs)
				applyHookProgress(rows, hooksCh)
				applyFleetHookProgress(rows, fleetProg)
				render.JobsTable(label, config.User(), rows, render.JobCols{Start: start})
			}
			for _, d := range down { // unreachable clusters degrade to warnings, never a hang
				render.Warn(d)
			}
			return nil
		},
	}
	addSiteScopeFlags(c, &node, &local, &fleet, &all, siteScopeHelp{
		node:  "fetch the queue from this node (else read stdin)",
		local: "current cluster only, fetched locally (default on HPC)",
		fleet: "collate the `fleet` node list, else active clusters (default off HPC)",
		all:   "collate every distinct queue: the fleet plus one node per cluster not in it, incl. inactive",
	})
	c.Flags().BoolVarP(&allUsers, "all-users", "a", false, "all users' jobs (default: yours)")
	c.Flags().StringVarP(&userList, "user", "u", "", "show these users' jobs (comma-separated), e.g. -u alice,bob")
	c.Flags().BoolVar(&start, "start", false, "add a Start column: actual start (running) or estimated start (pending); SLURM only")
	c.Flags().BoolVarP(&interactive, "interactive", "i", false, "pick jobs to cancel interactively (with -f/-e: across the collate view)")
	c.Flags().BoolVar(&jsonOut, "json", false, "emit jobs as JSON (complete, untruncated) instead of a table")
	c.MarkFlagsMutuallyExclusive("all-users", "user") // both pick WHO; -u is a subset, -a is everyone
	c.AddCommand(queueKillCmd(), queueInfoCmd(), queuePeekCmd(), queueHoldCmd(), queueReleaseCmd(), queueHistCmd())
	return c
}

// errNoScheduler is the code-2 error for an unconfigured scheduler — the shared no-op
// for the queue verbs when config carries no scheduler = "slurm"|"pbs". Callers return it.
func errNoScheduler(label string) error {
	return usageErr("no scheduler configured for %s — set `scheduler = \"slurm\"|\"pbs\"` in config.toml", label)
}

// fetchJobs runs the cluster's scheduler command on node over remote-exec and parses
// it into normalized jobs. The scheduler comes from config (not a probe); an
// unconfigured scheduler or a remote failure returns one house error line.
func fetchJobs(node string, who userSel) ([]queue.Job, error) {
	target, err := hpc.Resolve(node)
	if err != nil {
		return nil, usageErr("%s", err)
	}
	cmd, parse := fetchSpec(config.SchedulerFor(node), who)
	if cmd == "" {
		return nil, errNoScheduler(node)
	}
	if err := hpc.EnsureTicket(); err != nil {
		return nil, runErr("%s", err)
	}
	out, err := hpc.RemoteExec(target, cmd)
	if err != nil {
		return nil, runErr("%s: remote fetch failed: %v", node, err)
	}
	return parse(out), nil
}

// fetchJobsLocal runs the current cluster's scheduler command locally (no ssh) for
// the on-cluster `mstat` path — you're already on the login node, so mu just runs
// squeue/qstat here. Returns the resolved cluster label + jobs; off-HPC (no current
// cluster) it errors, steering to `<node> mstat` / --node.
func fetchJobsLocal(who userSel) (string, []queue.Job, error) {
	self, scheduler := currentCluster()
	if self == "" {
		return "", nil, usageErr("not on an HPC cluster — use `<node> mstat` or `mu hpc queue --node <n>`")
	}
	cmd, parse := fetchSpec(scheduler, who)
	if cmd == "" {
		return "", nil, errNoScheduler(self)
	}
	// Same command as the remote fetch, run through the local exec seam (bash -lc,
	// stderr filtered) — the on-cluster mirror of hpc.RemoteExec.
	out, err := hpc.LocalExec(cmd)
	if err != nil {
		return "", nil, runErr("%s: local queue fetch failed: %v", self, err)
	}
	return self, parse(out), nil
}

// userSel is the WHO axis of a fetch: whose jobs to show. Exactly one applies, in
// precedence order — an explicit list (-u), then all users (-a), else just you.
type userSel struct {
	all  bool   // -a / --all-users: no user filter
	list string // -u / --user: comma-separated user list; "" = unset
}

// validUserList guards the -u value: it is interpolated into the remote/local shell
// command, so restrict it to the characters a username list can contain (comma is the
// separator) — no spaces or shell metacharacters.
func validUserList(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ',' || r == '_' || r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

// fetchSpec returns the live-queue command + matching parser for a scheduler. The command
// is built by the scheduler Adapter (qstat -a / squeue -o …); it's paired here with the
// parser its format needs — SLURM's controlled pipe format → ParseSLURMDelim, PBS's wide
// qstat -a → ParsePBS. config.HPCUser() supplies the "just you" default. "" = unknown.
func fetchSpec(scheduler string, who userSel) (string, func(string) []queue.Job) {
	a := queue.For(scheduler)
	if a == nil {
		return "", nil
	}
	cmd := a.ListCmd(who.all, who.list, config.HPCUser())
	if scheduler == "slurm" {
		return cmd, queue.ParseSLURMDelim
	}
	return cmd, queue.ParsePBS
}

// toJobRows maps normalized jobs to render's plain JobRow (keeping render domain-
// free). An unknown state shows the raw scheduler code so nothing is silently hidden.
func toJobRows(jobs []queue.Job) []render.JobRow {
	rows := make([]render.JobRow, len(jobs))
	for i, j := range jobs {
		state := j.State.String()
		if j.State == queue.Unknown {
			state = strings.TrimSpace(j.RawState)
		}
		rows[i] = render.JobRow{
			ID: j.ShortID, Name: j.Name, User: j.User, Queue: j.Queue, Nodes: j.Nodes,
			State: state, Elapsed: j.Elapsed, ReqWall: j.ReqWall, Reason: j.PendingReason(),
			Submit: j.Submit, Start: j.Start, End: j.End, Cluster: j.Cluster,
		}
	}
	return rows
}
