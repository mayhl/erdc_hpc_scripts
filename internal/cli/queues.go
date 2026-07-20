package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// showQueuesCmd is the site command mu runs to enumerate a cluster's batch queues
// (limits + live counts). It emits the same wide format on PBS and SLURM, so one parser
// covers both; it is broken/absent on some systems, which the fetch degrades gracefully.
const showQueuesCmd = "show_queues"

// hpcQueuesCmd is `mu hpc queues`: list a cluster's batch queues (type, limits, live
// counts, up/stopped/disabled state) as a house table. Sibling of `mu hpc queue` (which
// lists jobs). One `show_queues` parser covers PBS and SLURM; target like queue: --node
// fetches one cluster over remote-exec, --local runs it on the current cluster, else
// stdin is parsed.
func hpcQueuesCmd() *cobra.Command {
	var node string
	var local, jsonOut, all, interactive, fleet, allSystems bool
	c := &cobra.Command{
		Use:   "queues",
		Short: "Show a cluster's batch queues (show_queues) as a house table.",
		Long: "List a cluster's batch queues — walltime/job/core limits, live run/pend counts,\n" +
			"and each queue's up / stopped / disabled state — as one house table. `show_queues`\n" +
			"emits the same wide format on PBS and SLURM, so one parser covers both.\n\n" +
			"By default only submittable (Exe) queues that are up are shown, with a compact\n" +
			"column set. Since they're all Exe and all up, the Type and State columns are\n" +
			"dropped as noise; -a/--all brings them back and adds the routing/admin and\n" +
			"down queues too. A submittable queue that's stopped/disabled is warned about.\n\n" +
			"Target, like `mu hpc queue`: --node fetches one cluster over remote-exec, --local\n" +
			"runs it on the current cluster (no ssh), -f/--fleet collates the fleet and\n" +
			"-e/--all-systems every configured cluster (adding a System column), and with none\n" +
			"of those a listing piped on stdin is parsed — the test/pipe-your-own seam:\n" +
			"    mu hpc queues --node hpc1\n" +
			"    mu hpc queues -f\n" +
			"    hpc1 show_queues | mu hpc queues",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if interactive && !render.Interactive() {
				return fmt.Errorf("mu hpc queues -i needs a terminal (stdin is not a tty)")
			}
			var (
				label string
				qs    []queue.QueueInfo
				err   error
			)
			switch {
			case fleet, allSystems:
				targets, scope := fleetScope(), "fleet"
				if allSystems {
					targets, scope = allSystemsScope(), "all"
				}
				var down []string
				label, qs, down, err = collateQueues(targets, scope)
				for _, d := range down {
					render.Warn(d)
				}
			case node != "":
				label, qs, err = fetchQueues(node)
			case local:
				label, qs, err = fetchQueuesLocal()
			case !term.IsTerminal(os.Stdin.Fd()):
				var data []byte
				data, err = io.ReadAll(os.Stdin)
				if err == nil {
					label, qs = "queues", queue.ParseShowQueues(string(data))
					if len(qs) == 0 {
						render.Warn("no queues parsed — is this `show_queues` output?")
						return nil
					}
				}
			default:
				// Bare `mu hpc queues`, no pipe → run locally on a login node;
				// fetchQueuesLocal returns a usage error off HPC, steering to --node or a pipe.
				label, qs, err = fetchQueuesLocal()
			}
			if err != nil {
				return err
			}
			raw := len(qs)
			if !all {
				// Default view: submittable (Exe) queues, then keep only the up ones —
				// the up filter is applied LAST (after Exe and any other filters). After
				// this they should all be up, so QueuesTable drops the Type/State columns;
				// a nonzero down count means a submittable queue is stopped/disabled, worth
				// a heads-up.
				qs = execQueues(qs)
				up, down := upQueues(qs)
				if down > 0 && len(up) > 0 {
					render.Warn(fmt.Sprintf("%d submittable queue(s) not up (stopped/disabled) — -a to show", down))
				}
				qs = up
			}
			if jsonOut {
				return writeJSON(qs)
			}
			if len(qs) == 0 {
				// raw>0 means everything was filtered out (routing/admin or all down); a
				// broken fetch (raw==0) already warned, so stay quiet there.
				if raw > 0 && !all {
					render.Warn(fmt.Sprintf("no up submittable queues on %s — -a to show all %d", label, raw))
				}
				return nil
			}
			if interactive {
				return queuesInteractive(label, qs, all)
			}
			render.QueuesTable(label, toQueueRows(label, qs), all) // Type/State columns only under -a
			return nil
		},
	}
	c.Flags().StringVarP(&node, "node", "N", "", "fetch queues from this node (else read stdin)")
	c.Flags().BoolVarP(&local, "local", "l", false, "run show_queues on the current cluster, locally (no ssh)")
	c.Flags().BoolVarP(&fleet, "fleet", "f", false, "collate the fleet's queues (adds a System column)")
	c.Flags().BoolVarP(&allSystems, "all-systems", "e", false, "collate every configured cluster, incl. inactive")
	c.Flags().BoolVarP(&all, "all", "a", false, "include routing/admin (non-Exe) queues (default: only submittable Exe queues)")
	c.Flags().BoolVarP(&interactive, "interactive", "i", false, "browse queues in a live-filterable picker (type to narrow, `i` to inspect)")
	c.Flags().BoolVar(&jsonOut, "json", false, "emit queues as JSON (complete, untruncated) instead of a table")
	c.MarkFlagsMutuallyExclusive("node", "local", "fleet", "all-systems")
	c.MarkFlagsMutuallyExclusive("json", "interactive")
	completeNodeFlag(c)
	return c
}

// queueLabel is the cluster a queue fetch is about: the named node, else the cluster we
// are standing on. It's also the queue cache's key, so it is resolved BEFORE any fetch.
func queueLabel(node string) (string, error) {
	if node != "" {
		return node, nil
	}
	self, _ := currentCluster()
	if self == "" {
		return "", usageErr("not on an HPC cluster — use `mu hpc queues --node <n>` or pipe `show_queues`")
	}
	return self, nil
}

// showQueuesAt runs show_queues on node ("" = the current cluster, no ssh — bash -lc so
// the site profile puts it on PATH) and parses the listing (same format on PBS and SLURM;
// it's a site wrapper). QUIET: it renders nothing, so it is safe under a TUI. Two failure
// shapes, which the callers tell apart: an unknown node or a dead ticket is an exitErr and
// stops the command; a show_queues that failed (it is broken/absent on some systems) is a
// plain error, which the table views degrade to a warning.
func showQueuesAt(node string) (string, []queue.QueueInfo, error) {
	label, err := queueLabel(node)
	if err != nil {
		return "", nil, err
	}
	var out string
	if node == "" {
		out, err = hpc.LocalExec(showQueuesCmd)
	} else {
		target, rerr := hpc.Resolve(node)
		if rerr != nil {
			return "", nil, usageErr("%s", rerr)
		}
		if terr := hpc.EnsureTicket(); terr != nil {
			return "", nil, runErr("%s", terr)
		}
		out, err = hpc.RemoteExec(target, showQueuesCmd)
	}
	if err != nil {
		return label, nil, err
	}
	return label, queue.ParseShowQueues(out), nil
}

// fetchQueues is the table views' live fetch: show_queues over remote-exec, warning instead
// of dying when it's broken, and refreshing the submit path's cache on the way past — which
// makes `mu hpc queues -N <n>` the way to force a queue-list refresh. Mirrors fetchJobs.
func fetchQueues(node string) (string, []queue.QueueInfo, error) {
	return warnBroken(showQueuesAt(node))
}

// fetchQueuesLocal is fetchQueues for the cluster we're standing on (no ssh).
func fetchQueuesLocal() (string, []queue.QueueInfo, error) {
	return warnBroken(showQueuesAt(""))
}

// warnBroken degrades a failed show_queues to a warning and an empty list — a table would
// rather print nothing than crash — and caches whatever a live fetch did return. A hard
// exitErr (unknown node, no ticket) passes straight through and stops the command.
func warnBroken(label string, qs []queue.QueueInfo, err error) (string, []queue.QueueInfo, error) {
	var hard *exitErr
	if err != nil {
		if errors.As(err, &hard) {
			return label, nil, err
		}
		render.Warn(fmt.Sprintf("%s: show_queues failed (broken or unsupported on this system?): %v", label, err))
		return label, nil, nil
	}
	writeQueueCache(label, qs)
	return label, qs, nil
}

// collateQueues is the -f/--fleet / -e/--all-systems queues view: the shared site-command
// fan-out with each row's System tagged by cluster label (show_queues, unlike show_storage,
// emits no System column of its own).
func collateQueues(targets []queueTarget, scope string) (string, []queue.QueueInfo, []string, error) {
	return collateSite(targets, scope, showQueuesCmd, queue.ParseShowQueues,
		func(q *queue.QueueInfo, label string) { q.System = label })
}

// execQueues keeps only submittable (Exe) queues — part of the default `mu hpc queues`
// view. Non-Exe queues are routing/admin queues a user never targets with `mu job sub`.
// Matches the `Exe` Type code case-insensitively by prefix, tolerating a longer spelling
// on some systems.
func execQueues(qs []queue.QueueInfo) []queue.QueueInfo {
	out := make([]queue.QueueInfo, 0, len(qs))
	for _, q := range qs {
		if strings.HasPrefix(strings.ToLower(q.Type), "exe") {
			out = append(out, q)
		}
	}
	return out
}

// upQueues splits queues into the ones that are up and a count of the rest — the last
// filter in the default view. "Up" is defined as NOT known-down: a queue is dropped only
// when it is explicitly stopped/disabled (E=N or R=N); blank/"-" flags (systems that don't
// report state) are kept, since we can't confirm they're down. After the Exe filter the
// survivors should all be up, so a nonzero down count is abnormal and the caller warns.
func upQueues(qs []queue.QueueInfo) (up []queue.QueueInfo, down int) {
	for _, q := range qs {
		if q.Enabled == "N" || q.Running == "N" {
			down++
			continue
		}
		up = append(up, q)
	}
	return up, down
}

// toQueueRows maps parsed QueueInfo to render's plain QueueRow (keeping render domain-
// free). label is the cluster, used to resolve the config class/cores overrides: Class is
// the config override or the name heuristic, and MaxNodes = ceil(MaxCores / cores-per-node)
// with a per-queue cores override falling back to the cluster default. In a collate view
// each row carries its OWN cluster (System), which overrides label — the overrides are
// per-cluster, so a fleet row must resolve against the cluster it came from.
func toQueueRows(label string, qs []queue.QueueInfo) []render.QueueRow {
	rows := make([]render.QueueRow, len(qs))
	for i, q := range qs {
		cluster := queueCluster(label, q)
		rows[i] = render.QueueRow{
			System: q.System,
			Name:   q.Name, Class: queueClass(cluster, q.Name), Type: q.Type,
			Walltime: q.MaxWalltime, MaxJobs: q.MaxJobs, MaxCores: q.MaxCores,
			MaxNodes: queueMaxNodes(cluster, q.Name, q.MaxCores),
			Run:      q.JobsRun, Pend: q.JobsPend, Enabled: q.Enabled, Running: q.Running,
		}
	}
	return rows
}

// queueCluster is the cluster a queue's config overrides resolve against: its own System
// tag in a collate view, else the view's label.
func queueCluster(label string, q queue.QueueInfo) string {
	if q.System != "" {
		return q.System
	}
	return label
}

// queueClass resolves a queue's node class: the config queue→class override if set, else
// the generic name heuristic.
func queueClass(label, name string) string {
	if c := config.QueueClassOverride(label, name); c != "" {
		return c
	}
	return queue.ClassifyQueue(name)
}

// queueMaxNodes returns the max nodes a job can span in a queue = ceil(MaxCores /
// cores-per-node), or "" when cores-per-node is unconfigured (0) or MaxCores isn't a
// positive integer. cores-per-node is the per-queue override if set, else the cluster
// default — GPU/specialty queues can list a MaxCores that doesn't match the node's CPU
// core count, so their divisor is overridden in config.toml.
func queueMaxNodes(label, name, maxCores string) string {
	cpn := config.QueueCoresOverride(label, name)
	if cpn == 0 {
		cpn = config.CoresPerNodeFor(label)
	}
	return maxNodesFrom(maxCores, cpn)
}

// maxNodesFrom is the pure core of queueMaxNodes: ceil(maxCores / coresPerNode) as a
// string, or "" when coresPerNode is unset (≤0) or maxCores isn't a positive integer.
func maxNodesFrom(maxCores string, coresPerNode int) string {
	if coresPerNode <= 0 {
		return ""
	}
	mc, err := strconv.Atoi(strings.TrimSpace(maxCores))
	if err != nil || mc <= 0 {
		return ""
	}
	return strconv.Itoa((mc + coresPerNode - 1) / coresPerNode) // ceil
}

// queuesInteractive is `mu hpc queues -i`: a live-filterable, scrollable viewer over the
// queue list (read-only — queues aren't acted on), so a list noisy with project-specific
// queues can be narrowed by typing. `i` inspects a queue's full limits. Snapshot-based —
// the rows are fetched once (queues rarely change), so the refresh tick doesn't re-ssh.
func queuesInteractive(label string, qs []queue.QueueInfo, all bool) error {
	if !render.Interactive() {
		return fmt.Errorf("mu hpc queues -i needs a terminal (stdin is not a tty)")
	}
	// Keyed by System/Name: a bare queue name collides across clusters in a collate view
	// (every system has a `standard`), and the key is the picker's row ID.
	byID := make(map[string]queue.QueueInfo, len(qs))
	for _, q := range qs {
		byID[queueID(q)] = q
	}
	qrows := toQueueRows(label, qs)
	cols := render.QueueColumns(qrows, all) // shed low-priority columns to fit, like the table
	display := make([]string, len(cols))
	for i, h := range cols {
		display[i] = strings.ToUpper(h) // picker headers are uppercase (like mstat -i)
	}
	selRows := make([]render.SelectRow, len(qrows))
	for i, r := range qrows {
		cells := make([]string, len(cols))
		hues := make([]string, len(cols))
		for j, h := range cols {
			cells[j], hues[j] = queuePickerCell(h, r)
		}
		selRows[i] = render.SelectRow{ID: rowID(r), Cells: cells, Hues: hues}
	}
	return render.Viewer(render.SelectSpec{
		Title:      label + " queues",
		Columns:    display,
		Interval:   time.Hour, // static snapshot — queues rarely change; don't refetch on the tick
		Fetch:      func() []render.SelectRow { return selRows },
		Detail:     queueDetailCard(label, byID),
		FacetCol:   colIndex(cols, "Class"), // 1-based; `f` cycles all → CPU → GPU → … → all
		FacetLabel: "class",
	})
}

// queueID / rowID are the picker's row key — System/Name in a collate view, the bare name
// otherwise (the two must agree, hence one rule expressed over each shape).
func queueID(q queue.QueueInfo) string {
	if q.System == "" {
		return q.Name
	}
	return q.System + "/" + q.Name
}

func rowID(r render.QueueRow) string {
	if r.System == "" {
		return r.Name
	}
	return r.System + "/" + r.Name
}

// colIndex is the 1-based position of a header in the shed column set (0 = absent, which
// disables the facet).
func colIndex(cols []string, header string) int {
	for i, h := range cols {
		if h == header {
			return i + 1
		}
	}
	return 0
}

// queuePickerCell returns the plain cell text + house hue for a queues-picker column, so
// the picker colors cells via SelectRow.Hues (keeping them plain for the reverse-video
// cursor row) exactly as the static table colors its columns.
func queuePickerCell(header string, r render.QueueRow) (string, string) {
	switch header {
	case "System":
		return orDash(r.System), render.HueLoc
	case "Queue":
		return r.Name, render.HueGroup
	case "Class":
		return orDash(r.Class), render.HueUser // magenta
	case "Type":
		return orDash(r.Type), render.HueDim
	case "Walltime":
		return orDash(r.Walltime), render.HueName
	case "MaxJobs":
		return orDash(r.MaxJobs), ""
	case "MaxCores":
		return orDash(r.MaxCores), ""
	case "MaxNodes":
		return orDash(r.MaxNodes), ""
	case "Run":
		return orDash(r.Run), ""
	case "Pend":
		return orDash(r.Pend), ""
	case "Load":
		return render.QueueLoad(r.Run, r.Pend)
	case "State":
		return render.QueueState(r.Enabled, r.Running)
	default:
		return "", ""
	}
}

// queueDetailCard returns the `i`-inspect renderer for the queue picker: a house KVCard
// with the queue's derived class/load/state plus the full limits and per-state core figures
// the table omits, looked up by row id from the fetched snapshot.
func queueDetailCard(label string, byID map[string]queue.QueueInfo) func(string) string {
	return func(id string) string {
		q, ok := byID[id]
		if !ok {
			return ""
		}
		cluster := queueCluster(label, q)
		loadLabel, loadHue := render.QueueLoad(q.JobsRun, q.JobsPend)
		stateLabel, stateHue := render.QueueState(q.Enabled, q.Running)
		title := render.Bold("Queue "+q.Name, render.HueGroup) + "   ·   " +
			render.Fg(queueClass(cluster, q.Name), render.HueUser)
		if q.System != "" {
			title += "   ·   " + render.Fg(q.System, render.HueLoc)
		}
		return render.KVCard(title, []render.KVField{
			{Label: "State", Value: stateLabel, Hue: stateHue},
			{Label: "Load", Value: loadLabel, Hue: loadHue},
			{Label: "Type", Value: orDash(q.Type), Hue: render.HueDim},
			{Label: "Walltime", Value: orDash(q.MaxWalltime), Hue: render.HueName},
			{Label: "Max jobs", Value: orDash(q.MaxJobs)},
			{Label: "Cores/job", Value: coresRange(q.MinCores, q.MaxCores)},
			{Label: "Max nodes", Value: orDash(queueMaxNodes(cluster, q.Name, q.MaxCores))},
			{Label: "Running", Value: countCores(q.JobsRun, q.CoresRun)},
			{Label: "Pending", Value: countCores(q.JobsPend, q.CoresPend)},
		})
	}
}

// coresRange formats a queue's per-job core limits as "min – max" (or the one that's set),
// or "--" when neither is.
func coresRange(minC, maxC string) string {
	lo, hi := strings.TrimSpace(minC), strings.TrimSpace(maxC)
	switch {
	case lo == "" && hi == "":
		return "--"
	case lo == "":
		return hi
	case hi == "":
		return lo
	default:
		return lo + " – " + hi
	}
}

// countCores formats a "N jobs / M cores" line for the running/pending detail rows.
func countCores(jobs, cores string) string {
	return orDash(jobs) + " jobs / " + orDash(cores) + " cores"
}

// orDash normalizes an empty / "--" queue field to a single "--" placeholder (the picker
// counterpart of render's internal dash).
func orDash(s string) string {
	if strings.TrimSpace(s) == "" || s == "--" {
		return "--"
	}
	return s
}
