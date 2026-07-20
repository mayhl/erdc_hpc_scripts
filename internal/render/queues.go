package render

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// QueueRow is one batch queue for QueuesTable — plain fields only, so render stays
// domain-free (like JobRow/ProcRow). Type is the queue class (Exe = submittable, Rou =
// routing); Enabled/Running are the raw flags (Y / N / -, or blank) — "-" or blank means
// the system doesn't report them, which the State cell renders as unknown, not disabled.
// System is the owning cluster in a collate view, "" in a single-cluster view (where the
// cluster is the title and the column would be uniform noise) — as in StorageRow.
type QueueRow struct {
	System, Name, Class, Type, Walltime, MaxJobs, MaxCores, MaxNodes, Run, Pend string
	Enabled, Running                                                            string
}

// QueuesTable renders a cluster's batch queues (show_queues) as the house table. The full
// column set is Queue / Class / [Type] / Walltime / MaxJobs / MaxCores / [MaxNodes] / Run /
// Pend / Load / [State]. Class is the inferred node class; MaxNodes = MaxCores / cores-per-
// node (shown only when configured). The -a view shows every column; the default view is
// already Exe+up filtered (so Type/State would be uniform and are dropped) and AUTO-FITS the
// terminal — planQueueCols sheds the raw columns MaxJobs → Pend → Run → MaxCores in priority
// order until the row fits, keeping Queue/Class/Walltime/a size column/Load.
func QueuesTable(cluster string, rows []QueueRow, all bool) {
	cols := planQueueCols(rows, all)
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	applyStyle(t)
	t.SetTitle(fmt.Sprintf("%s — %d queues", cluster, len(rows)))
	appendCols(t, cols, rows)
	// ColumnConfigs match by header name, so a config for an absent column is harmless.
	// Class gets its own blue (HueLoc) to stand out next to the bright-blue bold Queue.
	t.SetColumnConfigs([]table.ColumnConfig{
		{Name: "System", Colors: append(tc(HueLoc), text.Bold)}, // cluster — same hue as mstat's collate column
		{Name: "Queue", Colors: append(tc(HueGroup), text.Bold), WidthMax: 24, WidthMaxEnforcer: truncRight},
		{Name: "Class", Colors: tc(HueUser)}, // magenta — stands out from the blue Queue
		{Name: "Type", Colors: tc(HueDim)},
		{Name: "Walltime", Colors: tc(HueName)},
	})
	t.Render()
}

// QueueColumns returns the ordered column headers the queues view shows after width-
// shedding — the same plan QueuesTable renders, exposed so the interactive picker sheds the
// same low-priority columns to fit the terminal.
func QueueColumns(rows []QueueRow, all bool) []string {
	cols := planQueueCols(rows, all)
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.header
	}
	return out
}

// queueColDefs is the full set of column formatters, keyed by header.
func queueColDefs() map[string]col[QueueRow] {
	return map[string]col[QueueRow]{
		"System":   {"System", func(r QueueRow) string { return dash(r.System) }},
		"Queue":    {"Queue", func(r QueueRow) string { return r.Name }},
		"Class":    {"Class", func(r QueueRow) string { return dash(r.Class) }},
		"Type":     {"Type", func(r QueueRow) string { return dash(r.Type) }},
		"Walltime": {"Walltime", func(r QueueRow) string { return dash(r.Walltime) }},
		"MaxJobs":  {"MaxJobs", func(r QueueRow) string { return dash(r.MaxJobs) }},
		"MaxCores": {"MaxCores", func(r QueueRow) string { return dash(r.MaxCores) }},
		"MaxNodes": {"MaxNodes", func(r QueueRow) string { return dash(r.MaxNodes) }},
		"Run":      {"Run", func(r QueueRow) string { return dash(r.Run) }},
		"Pend":     {"Pend", func(r QueueRow) string { return dash(r.Pend) }},
		"Load":     {"Load", func(r QueueRow) string { return queueLoadCell(r.Run, r.Pend) }},
		"State":    {"State", func(r QueueRow) string { return queueStateCell(r.Enabled, r.Running) }},
	}
}

// planQueueCols builds the ordered columns. MaxNodes appears only when configured (some row
// has a value); when it's absent MaxCores is the protected size column, else MaxCores is
// shed-able. The -a view returns every applicable column unshed. The default view auto-fits
// the terminal: it starts from the full default set and sheds the raw columns in priority
// order (MaxJobs → Pend → Run → MaxCores) until it fits termWidth, always keeping Queue,
// Class, Walltime, the size column, and Load. A zero/unknown width (piped) sheds nothing.
func planQueueCols(rows []QueueRow, all bool) []col[QueueRow] {
	defs := queueColDefs()
	hasNodes := false
	for _, r := range rows {
		if r.MaxNodes != "" {
			hasNodes = true
			break
		}
	}
	// System leads only in a collate view (rows tagged by cluster), like StorageTable —
	// and is never shed: it's what disambiguates same-named queues across clusters.
	var lead []string
	if anyReported(rows, func(r QueueRow) string { return r.System }) {
		lead = []string{"System"}
	}

	if all {
		order := append(lead, "Queue", "Class", "Type", "Walltime", "MaxJobs", "MaxCores")
		if hasNodes {
			order = append(order, "MaxNodes")
		}
		order = append(order, "Run", "Pend", "Load", "State")
		return pickCols(defs, order)
	}

	order := append(lead, "Queue", "Class", "Walltime", "MaxJobs", "MaxCores")
	if hasNodes {
		order = append(order, "MaxNodes")
	}
	order = append(order, "Run", "Pend", "Load")

	shed := []string{"MaxJobs", "Pend", "Run"}
	if hasNodes { // MaxNodes is the size column, so MaxCores is redundant and shed-able
		shed = append(shed, "MaxCores")
	}
	dropped := map[string]bool{}
	for {
		cur := keepUndropped(order, dropped)
		if queueColsFit(defs, cur, rows) {
			return pickCols(defs, cur)
		}
		next := ""
		for _, k := range shed {
			if !dropped[k] {
				next = k
				break
			}
		}
		if next == "" { // nothing left to shed — render the protected set even if it overflows
			return pickCols(defs, cur)
		}
		dropped[next] = true
	}
}

// pickCols resolves ordered header keys to their column defs.
func pickCols(defs map[string]col[QueueRow], order []string) []col[QueueRow] {
	out := make([]col[QueueRow], len(order))
	for i, k := range order {
		out[i] = defs[k]
	}
	return out
}

// keepUndropped returns order minus the dropped keys, preserving sequence.
func keepUndropped(order []string, dropped map[string]bool) []string {
	out := make([]string, 0, len(order))
	for _, k := range order {
		if !dropped[k] {
			out = append(out, k)
		}
	}
	return out
}

// queueColsFit reports whether the given columns render within the terminal width. A
// zero/unknown width (output not a tty) is treated as unconstrained → always fits. Queue is
// capped at its 24-col truncRight width so a long name doesn't force needless shedding.
func queueColsFit(defs map[string]col[QueueRow], order []string, rows []QueueRow) bool {
	w := termWidth()
	if w <= 0 {
		return true
	}
	total := 0
	for _, k := range order {
		c := defs[k]
		cw := text.StringWidth(c.header)
		for _, r := range rows {
			if x := text.StringWidth(c.cell(r)); x > cw {
				cw = x
			}
		}
		if k == "Queue" && cw > 24 {
			cw = 24
		}
		total += cw
	}
	// StyleRounded overhead: 2 padding + a border glyph per column, + the final border.
	return total+3*len(order)+1 <= w
}

// QueueState classifies a queue's enabled/running flags (Y / N / -) into a display label
// and a house hue, tolerating the "-"/blank some systems emit: ○ disabled (E=N, HueDim),
// ○ stopped (enabled but R=N, HueWarn), ● up (E=Y and R=Y, HueOK), and -- (HueDim) when
// the flags aren't reported. Shared by the static table cell and the interactive picker
// row so both read the flags the same way.
func QueueState(enabled, running string) (label, hue string) {
	switch {
	case enabled == "N":
		return "○ disabled", HueDim
	case running == "N":
		return "○ stopped", HueWarn
	case enabled == "Y" && running == "Y":
		return "● up", HueOK
	default: // "-"/blank on either flag → not reported
		return "--", HueDim
	}
}

// queueStateCell renders the QueueState label colored for the static table. tc(hue).Sprint
// no-ops under DisableColors (plain / NO_COLOR).
func queueStateCell(enabled, running string) string {
	label, hue := QueueState(enabled, running)
	return tc(hue).Sprint(label)
}

// QueueLoad summarizes a queue's live utilization from its running/pending counts into a
// label + hue, graded by the backlog ratio Pend/Run: unused (nothing running or pending,
// HueDim), low (Pend ≤ 1×Run, HueOK), med (≤ 3×Run, HueWarn), high (≤ 10×Run, HueErr),
// extreme (> 10×Run — or nothing running with jobs pending, i.e. starved — HueErr), or --
// (HueDim) when neither count is numeric. high and extreme share red (warm = status), told
// apart by the word. Shared by the static table cell and the interactive picker row.
func QueueLoad(run, pend string) (label, hue string) {
	r, rok := atoiField(run)
	p, pok := atoiField(pend)
	switch {
	case !rok && !pok:
		return "--", HueDim
	case r == 0 && p == 0:
		return "unused", HueDim
	case r == 0: // jobs pending with nothing running → starved
		return "extreme", HueErr
	case p <= r: // backlog no bigger than what's running
		return "low", HueOK
	case p <= 3*r:
		return "med", HueWarn
	case p <= 10*r:
		return "high", HueErr
	default:
		return "extreme", HueErr
	}
}

// queueLoadCell renders the QueueLoad label colored for the static table.
func queueLoadCell(run, pend string) string {
	label, hue := QueueLoad(run, pend)
	return tc(hue).Sprint(label)
}

// atoiField parses a queue count field to an int, ok=false for a blank / "--" / non-numeric
// value (so QueueLoad can fall back to "--" rather than treat unknown as zero).
func atoiField(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return n, true
}
