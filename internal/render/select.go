package render

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
)

// refreshInterval is the default picker refresh cadence — right for a cheap local
// fetch (on an HPC login node). A remote fetch (off-HPC, ssh + Kerberos per tick)
// should pass a longer SelectSpec.Interval so it isn't hammered.
const refreshInterval = 2 * time.Second

// SelectRow is one row of the interactive picker: a stable ID (used for selection
// and returned to the caller), the column cells, and optional per-cell hues (parallel
// to Cells; "" = terminal default). Domain-free — procs and jobs both build these.
type SelectRow struct {
	ID    string
	Cells []string
	Hues  []string
	// Preview is the row's full-text block for the SelectSpec.Preview pane ("" = none).
	// Carried on the row — built in bulk by Fetch — so cursor moves never wait on a fetch.
	Preview string
}

// SelectSpec configures the picker: the action verb (e.g. "kill", shown as "Select
// to kill"), the column headers, and a Fetch that supplies rows — re-called on a
// timer so the list stays live (new rows appear, gone ones drop). Put the widest /
// most variable column LAST; it absorbs the leftover terminal width.
type SelectSpec struct {
	Verb     string
	Columns  []string
	Fetch    func() []SelectRow
	Interval time.Duration // refresh cadence; 0 → default (refreshInterval). Slow it for remote fetches.
	// Detail, when set, enables the `i` inspect key: it maps a row ID to a rendered
	// detail block (e.g. the job card) shown as a modal overlay, dismissed by any key.
	// Called off the UI loop (it may ssh), so a slow fetch doesn't freeze the picker.
	// nil → no `i` key (e.g. the process picker has no card).
	Detail func(id string) string
	// ReadOnly turns the widget into a VIEWER: no selection marks or select keys, and
	// the footer/title drop the "select"/verb language. Scroll, filter, live refresh,
	// and `i` inspect still work. Used by `mu log -i`. Title labels the header (a plain
	// title instead of "Select to <verb>").
	ReadOnly bool
	Title    string
	// PickOne turns the widget into a single-pick CHOOSER: enter accepts the CURSOR row
	// (a one-element result) and the mark keys (space/a) are gone — for cut-point picks
	// like `grv -i` where landing on a row means "through here".
	PickOne bool
	// Preview turns on the live pane under the list: the CURSOR row's Preview text
	// (e.g. the full commit message), following the cursor as it moves. The list yields
	// up to half its height to the pane; a cropped pane defers to the `i` Detail
	// overlay for the full text.
	Preview bool
	// StderrUI renders the picker on stderr, keeping stdout clean for a machine-readable
	// result a wrapping script captures (e.g. grv -i reading the picked count).
	StderrUI bool
	// FacetCol enables the `f` key to cycle the list through the distinct values of one
	// column: all → value1 → value2 → … → all. It's the 1-based index of that column in
	// Columns (0 = disabled). Domain-free — the widget just cycles the cell values.
	// FacetLabel names it in the footer ("f class: GPU"); defaults to the column header.
	FacetCol   int
	FacetLabel string
}

// Select runs the interactive picker and returns the IDs the user marked (nil if
// they cancelled). It is a SELECTOR, not an actuator — it never acts; the caller
// runs the shared kill+confirm+log path. Selection is keyed by ID so marks survive
// both filtering and a live refresh. The Model/Update/View island stays private
// behind this plain call, like Spinner/ProgressBar.
func Select(spec SelectSpec) ([]string, error) {
	rows := spec.Fetch()
	if len(rows) == 0 {
		return nil, nil
	}
	var opts []tea.ProgramOption
	if spec.StderrUI {
		opts = append(opts, tea.WithOutput(os.Stderr))
	}
	res, err := tea.NewProgram(newSelectModel(spec, rows), opts...).Run()
	if err != nil {
		return nil, err
	}
	m := res.(selectModel)
	if !m.confirmed {
		return nil, nil
	}
	if spec.PickOne {
		if m.cursor < len(m.visible) {
			return []string{m.rows[m.visible[m.cursor]].ID}, nil
		}
		return nil, nil
	}
	var out []string
	for _, r := range m.rows {
		if m.selected[r.ID] {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

// Viewer runs the widget as a read-only, scrollable, live-filterable viewer (no
// selection or action) — e.g. `mu log -i`. It reuses the Select model in ReadOnly
// mode and discards the (empty) selection.
func Viewer(spec SelectSpec) error {
	spec.ReadOnly = true
	_, err := Select(spec)
	return err
}

// selectModel is the picker's private bubbletea model. cursor/top index into the
// filtered `visible` view; selected is keyed by row ID so marks survive filtering
// and live refresh.
type selectModel struct {
	spec          SelectSpec
	rows          []SelectRow
	visible       []int // indices into rows, after the current filter
	selected      map[string]bool
	cursor        int // into visible
	top           int // scroll offset into visible
	filter        string
	filtering     bool
	facetVal      string // active FacetCol filter value; "" = all (no facet filter)
	detail        string // inspect overlay text; "" = list view
	detailLoading bool   // Detail fetch in flight (overlay shows a spinner-less notice)
	interval      time.Duration
	width, height int
	confirmed     bool
}

func newSelectModel(spec SelectSpec, rows []SelectRow) selectModel {
	interval := spec.Interval
	if interval <= 0 {
		interval = refreshInterval
	}
	m := selectModel{
		spec:     spec,
		rows:     rows,
		selected: make(map[string]bool),
		interval: interval,
		width:    80,
		height:   24,
	}
	m.recompute()
	return m
}

func (m selectModel) Init() tea.Cmd { return tickCmd(m.interval) }

type tickMsg struct{}

func tickCmd(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
}

// refresh re-fetches the list, keeping the filter, ID-keyed selection, and the
// cursor's ID (so a row doesn't jump under you as the list changes).
func (m *selectModel) refresh() {
	curID := ""
	if m.cursor < len(m.visible) {
		curID = m.rows[m.visible[m.cursor]].ID
	}
	m.rows = m.spec.Fetch()
	m.recompute() // resets cursor/top to 0
	if curID != "" {
		for i, idx := range m.visible {
			if m.rows[idx].ID == curID {
				m.cursor = i
				break
			}
		}
	}
	m.clampScroll()
}

func (m selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampScroll()
	case tickMsg:
		if !m.filtering && m.detail == "" && !m.detailLoading { // don't churn under the filter box or the overlay
			m.refresh()
		}
		return m, tickCmd(m.interval)
	case detailMsg:
		m.detailLoading = false
		m.detail = msg.text
		if strings.TrimSpace(m.detail) == "" {
			m.detail = "(no detail available)"
		}
		return m, nil
	case tea.KeyPressMsg:
		if m.detailLoading { // ignore keys until the fetch lands
			return m, nil
		}
		if m.detail != "" { // any key dismisses the inspect overlay
			m.detail = ""
			return m, nil
		}
		if m.filtering {
			return m.updateFilter(msg), nil
		}
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.confirmed = false
			return m, tea.Quit
		case "enter":
			m.confirmed = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			m.clampScroll()
		case "down", "j":
			if m.cursor < len(m.visible)-1 {
				m.cursor++
			}
			m.clampScroll()
		case " ", "space":
			if m.spec.ReadOnly || m.spec.PickOne {
				break // viewer / single-pick: no marks
			}
			if len(m.visible) > 0 {
				id := m.rows[m.visible[m.cursor]].ID
				m.selected[id] = !m.selected[id]
			}
		case "a": // toggle every currently-visible row
			if m.spec.ReadOnly || m.spec.PickOne {
				break
			}
			all := true
			for _, idx := range m.visible {
				if !m.selected[m.rows[idx].ID] {
					all = false
					break
				}
			}
			for _, idx := range m.visible {
				m.selected[m.rows[idx].ID] = !all
			}
		case "/":
			m.filtering = true
		case "f": // cycle the facet filter (e.g. class): all → v1 → … → all
			if m.spec.FacetCol > 0 {
				m.facetVal = m.nextFacet()
				m.recompute()
				m.clampScroll()
			}
		case "i": // inspect the cursor row (job card); async so a remote fetch doesn't freeze
			if m.spec.Detail != nil && m.cursor < len(m.visible) {
				id := m.rows[m.visible[m.cursor]].ID
				m.detailLoading = true
				return m, fetchDetailCmd(m.spec.Detail, id)
			}
		}
	}
	return m, nil
}

// detailMsg carries the result of a Detail fetch back onto the UI loop.
type detailMsg struct{ text string }

// fetchDetailCmd runs the (possibly slow / ssh-backed) Detail lookup off the UI loop
// and delivers the rendered block as a detailMsg.
func fetchDetailCmd(fn func(string) string, id string) tea.Cmd {
	return func() tea.Msg { return detailMsg{fn(id)} }
}

// updateFilter handles keystrokes while the filter box is active.
func (m selectModel) updateFilter(msg tea.KeyPressMsg) selectModel {
	switch msg.String() {
	case "enter":
		m.filtering = false
	case "esc":
		m.filtering = false
		m.filter = ""
		m.recompute()
	case "backspace":
		if r := []rune(m.filter); len(r) > 0 {
			m.filter = string(r[:len(r)-1])
			m.recompute()
		}
	default:
		if t := msg.Key().Text; t != "" {
			m.filter += t
			m.recompute()
		}
	}
	return m
}

// recompute rebuilds the visible set from the facet filter (an exact match on the
// FacetCol cell) and the text filter (case-insensitive substring over the ID + all
// cells), then resets the cursor to the top.
func (m *selectModel) recompute() {
	q := strings.ToLower(m.filter)
	m.visible = m.visible[:0]
	for i, r := range m.rows {
		if m.facetVal != "" && m.facetCell(r) != m.facetVal {
			continue
		}
		if q == "" || strings.Contains(strings.ToLower(r.ID+" "+strings.Join(r.Cells, " ")), q) {
			m.visible = append(m.visible, i)
		}
	}
	m.cursor, m.top = 0, 0
}

// facetCell returns a row's FacetCol value ("" when the facet is disabled or the row is
// too short — such a row is filtered out while a facet is active).
func (m selectModel) facetCell(r SelectRow) string {
	if m.spec.FacetCol <= 0 || m.spec.FacetCol > len(r.Cells) {
		return ""
	}
	return r.Cells[m.spec.FacetCol-1]
}

// facetValues returns the sorted distinct non-empty FacetCol values across all rows.
func (m selectModel) facetValues() []string {
	seen := map[string]bool{}
	var vals []string
	for _, r := range m.rows {
		if v := m.facetCell(r); v != "" && !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	sort.Strings(vals)
	return vals
}

// nextFacet advances the facet cycle: all → value1 → … → last → all. A current value
// that vanished after a refresh restarts at the first value.
func (m selectModel) nextFacet() string {
	vals := m.facetValues()
	if len(vals) == 0 {
		return ""
	}
	if m.facetVal == "" {
		return vals[0]
	}
	for i, v := range vals {
		if v == m.facetVal {
			if i+1 < len(vals) {
				return vals[i+1]
			}
			return "" // past the last → back to all
		}
	}
	return vals[0]
}

// facetHint is the footer fragment for the f key: the label + the active value (or "all").
func (m selectModel) facetHint(dot string) string {
	if m.spec.FacetCol <= 0 {
		return ""
	}
	label := m.spec.FacetLabel
	if label == "" && m.spec.FacetCol-1 < len(m.spec.Columns) {
		label = strings.ToLower(m.spec.Columns[m.spec.FacetCol-1])
	}
	val := m.facetVal
	if val == "" {
		val = "all"
	}
	return dot + "f " + label + ": " + val
}

// chromeLines counts the fixed lines around the list: title + border top + header +
// border bottom + footer (+ filter line).
func (m *selectModel) chromeLines() int {
	if m.filtering {
		return 6
	}
	return 5
}

func (m *selectModel) pageSize() int {
	p := m.height - m.chromeLines()
	if m.spec.Preview {
		// The pane takes what's left below the list: the list keeps at most half the
		// usable height (and shrinks to its row count when smaller), but only yields
		// when the pane earns at least 3 lines — a tiny terminal keeps the full list.
		half := max(p/2, 3)
		if n := len(m.visible); n > 0 && n < half {
			half = n
		}
		if half < p && p-half-1 >= 3 {
			p = half
		}
	}
	if p > 1 {
		return p
	}
	return 1
}

// previewBudget is the pane's line allowance: the usable height minus the list block
// and its rule line. Below 3 the pane isn't drawn.
func (m *selectModel) previewBudget() int {
	return m.height - m.chromeLines() - m.pageSize() - 1
}

func (m *selectModel) clampScroll() {
	page := m.pageSize()
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+page {
		m.top = m.cursor - page + 1
	}
	if m.top < 0 {
		m.top = 0
	}
}

var (
	selTitle  = lg(HueID).Bold(true)              // cyan title, like table titles
	selHeader = lg(HueID)                         // cyan column header
	selRow    = lipgloss.NewStyle().Reverse(true) // cursor row (theme-safe)
	selMark   = lg(HueOK)                         // green ◉ selected
	selFoot   = lg(HueDim)                        // dim keybinds
	selFilter = lg(HueWarn)                       // yellow filter
	selBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(HueDim)).Padding(0, 1) // dim rounded frame
)

func (m selectModel) View() tea.View {
	if m.detailLoading {
		return tea.NewView(selFoot.Render("Loading detail" + glyph("…", "...")))
	}
	if m.detail != "" {
		back := glyph("↩", "<") + " press any key to go back"
		return tea.NewView(m.detail + "\n" + selFoot.Render(back))
	}
	w := m.colWidths()
	last := len(w) - 1
	inner := m.width - 4 // rounded border (2) + padding (2)
	if inner < 24 {
		inner = 24
	}
	// The last column absorbs the leftover width: prefix (4) + gaps + fixed columns.
	fixed := 4 + last // 4-char cursor+mark prefix + one space between each column
	for i := 0; i < last; i++ {
		fixed += w[i]
	}
	lastW := inner - fixed
	if lastW < 8 {
		lastW = 8
	}

	lines := []string{selHeader.Render("    " + m.renderCells(m.spec.Columns, nil, w, last, lastW, false))}
	page := m.pageSize()
	end := min(m.top+page, len(m.visible))
	for i := m.top; i < end; i++ {
		idx := m.visible[i]
		r := m.rows[idx]
		markGlyph := glyph("○", " ")
		if m.selected[r.ID] {
			markGlyph = glyph("◉", "*")
		}
		if m.spec.ReadOnly || m.spec.PickOne {
			markGlyph = " " // viewer / single-pick: keep the column width, show no mark
		}
		cur := cursorGlyph(i == m.cursor)
		if i == m.cursor {
			// Reverse-video the whole plain line — clearest highlight, so skip
			// per-column color (nested ANSI under reverse muddies it).
			plain := cur + " " + markGlyph + " " + m.renderCells(r.Cells, nil, w, last, lastW, false)
			lines = append(lines, selRow.Render(plain))
			continue
		}
		mark := markGlyph
		if m.selected[r.ID] {
			mark = selMark.Render(markGlyph)
		}
		lines = append(lines, cur+" "+mark+" "+m.renderCells(r.Cells, r.Hues, w, last, lastW, true))
	}

	dot := glyph(" · ", " - ")
	var title string
	if m.spec.ReadOnly {
		title = m.spec.Title
		if title == "" {
			title = "View"
		}
	} else if m.spec.PickOne {
		title = "Select to " + m.spec.Verb // cursor is the selection — no mark count
	} else {
		title = "Select to " + m.spec.Verb + dot + fmt.Sprintf("%d selected", m.countSelected())
	}
	if len(m.visible) > page {
		title += dot + fmt.Sprintf("%d%s%d of %d", m.top+1, glyph("–", "-"), end, len(m.visible))
	}
	box := selBox
	if asciiMode() {
		box = box.Border(asciiBorder) // PuTTY-safe frame on non-UTF-8 terminals
	}
	out := selTitle.Render(title) + "\n" + box.Render(strings.Join(lines, "\n"))
	if pane := m.previewPane(); pane != "" {
		out += "\n" + pane
	}
	if m.filtering {
		out += "\n" + selFilter.Render("/"+m.filter+glyph("▏", "|"))
	}
	out += "\n" + selFoot.Render(m.footer(dot))
	return tea.NewView(out)
}

// previewPane renders the live pane under the list: a dim rule, then the cursor row's
// Preview text hard-wrapped to the terminal width and cropped to the line budget
// (the `i` overlay has the full text). "" hides the pane (no Preview / no budget / no rows).
func (m selectModel) previewPane() string {
	budget := m.previewBudget()
	if !m.spec.Preview || budget < 3 || m.cursor >= len(m.visible) {
		return ""
	}
	text := m.rows[m.visible[m.cursor]].Preview
	w := max(m.width, 20)
	rule := selFoot.Render(strings.Repeat(glyph("─", "-"), w))
	lines := wrapLines(strings.TrimRight(text, "\n"), w)
	if len(lines) > budget-1 {
		ind := glyph("…", "...")
		if m.spec.Detail != nil {
			ind += " i full text"
		}
		lines = append(lines[:budget-2], selFoot.Render(ind))
	}
	return rule + "\n" + strings.Join(lines, "\n")
}

// wrapLines hard-wraps text to w columns (rune-counted) so a long body line can't
// blow the pane's height budget via terminal soft-wrap.
func wrapLines(text string, w int) []string {
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		r := []rune(ln)
		if len(r) == 0 {
			out = append(out, "")
			continue
		}
		for len(r) > w {
			out = append(out, string(r[:w]))
			r = r[w:]
		}
		out = append(out, string(r))
	}
	return out
}

// footer builds the dim keybind hint line: viewer keys (move/filter/[info]/quit) in
// ReadOnly mode, the full select-and-act keys otherwise. dot is the pre-rendered
// separator (glyph or ASCII). facetHint and the `i` info key are shared by both modes.
func (m selectModel) footer(dot string) string {
	move := glyph("↑↓", "u/d") + " move"
	info := func(s string) string {
		if m.spec.Detail != nil {
			return s + dot + "i info"
		}
		return s
	}
	if m.spec.ReadOnly {
		return info(move+dot+"/ filter"+m.facetHint(dot)) + dot + "q quit"
	}
	foot := move + dot + "space select" + dot + "a all" + dot + "/ filter" + m.facetHint(dot)
	if m.spec.PickOne {
		foot = move + dot + "/ filter" + m.facetHint(dot)
	}
	return info(foot) + dot + "enter " + m.spec.Verb + dot + "q cancel"
}

// renderCells pads each cell to its column width and colors it by its hue (unless
// colored is false, e.g. the reverse-video cursor row). The last column is cropped
// to lastW behind a trailing … so a wide field never wraps the row.
func (m selectModel) renderCells(cells, hues []string, w []int, last, lastW int, colored bool) string {
	parts := make([]string, len(w))
	for i := range w {
		val := ""
		if i < len(cells) {
			val = cells[i]
		}
		width := w[i]
		if i == last {
			width = lastW
			val = cropCmd(val, width)
		} else {
			val = trunc(val, width)
		}
		cell := fmt.Sprintf("%-*s", width, val)
		if colored && i < len(hues) && hues[i] != "" {
			cell = lg(hues[i]).Render(cell)
		}
		parts[i] = cell
	}
	return strings.Join(parts, " ")
}

func (m selectModel) countSelected() int {
	n := 0
	for _, v := range m.selected {
		if v {
			n++
		}
	}
	return n
}

// colWidths sizes each column to its header + widest visible cell, capping every
// column but the last at 20 so one wide field doesn't starve the last column (which
// absorbs the leftover width in View).
func (m selectModel) colWidths() []int {
	w := make([]int, len(m.spec.Columns))
	for i, h := range m.spec.Columns {
		w[i] = len(h)
	}
	for _, idx := range m.visible {
		for i, c := range m.rows[idx].Cells {
			if i < len(w) {
				w[i] = max(w[i], len(c))
			}
		}
	}
	for i := 0; i < len(w)-1; i++ {
		w[i] = min(w[i], 20)
	}
	return w
}

func cursorGlyph(on bool) string {
	if on {
		return glyph("▸", ">")
	}
	return " "
}

// asciiBorder is a PuTTY-safe box for non-UTF-8 terminals (overrides only the
// border runes on selBox, keeping its dim color + padding).
var asciiBorder = lipgloss.Border{
	Top: "-", Bottom: "-", Left: "|", Right: "|",
	TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+",
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// cropCmd truncates s to n columns behind a trailing … (or "..." in ASCII mode, so
// the ellipsis doesn't mojibake on a non-UTF-8 PuTTY).
func cropCmd(s string, n int) string {
	if !asciiMode() {
		return truncRight(s, n)
	}
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// Interactive reports whether stdin is a terminal, so the CLI can fall back to a
// clear error instead of a bubbletea crash when piped.
func Interactive() bool {
	return term.IsTerminal(os.Stdin.Fd())
}
