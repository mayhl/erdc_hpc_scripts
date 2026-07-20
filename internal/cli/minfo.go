package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// queueInfoCmd is `mu hpc queue info` (front-door `minfo`): show full job detail as the
// house card — normalized from `qstat -f` (PBS) / `scontrol show job` (SLURM). Pretty by
// default (mu = one house format); --raw prints the scheduler's own output verbatim and
// --json emits the normalized detail (the data contract), the standard three-mode set.
// Single-cluster like mdel; selectors resolve short ids against your queue (-u/-a widen).
// No idiom variant — there's no clean squeue word for it, so `minfo` whatever queue_aliases.
func queueInfoCmd() *cobra.Command {
	var node, userList string
	var allUsers, pattern, raw, jsonOut bool
	c := &cobra.Command{
		Use:   "info <selector>...",
		Short: "Show job detail as a house card (qstat -f / scontrol show job).",
		Long: "Resolve selectors against one cluster's queue and show each match's full\n" +
			"detail as the house card, normalized across schedulers. --raw prints the\n" +
			"scheduler's own `qstat -f` / `scontrol show job` verbatim; --json emits the\n" +
			"normalized detail (data contract). Single-cluster: --node <cluster> off an HPC\n" +
			"login node, else the current one. Scoped to your jobs (-u/-a widen). A selector\n" +
			"is a job id (short or full), a range (4501-4510), a list (4501,4507), or a name\n" +
			"mask; -p forces a mask. Front-door: `minfo`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			who, err := mustUserSel(userList, allUsers)
			if err != nil {
				return err
			}
			label, scheduler, snapshot, _, capture, err := queueTargetCtx(node, who)
			if err != nil {
				return err
			}
			matched, err := resolveJobs(label, snapshot, args, pattern)
			if err != nil {
				return err
			}
			if len(matched) == 0 {
				return nil
			}
			cmd := detailCmd(scheduler, jobIDs(matched))
			if cmd == "" {
				return errNoScheduler(label)
			}
			var modelCh <-chan map[string][][2]string
			if !raw && !jsonOut {
				modelCh = fetchHookModel(capture, jobIDs(matched)) // concurrent with the detail fetch
			}
			out, err := capture(cmd)
			if err != nil {
				return runErr("%s: info fetch failed: %v", label, err)
			}
			if raw {
				fmt.Print(out)
				return nil
			}
			details := queue.ParseDetails(scheduler, out)
			if jsonOut {
				return writeJSON(details)
			}
			model := awaitHookModel(modelCh)
			for _, d := range details {
				v := toDetailView(d)
				v.Model = model[d.ShortID]
				render.JobDetailCard(v)
			}
			return nil
		},
	}
	setHelpArgs(c, [2]string{"<selector>", argJobSelectorDesc})
	c.Flags().BoolVar(&raw, "raw", false, "print the scheduler's own detail output verbatim")
	c.Flags().BoolVar(&jsonOut, "json", false, "emit the normalized detail as JSON")
	addQueueScopeFlags(c, &node, &userList, &allUsers, &pattern)
	return c
}

// toDetailView maps a normalized queue.JobDetail to render's plain card view (keeping
// render domain-free, like toJobRows).
func toDetailView(d queue.JobDetail) render.JobDetailView {
	return render.JobDetailView{
		ID: d.ID, Name: d.Name, User: d.User, Account: d.Account, Queue: d.Queue,
		State: d.State, RawState: d.RawState, Nodes: d.Nodes, Tasks: d.Tasks,
		Elapsed: d.Elapsed, ReqWall: d.ReqWall, Submit: d.Submit, Start: d.Start, End: d.End,
		WorkDir: d.WorkDir, StdOut: d.StdOut, StdErr: d.StdErr, ExitStatus: d.ExitStatus,
		Reason: d.Reason,
	}
}

// queuePeekCmd is `mu hpc queue peek` (front-door `mpeek`): tail a job's output file.
// It reads the job's Output_Path/StdOut from the scheduler detail, then tails that file
// — not `qpeek`. Live for SLURM (StdOut is written to the shared FS as the job runs);
// for a running PBS job whose output still sits in node-local spool, the file may not
// be readable until it completes (a hint is shown on failure). One job at a time.
func queuePeekCmd() *cobra.Command {
	var node, userList string
	var allUsers, stderr bool
	var lines int
	c := &cobra.Command{
		Use:   "peek <selector>",
		Short: "Tail a job's stdout (or -e stderr) output file.",
		Long: "Resolve a selector to one job, read its output-file path from the scheduler\n" +
			"detail (Output_Path / StdOut), and tail it — the last -n lines of stdout, or\n" +
			"stderr with -e. Not `qpeek`: it reads the real file, so for a running PBS job\n" +
			"whose output is still in node-local spool it may only work once the job ends.\n" +
			"Single-cluster (--node off HPC). Front-door: `mpeek`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			who, err := mustUserSel(userList, allUsers)
			if err != nil {
				return err
			}
			label, scheduler, snapshot, _, capture, err := queueTargetCtx(node, who)
			if err != nil {
				return err
			}
			matched, err := resolveJobs(label, snapshot, args, false)
			if err != nil {
				return err
			}
			if len(matched) == 0 {
				return nil
			}
			job := matched[0]
			if len(matched) > 1 {
				render.Info(fmt.Sprintf("%d jobs matched — peeking %s (%s); narrow the selector for another", len(matched), job.ShortID, job.Name))
			}
			detail, err := capture(detailCmd(scheduler, []string{job.ID}))
			if err != nil {
				return runErr("%s: detail fetch failed: %v", label, err)
			}
			stream := "stdout"
			if stderr {
				stream = "stderr"
			}
			path := queue.OutputPath(scheduler, detail, stderr)
			if path == "" {
				return runErr("no %s path reported for %s — the job may not have started yet", stream, job.ShortID)
			}
			out, err := capture(fmt.Sprintf("tail -n %d %s", lines, shell.Quote(path)))
			if err != nil {
				return runErr("%s: cannot read %s (%v) — a running PBS job's output may still be in node-local spool", label, path, err)
			}
			render.Info(fmt.Sprintf("%s  %s  %s (last %d)", job.ShortID, stream, path, lines))
			fmt.Print(out)
			return nil
		},
	}
	setHelpArgs(c, [2]string{"<selector>", "one job — " + argJobSelectorDesc + "; multiple matches peek the first"})
	c.Flags().BoolVarP(&stderr, "stderr", "e", false, "tail the job's stderr file instead of stdout")
	c.Flags().IntVarP(&lines, "lines", "n", 40, "number of trailing lines to show")
	addQueueScopeFlags(c, &node, &userList, &allUsers, nil)
	return c
}

// argJobSelectorDesc is the shared Arguments-panel line for the queue verbs' job
// selectors (kill/hold/release/info/peek) — one wording, matching their Long texts.
const argJobSelectorDesc = "job id (short or full), range (4501-4510), list (4501,4507), or name mask (-p forces a mask)"

// addQueueScopeFlags registers the WHERE/WHO/selector flags the queue verbs share:
// --node target, -a/-u user scope, and (when patternFlag is non-nil) -p force-mask.
func addQueueScopeFlags(c *cobra.Command, node, userList *string, allUsers, patternFlag *bool) {
	c.Flags().StringVarP(node, "node", "N", "", "cluster to target (required off an HPC login node)")
	c.Flags().BoolVarP(allUsers, "all-users", "a", false, "widen to all users' jobs (default: yours)")
	c.Flags().StringVarP(userList, "user", "u", "", "widen to these users' jobs (comma-separated)")
	if patternFlag != nil {
		c.Flags().BoolVarP(patternFlag, "pattern", "p", false, "force every argument to be a name mask")
	}
	c.MarkFlagsMutuallyExclusive("all-users", "user")
	completeNodeFlag(c)
}

// mustUserSel builds the WHO axis from -u/-a, returning a code-2 error on a malformed
// user list (it's interpolated into the fetch command). Shared by the queue verbs.
func mustUserSel(userList string, allUsers bool) (userSel, error) {
	if userList != "" && !validUserList(userList) {
		return userSel{}, usageErr("--user takes a comma-separated user list (letters/digits/._-), e.g. -u alice,bob")
	}
	return userSel{all: allUsers, list: userList}, nil
}

// resolveJobs snapshots the target queue and resolves selector args to matched jobs
// (short-id → full, ranges, lists, name masks). A fetch failure is a code-1 error; an
// empty match prints a house notice and returns an empty slice + nil (a clean no-op the
// caller returns on), so callers get a non-empty set, a clean stop, or an error.
func resolveJobs(label string, snapshot func() ([]queue.Job, error), args []string, pattern bool) ([]queue.Job, error) {
	jobs, err := snapshot()
	if err != nil {
		return nil, runErr("%s: queue fetch failed: %v", label, err)
	}
	matched := queue.MatchAll(jobs, args, pattern)
	if len(matched) == 0 {
		render.Info("no matching jobs on " + label)
		return nil, nil
	}
	return matched, nil
}

// detailCmd builds the scheduler's full-detail command for the given full ids. PBS
// `qstat -f` takes space-separated ids; SLURM `scontrol show job` takes a comma list.
// Ids are single-quoted so PBS array brackets ("1284[7].hpc1") don't glob-expand. ""
// for an unknown scheduler.
func detailCmd(scheduler string, ids []string) string {
	if a := queue.For(scheduler); a != nil {
		return a.DetailCmd(ids)
	}
	return ""
}
