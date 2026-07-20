package render

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// JobRow is one scheduler job for JobsTable — plain fields only, so render stays
// domain-free (like MountRow). State is a normalized label ("running", "queued",
// "held", "exiting", "complete", "waiting", "suspended") or a raw scheduler code for
// anything unrecognized; the table maps it to a glyph + color.
type JobRow struct {
	ID, Name, User, Queue, Nodes, State, Elapsed, ReqWall, Reason string
	Submit, Start, End                                            string // scheduler timestamps; shown as optional time columns per JobCols
	Cluster                                                       string // collate source tag (cluster or fleet node) → the leftmost System column
	Prog                                                          string // model progress-hook value ("38%") → an auto-shown Prog column
}

// JobCols selects which optional time columns JobsTable shows. Start is the live
// queue's est/actual start (mstat --start); Submit/End are finished-job stamps (mhist:
// End by default, Submit+Start+End under --times). Each is a fixed opt-in column —
// never shed by the terminal fit, it just consumes width budget.
type JobCols struct {
	Submit, Start, End bool
}

// JobsTable renders a scheduler queue as the house table: ID / Name / Queue / NDS /
// State / Elap·Wall. The state cell is a glyph colored by state (● green running / ○
// dim queued / ! yellow held / …); the title carries the cluster + a job-count
// summary. Long names truncate on the right to keep the table from wrapping.
func JobsTable(cluster, user string, rows []JobRow, cols JobCols) {
	showUser := multipleUsers(rows)
	showCluster := anyCluster(rows)
	showProg := anyProg(rows)
	nameMax, showReason, showWall := planFit(rows, showUser, showCluster, showProg, cols)
	wallHdr := "Elap / Wall"
	if !showWall {
		wallHdr = "Elap"
	}

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	applyStyle(t)
	t.SetTitle(jobsTitle(cluster, user, rows))

	header := table.Row{}
	if showCluster {
		header = append(header, "System")
	}
	header = append(header, "ID", "Name")
	if showUser {
		header = append(header, "User")
	}
	header = append(header, "Queue", "NDS", "State")
	if showProg {
		header = append(header, "Prog")
	}
	header = append(header, wallHdr)
	if cols.Submit {
		header = append(header, "Submit")
	}
	if cols.Start {
		header = append(header, "Start")
	}
	if cols.End {
		header = append(header, "End")
	}
	t.AppendHeader(header)

	for _, r := range rows {
		row := table.Row{}
		if showCluster {
			row = append(row, r.Cluster)
		}
		row = append(row, r.ID, r.Name)
		if showUser {
			row = append(row, r.User)
		}
		cell := elapWall(r.Elapsed, r.ReqWall, showWall)
		if lvl := walltimeLevel(r.State, r.Elapsed, r.ReqWall); lvl != "" {
			cell = levelColors(lvl).Sprint(cell) // Sprint no-ops under DisableColors (plain/NO_COLOR)
		}
		row = append(row, r.Queue, dash(r.Nodes), stateCell(r, showReason))
		if showProg {
			row = append(row, dash(r.Prog))
		}
		row = append(row, cell)
		if cols.Submit {
			row = append(row, timeCell(r.Submit))
		}
		if cols.Start {
			row = append(row, timeCell(r.Start))
		}
		if cols.End {
			row = append(row, timeCell(r.End))
		}
		t.AppendRow(row)
	}

	// ColumnConfigs match by header name, so listing a column is harmless when it's
	// absent (single-cluster / no-time-column views).
	const nameCol = 2 // index of the Name config below (System, ID, Name, …)
	// Palette hues (theme-adaptive Fg), not raw colors: ID cyan, location (System) blue,
	// User magenta, Queue/partition bright-blue, times dim. Green/yellow/red stay reserved
	// for the State badge + walltime burn (status/verdict only).
	colCfg := []table.ColumnConfig{
		{Name: "System", Colors: append(tc(HueLoc), text.Bold)},
		{Name: "ID", Colors: append(tc(HueID), text.Bold)},
		{Name: "Name", WidthMaxEnforcer: truncRight},
		{Name: "User", Colors: tc(HueUser)},
		{Name: "Queue", Colors: tc(HueGroup)},
		{Name: "State", Transformer: jobStateTransformer},
		{Name: "Submit", Colors: tc(HueDim)},
		{Name: "Start", Colors: tc(HueDim)},
		{Name: "End", Colors: tc(HueDim)},
	}
	if nameMax > 0 {
		colCfg[nameCol].WidthMax = nameMax // cap Name (truncRight enforces it); ColumnConfigs match by Name so slice order is otherwise cosmetic
	}
	t.SetColumnConfigs(colCfg)
	t.Render()
}

// multipleUsers reports whether the rows span more than one owner — the cue to add a
// User column (an all-users view) vs omit it (your own jobs are all you).
func multipleUsers(rows []JobRow) bool {
	seen := ""
	for _, r := range rows {
		u := strings.TrimSpace(r.User)
		if u == "" {
			continue
		}
		if seen == "" {
			seen = u
		} else if u != seen {
			return true
		}
	}
	return false
}

// anyCluster reports whether any row carries a cluster tag — the cue to add the
// leftmost Cluster column (a cross-cluster collate) vs omit it (single-cluster view).
func anyCluster(rows []JobRow) bool {
	for _, r := range rows {
		if strings.TrimSpace(r.Cluster) != "" {
			return true
		}
	}
	return false
}

// anyProg reports whether any row carries a progress-hook value — the cue to add
// the Prog column. Hooks past the fetch cap simply leave it absent everywhere.
func anyProg(rows []JobRow) bool {
	for _, r := range rows {
		if strings.TrimSpace(r.Prog) != "" {
			return true
		}
	}
	return false
}

// stateCell is the State column value: the state badge, plus a pending job's reason
// (SLURM — why it waits) when showReason survives the terminal fit.
func stateCell(r JobRow, showReason bool) string {
	s := jobStateBadge(r.State)
	if showReason && r.Reason != "" {
		s += " · " + r.Reason
	}
	return s
}

// planFit decides how the table fits the terminal: Name's max width (0 = uncapped)
// and whether the pending-reason and walltime survive. Name is first capped at a hard
// nameCap (so one long job name can't dominate even a wide terminal), then it sheds width
// in priority order — shrink Name to a floor, then drop the reason, then the walltime — so
// even a narrow terminal renders one clean, unwrapped table. It runs for both pretty and
// --plain (both are human views that fit the terminal), no-opping only when the width
// is unknown (piped/redirected), where full values flow — use --json for complete data.
func planFit(rows []JobRow, showUser, showCluster, showProg bool, cols JobCols) (nameMax int, showReason, showWall bool) {
	showReason, showWall = true, true
	tw := termWidth()
	if tw <= 0 {
		return 0, true, true
	}
	const nameFloor = 6
	const nameCap = 20 // hard ceiling on Name regardless of terminal room; longer names truncate
	w := measureCols(rows, showUser, showCluster, showProg, cols)
	// StyleRounded overhead: 2 padding + a border glyph per column → 3*ncols + 1.
	nCols := 6
	if showUser {
		nCols++
	}
	if showCluster {
		nCols++
	}
	if showProg {
		nCols++
	}
	nCols += boolN(cols.Submit) + boolN(cols.Start) + boolN(cols.End)
	// Time columns are opt-in, so they're fixed (never shed) — they just consume budget.
	room := tw - (w.id + w.user + w.cluster + w.queue + w.nds + w.badge + w.prog + w.elap + w.submit + w.start + w.end + 3*nCols + 1) // for Name + reason + wall
	nameMax = min(w.name, nameCap)
	for {
		need := nameMax + boolW(showReason, w.reason) + boolW(showWall, w.wall)
		if need <= room {
			break
		}
		switch {
		case nameMax > nameFloor:
			nameMax = max(nameFloor, room-boolW(showReason, w.reason)-boolW(showWall, w.wall))
		case showReason:
			showReason = false
		case showWall:
			showWall = false
		default:
			return nameFloor, false, false // can't shrink more — accept a slight overflow
		}
	}
	if nameMax >= w.name {
		nameMax = 0 // no cap needed
	}
	return nameMax, showReason, showWall
}

// colWidths is the measured display width of each queue-table column — the header width
// widened by the widest visible cell. The optional User/System and the opt-in time columns
// are 0 when off. planFit consumes these to size Name and decide what sheds.
type colWidths struct {
	id, queue, nds, badge, elap, name int
	user, cluster, reason, wall       int
	submit, start, end, prog          int
}

// measureCols measures every column's display width across rows in one pass, seeding each
// from its header and widening to the widest cell (reason/wall include their " · "/" / "
// lead-in; time columns use the formatted cell). Split out of planFit so the fit-shedding
// logic there reads as pure arithmetic over the measured widths.
func measureCols(rows []JobRow, showUser, showCluster, showProg bool, cols JobCols) colWidths {
	w := colWidths{
		id: len("ID"), queue: len("Queue"), nds: len("NDS"),
		badge: len("State"), elap: len("Elap"), name: len("Name"),
		submit: hdrW(cols.Submit, "Submit"), start: hdrW(cols.Start, "Start"), end: hdrW(cols.End, "End"),
	}
	if showUser {
		w.user = len("User")
	}
	if showCluster {
		w.cluster = len("System")
	}
	if showProg {
		w.prog = len("Prog") // cells are at most "100%" — the header width holds
	}
	for _, r := range rows {
		w.id = max(w.id, text.StringWidth(r.ID))
		w.queue = max(w.queue, text.StringWidth(r.Queue))
		w.nds = max(w.nds, text.StringWidth(dash(r.Nodes)))
		w.badge = max(w.badge, text.StringWidth(jobStateBadge(r.State)))
		w.elap = max(w.elap, text.StringWidth(dash(r.Elapsed)))
		w.name = max(w.name, text.StringWidth(r.Name))
		if showUser {
			w.user = max(w.user, text.StringWidth(r.User))
		}
		if showCluster {
			w.cluster = max(w.cluster, text.StringWidth(r.Cluster))
		}
		if r.Reason != "" {
			w.reason = max(w.reason, text.StringWidth(" · "+r.Reason))
		}
		if strings.TrimSpace(r.ReqWall) != "" {
			w.wall = max(w.wall, text.StringWidth(" / "+r.ReqWall))
		}
		if cols.Submit {
			w.submit = max(w.submit, text.StringWidth(timeCell(r.Submit)))
		}
		if cols.Start {
			w.start = max(w.start, text.StringWidth(timeCell(r.Start)))
		}
		if cols.End {
			w.end = max(w.end, text.StringWidth(timeCell(r.End)))
		}
	}
	return w
}

// boolW returns w when on, else 0 — for conditional width sums.
func boolW(on bool, w int) int {
	if on {
		return w
	}
	return 0
}

// boolN counts a flag as a column (1 when on, else 0).
func boolN(on bool) int {
	if on {
		return 1
	}
	return 0
}

// hdrW seeds a time column's width with its header when shown, else 0.
func hdrW(on bool, header string) int {
	if on {
		return len(header)
	}
	return 0
}

// jobsTitle is the two-line table title: the cluster (or "Queue") over a job-count
// summary and the username.
func jobsTitle(cluster, user string, rows []JobRow) string {
	head := "Queue"
	if cluster != "" {
		head = cluster
	}
	var run, q, other int
	for _, r := range rows {
		switch r.State {
		case "running":
			run++
		case "queued":
			q++
		default:
			other++
		}
	}
	sub := fmt.Sprintf("%d jobs", len(rows))
	if len(rows) > 0 {
		var parts []string
		if run > 0 {
			parts = append(parts, fmt.Sprintf("%d running", run))
		}
		if q > 0 {
			parts = append(parts, fmt.Sprintf("%d queued", q))
		}
		if other > 0 {
			parts = append(parts, fmt.Sprintf("%d other", other))
		}
		sub += " (" + strings.Join(parts, ", ") + ")"
	}
	if user != "" && user != "?" {
		sub += "   " + user
	}
	return head + "\n" + sub
}

// jobStateBadge is the plain (uncolored) badge text; jobStateTransformer colors it
// without changing width, so go-pretty measures the real display width.
func jobStateBadge(state string) string {
	switch state {
	case "running":
		return glyph("●", "*") + " running"
	case "queued":
		return glyph("○", "o") + " queued"
	case "held":
		return glyph("!", "!") + " held"
	case "exiting":
		return glyph("◐", "~") + " exiting"
	case "complete":
		return glyph("✓", "+") + " complete"
	case "waiting":
		return glyph("○", "o") + " waiting"
	case "suspended":
		return glyph("!", "!") + " suspended"
	default:
		s := strings.TrimSpace(state)
		if s == "" {
			s = "?"
		}
		return glyph("·", ".") + " " + s
	}
}

func jobStateTransformer(v interface{}) string {
	s := fmt.Sprint(v)
	switch {
	case strings.Contains(s, "running"):
		return text.Colors{text.FgGreen}.Sprint(s)
	case strings.Contains(s, "held"), strings.Contains(s, "suspended"):
		return text.Colors{text.FgYellow}.Sprint(s)
	case strings.Contains(s, "exiting"), strings.Contains(s, "complete"):
		return text.Colors{text.FgCyan}.Sprint(s)
	default: // queued, waiting, unknown → terminal default fg (always legible, unlike a dim gray)
		return s
	}
}

// elapWall renders the elapsed/walltime cell: "elap / wall", or just elapsed when
// the format carries no requested walltime (narrow qstat).
func elapWall(elapsed, reqWall string, showWall bool) string {
	e := dash(elapsed)
	if !showWall || strings.TrimSpace(reqWall) == "" {
		return e
	}
	return e + " / " + dash(reqWall)
}

// walltimeLevel grades a running job by how much of its requested walltime it has
// burned: "error" at/over 90% (imminent walltime kill — checkpoint now), "warn" at
// ≥75%, else "" (no color). Only running jobs are graded: a queued job hasn't
// started, and a finished one is moot. Feeds levelColors so the Elap/Wall cell
// reuses the house warn-yellow / error-red.
func walltimeLevel(state, elapsed, reqWall string) string {
	if state != "running" {
		return ""
	}
	e, ok1 := durSecs(elapsed)
	w, ok2 := durSecs(reqWall)
	if !ok1 || !ok2 || w <= 0 {
		return ""
	}
	switch r := float64(e) / float64(w); {
	case r >= 0.90:
		return "error"
	case r >= 0.75:
		return "warn"
	default:
		return ""
	}
}

// durSecs parses a scheduler duration into seconds, anchored on the right (rightmost
// field = seconds): SLURM "[D-]HH:MM:SS" / "MM:SS" parse exactly, and PBS "HH:MM"
// comes out 60× low — but walltimeLevel only uses the elapsed/limit ratio, which is
// scale-invariant when both share a format (they do within one job), so the ratio is
// always right. Returns ok=false for non-numeric limits ("UNLIMITED", "--", "").
func durSecs(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "--" {
		return 0, false
	}
	days := 0
	if i := strings.IndexByte(s, '-'); i > 0 { // SLURM day form "D-HH:MM:SS"
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		days, s = d, s[i+1:]
	}
	secs := 0
	for _, f := range strings.Split(s, ":") {
		n, err := strconv.Atoi(f)
		if err != nil {
			return 0, false
		}
		secs = secs*60 + n
	}
	return days*86400 + secs, true
}

// timeCell formats a scheduler timestamp for a compact table time column (Submit /
// Start / End): an ISO 8601 stamp "2006-01-02T15:04:05" collapses to "01-02 15:04"
// (drop year + seconds — tables are length-constrained; the full date lives in the
// minfo card), while a non-ISO value a scheduler may emit ("N/A" unschedulable,
// "Unknown") or an empty/unavailable field passes through dash().
func timeCell(s string) string {
	s = strings.TrimSpace(s)
	// ISO 8601 YYYY-MM-DDTHH:MM:SS — slice MM-DD and HH:MM by fixed offsets.
	if len(s) >= 16 && s[4] == '-' && s[7] == '-' && s[10] == 'T' && s[13] == ':' {
		return s[5:10] + " " + s[11:16]
	}
	return dash(s)
}

// dash normalizes empty / "--" fields to a single "--" placeholder.
func dash(s string) string {
	if strings.TrimSpace(s) == "" || s == "--" {
		return "--"
	}
	return s
}

// truncRight trims to maxLen display columns, keeping the head (a job name's
// distinguishing prefix) behind a trailing ….
func truncRight(s string, maxLen int) string {
	if text.StringWidth(s) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return "…"
	}
	r := []rune(s)
	i, w := 0, 0
	for i < len(r) && w+text.StringWidth(string(r[i])) <= maxLen-1 {
		w += text.StringWidth(string(r[i]))
		i++
	}
	return string(r[:i]) + "…"
}

// TunnelRow is one open tunnel in the TunnelsTable — a background `mu job tunnel`, its local
// Port (what you point a browser at), the job behind it, and how much walltime is left.
type TunnelRow struct {
	ID, Port, System, Job, Node, State, WallLeft string
}

// TunnelsTable renders the open background tunnels. Domain-shaped like the other house
// tables (JobsTable et al.): the caller hands rows, render owns the frame and accents. When
// the terminal is too narrow for every column, the least useful ones are dropped in order
// (Node, then Job) until it fits; piped output (unknown width) keeps them all so nothing is
// lost downstream.
func TunnelsTable(rows []TunnelRow) {
	cols := []col[TunnelRow]{
		{"ID", func(r TunnelRow) string { return r.ID }},
		{"PORT", func(r TunnelRow) string { return r.Port }},
		{"System", func(r TunnelRow) string { return r.System }},
		{"Job", func(r TunnelRow) string { return r.Job }},
		{"Node", func(r TunnelRow) string { return r.Node }},
		{"State", func(r TunnelRow) string { return r.State }},
		{"Wall left", func(r TunnelRow) string { return r.WallLeft }},
	}
	keep := fitTunnelColumns(cols, rows, []string{"Node", "Job"}, termWidth())

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	applyStyle(t)
	t.Style().Title.Colors = text.Colors{text.FgCyan, text.Bold}
	t.Style().Color.Header = text.Colors{text.FgCyan}
	t.Style().Color.Border = text.Colors{text.FgHiBlack}
	t.Style().Color.Separator = text.Colors{text.FgHiBlack}
	t.SetTitle("Open tunnels")

	var kept []col[TunnelRow]
	for _, c := range cols {
		if keep[c.header] {
			kept = append(kept, c)
		}
	}
	appendCols(t, kept, rows)
	t.SetColumnConfigs([]table.ColumnConfig{
		{Name: "ID", Colors: text.Colors{text.FgMagenta, text.Bold}}, // the handle you close by
		{Name: "PORT", Colors: text.Colors{text.FgCyan}},
	})
	t.Render()
}

// fitTunnelColumns decides which columns fit the terminal width, dropping the named columns in
// order (lowest value first) only as needed. Unknown width (piped, avail <= 0) keeps everything.
// The width model matches go-pretty's rounded style: each column costs its widest cell plus 3
// (separator + two padding spaces), plus one closing border.
func fitTunnelColumns(cols []col[TunnelRow], rows []TunnelRow, dropOrder []string, avail int) map[string]bool {
	keep := make(map[string]bool, len(cols))
	for _, c := range cols {
		keep[c.header] = true
	}
	if avail <= 0 {
		return keep
	}
	natural := make(map[string]int, len(cols))
	for _, c := range cols {
		w := utf8.RuneCountInString(c.header)
		for _, r := range rows {
			if n := utf8.RuneCountInString(c.cell(r)); n > w {
				w = n
			}
		}
		natural[c.header] = w
	}
	fits := func() bool {
		total := 1 // closing border
		for _, c := range cols {
			if keep[c.header] {
				total += natural[c.header] + 3
			}
		}
		return total <= avail
	}
	for _, d := range dropOrder {
		if fits() {
			break
		}
		keep[d] = false
	}
	return keep
}
