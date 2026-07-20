package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// queueKillCmd is `mu hpc queue kill` (front-door `mdel`): cancel your jobs on ONE
// cluster (qdel/scancel). Single-cluster by design — a blind mask never fans across
// clusters; cross-cluster cancels go through `mstat -i`, where you see and pick jobs.
func queueKillCmd() *cobra.Command {
	var node, userList string
	var allUsers, pattern, yes bool
	c := &cobra.Command{
		Use:   "kill <selector>...",
		Short: "Cancel your jobs on one cluster (qdel/scancel) — preview + confirm.",
		Long: "Resolve selectors against one cluster's queue and cancel the matches after\n" +
			"confirmation. Single-cluster by design: --node <cluster> off an HPC login\n" +
			"node, else the current cluster. Scoped to your jobs (-u/-a widen). A selector\n" +
			"is a job id (short or full), a range (4501-4510), a list (4501,4507), or a\n" +
			"name mask; -p forces a mask, ~ forces one token. Front-door: `mdel`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			who, err := mustUserSel(userList, allUsers)
			if err != nil {
				return err
			}
			label, scheduler, snapshot, run, _, err := queueTargetCtx(node, who)
			if err != nil {
				return err
			}
			jobs, err := snapshot()
			if err != nil {
				return runErr("%s: queue fetch failed: %v", label, err)
			}
			matched := queue.MatchAll(jobs, args, pattern)
			if len(matched) == 0 {
				render.Info("no matching jobs on " + label)
				return nil
			}
			return cancelJobs(label, scheduler, matched, run, yes)
		},
	}
	setHelpArgs(c, [2]string{"<selector>", argJobSelectorDesc})
	addQueueScopeFlags(c, &node, &userList, &allUsers, &pattern)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	return c
}

// mstatInteractive is `mstat -i`: pick jobs from one cluster's live queue, then hand
// them to the SAME cancel path as headless `mdel`. Single-cluster (like mdel);
// cross-cluster interactive picking is a later step. Off-HPC the fetch is a remote
// ssh round-trip, so the live refresh runs on a slow cadence.
func mstatInteractive(node string, who userSel) error {
	if !render.Interactive() {
		return fmt.Errorf("mstat -i needs a terminal (stdin is not a tty)")
	}
	label, scheduler, snapshot, run, capture, err := queueTargetCtx(node, who)
	if err != nil {
		return err
	}
	interval := 2 * time.Second
	if node != "" { // remote fetch: ssh + Kerberos per tick → don't hammer it
		interval = 15 * time.Second
	}
	ids, err := render.Select(render.SelectSpec{
		Verb:     "cancel",
		Columns:  []string{"ID", "USER", "QUEUE", "ST", "ELAP/WALL", "NAME"},
		Interval: interval,
		Fetch: func() []render.SelectRow {
			jobs, _ := snapshot() // tolerate a blip; the picker keeps its last frame
			return jobSelectRows(jobs)
		},
		Detail: func(id string) string { return jobDetailCard(scheduler, capture, id) },
	})
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		render.Info("nothing selected")
		return nil
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	jobs, err := snapshot()
	if err != nil {
		return runErr("%s: queue fetch failed: %v", label, err)
	}
	var matched []queue.Job
	for _, j := range jobs {
		if want[j.ID] {
			matched = append(matched, j)
		}
	}
	if len(matched) == 0 {
		render.Info("selected jobs are no longer queued")
		return nil
	}
	return cancelJobs(label, scheduler, matched, run, false)
}

// cancelJobs is the shared actuator for mdel and mstat -i: preview the set, confirm
// (unless yes), run the batched scheduler cancel, and event-log it.
func cancelJobs(label, scheduler string, matched []queue.Job, run func(string) error, yes bool) error {
	render.JobsTable("Cancel on "+label, config.User(), toJobRows(matched), render.JobCols{})
	if !yes {
		if !confirm("cancel %d job(s) on %s?", len(matched), label) {
			render.Info("aborted")
			return nil
		}
	}
	cmd := cancelCmd(scheduler, jobIDs(matched))
	if cmd == "" {
		return errNoScheduler(label)
	}
	if err := run(cmd); err != nil {
		return err
	}
	msg := fmt.Sprintf("cancelled %d job(s) on %s", len(matched), label)
	render.OK(msg)
	render.EventOK("queue", msg)
	return nil
}

// queueTargetCtx resolves the single target cluster: its label, scheduler, a
// snapshot() that fetches its current jobs (returns an error rather than exiting, so
// the live picker tolerates a blip), a run() that executes a mutating command there
// with stderr surfaced (cancel/hold/release), and a capture() that returns a read
// command's raw stdout (info/peek/hist) — over remote-exec for --node, or a local
// shell on an HPC login node. It exits only when there's no target at all: off-HPC
// without --node.
func queueTargetCtx(node string, who userSel) (label, scheduler string, snapshot func() ([]queue.Job, error), run func(string) error, capture func(string) (string, error), err error) {
	if node != "" {
		var target string
		target, err = hpc.Resolve(node)
		if err != nil {
			err = usageErr("%s", err)
			return
		}
		label, scheduler = node, config.SchedulerFor(node)
		cmd, parse := fetchSpec(scheduler, who)
		snapshot = func() ([]queue.Job, error) {
			if cmd == "" {
				return nil, fmt.Errorf("no scheduler configured for %s", node)
			}
			if err := hpc.EnsureTicket(); err != nil {
				return nil, err
			}
			out, err := hpc.RemoteExec(target, cmd)
			if err != nil {
				return nil, err
			}
			return parse(out), nil
		}
		run = func(c string) error {
			if err := hpc.EnsureTicket(); err != nil {
				return err
			}
			out, err := hpc.RemoteExec(target, c)
			if err != nil {
				return fmt.Errorf("%s: command failed: %w", node, err)
			}
			if s := strings.TrimSpace(out); s != "" {
				render.Detail(s)
			}
			return nil
		}
		// capture runs an arbitrary read command over remote-exec and returns its raw
		// stdout — the read verbs (minfo/mpeek/mhist) print/parse it themselves.
		capture = func(c string) (string, error) {
			if err := hpc.EnsureTicket(); err != nil {
				return "", err
			}
			return hpc.RemoteExec(target, c)
		}
		return
	}
	self, sched := currentCluster()
	if self == "" {
		err = usageErr("needs --node <cluster> off an HPC login node")
		return
	}
	label, scheduler = self, sched
	cmd, parse := fetchSpec(scheduler, who)
	snapshot = func() ([]queue.Job, error) {
		if cmd == "" {
			return nil, fmt.Errorf("no scheduler configured for %s", self)
		}
		out, err := hpc.LocalExec(cmd)
		if err != nil {
			return nil, err
		}
		return parse(out), nil
	}
	run = func(c string) error {
		out, err := hpc.LocalExec(c)
		if err != nil {
			return fmt.Errorf("%s: command failed: %w", self, err)
		}
		if s := strings.TrimSpace(out); s != "" {
			render.Detail(s)
		}
		return nil
	}
	capture = func(c string) (string, error) {
		return hpc.LocalExec(c)
	}
	return
}

// jobSelectRows adapts jobs into generic picker rows: the row ID is the FULL native
// id (what cancel needs) while the visible ID cell shows the short id (what mstat
// shows). Columns match `mstat`'s Elap / Wall pairing; NAME is last so it absorbs the
// leftover width and long job names show in full. Hues follow the house palette — id
// cyan, user magenta, queue bright-blue, name white; state/elap-wall default.
func jobSelectRows(jobs []queue.Job) []render.SelectRow {
	rows := make([]render.SelectRow, len(jobs))
	for i, j := range jobs {
		state := j.State.String()
		if j.State == queue.Unknown {
			state = strings.TrimSpace(j.RawState)
		}
		rows[i] = render.SelectRow{
			ID:    j.ID,
			Cells: []string{j.ShortID, j.User, j.Queue, state, elapWall(j.Elapsed, j.ReqWall), j.Name},
			Hues:  []string{render.HueID, render.HueUser, render.HueGroup, "", "", render.HueName},
		}
	}
	return rows
}

// jobDetailCard fetches one job's full detail (qstat -f / scontrol show job) via the
// target's capture func and renders it as the house card string — the `i` inspect
// overlay in `mstat -i`, the same card `minfo` prints. Errors return a one-line notice
// so the picker shows something rather than a blank overlay.
func jobDetailCard(scheduler string, capture func(string) (string, error), id string) string {
	cmd := detailCmd(scheduler, []string{id})
	if cmd == "" {
		return "no scheduler configured — cannot fetch detail"
	}
	out, err := capture(cmd)
	if err != nil {
		return "detail fetch failed: " + err.Error()
	}
	details := queue.ParseDetails(scheduler, out)
	if len(details) == 0 {
		return "no detail reported for " + id
	}
	return render.RenderJobDetailCard(toDetailView(details[0]))
}

// elapWall pairs elapsed with requested walltime as "elap / wall" (mirroring the
// mstat table), or just elapsed when the scheduler didn't report a walltime.
func elapWall(elapsed, wall string) string {
	if strings.TrimSpace(wall) == "" {
		return elapsed
	}
	return elapsed + " / " + wall
}

// cancelCmd builds the scheduler's batched cancel command for the given full job ids
// — one qdel/scancel with every id, not one call per job (cheap over Kerberos'd ssh).
// Ids are single-quoted so PBS array brackets ("1284[7].hpc1") don't glob-expand.
func cancelCmd(scheduler string, ids []string) string {
	if a := queue.For(scheduler); a != nil {
		return a.KillCmd(ids)
	}
	return ""
}

func jobIDs(jobs []queue.Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}
