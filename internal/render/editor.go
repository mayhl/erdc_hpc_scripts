package render

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// actionKey arms a section's structural command (ctrl+x — a guarded chord, since the action
// rewrites structure, not just a value).
const actionKey = "ctrl+x"

// EditorNode is one row of the Editor's tree: a SECTION when it has Children and no Field,
// a LEAF when it carries a Field. Domain-free — the caller builds the tree and reads the
// Changes back out; the widget knows nothing about config.
type EditorNode struct {
	Label string
	// Key is this node's IDENTITY in a path — what Change.Path and EditorPatch.Path are
	// built from — defaulting to Label. Set it whenever the label is decorated for display
	// ("[[cluster]] alpha" for the cluster `alpha`): a caller keys its write-back targets by
	// the real name, and if the path carried the decoration instead, every lookup would miss
	// and the edit would vanish with no error.
	Key    string
	Field  *FormField // nil → a section (a heading you expand), else an editable leaf
	Origin string     // dim provenance note ("from dsrc1", "unset", …); leaves only
	// Hue is the palette hue of a SECTION's heading (HueLoc, HueGroup, …), letting the
	// caller tier its own hierarchy without the widget knowing what a cluster is. "" → the
	// house header hue. Warm hues are status-reserved — don't pass them.
	Hue string
	// Action, when set on a SECTION, is a structural command the caller offers on it: ctrl+x
	// marks the section (the label names it, e.g. "decommission"), and Editor returns the
	// marked sections' paths in Actions on save for the caller to run. The widget stays
	// domain-free — it never performs the action, only reports the intent, like Changes.
	Action   string
	Children []EditorNode
}

// Action is one section the user marked for its structural command (its path from the root
// and the command's label). Returned alongside Changes so the caller can run the domain
// operation — dropping a node's block, say — that the tree can't express as a leaf edit.
type Action struct {
	Path  []string
	Label string
}

// isCollection reports the field kinds edited in the focused sub-panel rather than in place.
func isCollection(k FieldKind) bool { return k == FieldList || k == FieldMap || k == FieldSet }

// EditorSpec configures the panel: a titled tree whose leaves are edited in place with the
// same field editors as Form.
type EditorSpec struct {
	Title string
	Root  []EditorNode
	// Load, when set, runs ONCE off the UI loop at open (it may ssh) and its patches apply
	// when they land — the panel is usable immediately and enriches later, as in Form.
	Load     func() []EditorPatch
	LoadNote string // dim footer notice while Load is in flight
	// Collapsed opens every section folded, so a deep tree (config's cluster→node→keys)
	// presents as a short list of headings you expand into, rather than a wall.
	Collapsed bool
}

// EditorPatch is a late update to one leaf, addressed by its path (not an index — a tree's
// leaf order is an implementation detail the caller shouldn't have to track). Zero parts
// are kept, exactly as FieldPatch.
type EditorPatch struct {
	Path     []string
	Options  []string
	Value    string
	Hint     string
	Validate func(value string, all []string) string
}

// Change is one leaf the user actually edited: its path from the root, and the values on
// either side of the edit. Unchanged leaves are never reported.
type Change struct {
	Path     []string
	Old, New string
}

// Editor runs the interactive panel and returns the changed leaves, the sections the user
// marked for their structural action, and whether the user saved (false = cancelled). Like
// Select and Form it is a SELECTOR, not an actuator: it returns intent and never touches disk
// — the caller renders the diff, confirms, and writes. Save is refused while any leaf is
// invalid (the cursor jumps to the offender).
func Editor(spec EditorSpec) ([]Change, []Action, bool, error) {
	res, err := tea.NewProgram(newEditorModel(spec)).Run()
	if err != nil {
		return nil, nil, false, err
	}
	m := res.(editorModel)
	if !m.saved {
		return nil, nil, false, nil
	}
	return m.changes(), m.actions(), true, nil
}

// edRow is the tree flattened once at open: rendering and navigation both walk this slice,
// with expanded gating which rows are visible. A tree that never changes shape doesn't need
// to be re-walked.
type edRow struct {
	label    string
	depth    int
	path     []string
	field    *FormField // nil → section
	origin   string
	hue      string
	orig     string // value at open, for the Change diff
	err      string
	expanded bool
	kids     int    // number of DESCENDANT rows (the whole subtree), for collapse skipping
	action   string // a section's structural command label ("" → none), from EditorNode.Action
	marked   bool   // the user armed this section's action (ctrl+x); applied on save
}

type editorModel struct {
	spec    EditorSpec
	rows    []edRow
	cursor  int // index into rows (always a visible row)
	top     int // scroll offset, in VISIBLE rows — a fold changes what that counts
	height  int // terminal rows; a config tree outgrows a short window easily
	loading bool
	saved   bool
	// sub is the focused list/map sub-editor over the cursor leaf while it is open: it owns
	// every keystroke, and on done writes its re-serialized literal back into the leaf.
	sub *collEditor
}

func newEditorModel(spec EditorSpec) editorModel {
	m := editorModel{spec: spec, loading: spec.Load != nil, height: 24}
	m.rows = flatten(spec.Root, 0, nil, !spec.Collapsed)
	for i := range m.rows {
		r := &m.rows[i]
		if f := r.field; f != nil {
			if f.Kind == FieldEnum && f.Value == "" && len(f.Options) > 0 {
				f.Value = f.Options[0]
			}
			r.orig = f.Value
		}
	}
	m.validateAll()
	m.cursor = m.firstVisible()
	m.clampScroll()
	return m
}

// pageSize is how many rows fit inside the box: the window minus the chrome around it —
// title, the border's two rules, and the footer.
func (m editorModel) pageSize() int {
	if p := m.height - 4; p > 1 {
		return p
	}
	return 1
}

// visibleRows is the rows the folds leave on screen, in order — the sequence the scroll
// window and the cursor both index into.
func (m editorModel) visibleRows() []int {
	var out []int
	for i := range m.rows {
		if m.visible(i) {
			out = append(out, i)
		}
	}
	return out
}

// windowEnd is the first visible row that no longer fits a page starting at top. An invalid
// leaf costs two lines, not one (its complaint renders under it), so the window is filled by
// cost rather than counted — otherwise a few validation errors push the footer off-screen,
// which is exactly when you can least afford to lose it.
func (m editorModel) windowEnd(vis []int, top int) int {
	page, used := m.pageSize(), 0
	for i := top; i < len(vis); i++ {
		cost := 1
		if m.rows[vis[i]].err != "" {
			cost = 2
		}
		if used+cost > page && i > top { // always show at least the top row
			return i
		}
		used += cost
	}
	return len(vis)
}

// clampScroll scrolls the window the least it can to keep the cursor inside it.
func (m *editorModel) clampScroll() {
	vis := m.visibleRows()
	pos := 0
	for i, r := range vis {
		if r == m.cursor {
			pos = i
			break
		}
	}
	if pos < m.top {
		m.top = pos
	}
	for m.top < pos && pos >= m.windowEnd(vis, m.top) {
		m.top++
	}
	if m.top < 0 || m.top >= len(vis) {
		m.top = 0
	}
}

// flatten walks the tree depth-first into rows, recording each section's descendant count
// so a collapsed section can be skipped in one jump.
func flatten(nodes []EditorNode, depth int, prefix []string, expanded bool) []edRow {
	var out []edRow
	for _, n := range nodes {
		key := n.Key
		if key == "" {
			key = n.Label
		}
		path := append(append([]string(nil), prefix...), key)
		row := edRow{label: n.Label, depth: depth, path: path, origin: n.Origin, hue: n.Hue, action: n.Action, expanded: expanded}
		if n.Field != nil {
			f := *n.Field // copy: the widget edits its own state, never the caller's spec
			row.field = &f
		}
		at := len(out)
		out = append(out, row)
		kids := flatten(n.Children, depth+1, path, expanded)
		out = append(out, kids...)
		out[at].kids = len(kids)
	}
	return out
}

// editorPatchMsg carries Load's patches back onto the UI loop.
type editorPatchMsg struct{ patches []EditorPatch }

func (m editorModel) Init() tea.Cmd {
	if m.spec.Load == nil {
		return nil
	}
	load := m.spec.Load
	return func() tea.Msg { return editorPatchMsg{load()} }
}

func (m editorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.clampScroll()
		return m, nil
	case editorPatchMsg:
		m.loading = false
		for _, p := range msg.patches {
			m.apply(p)
		}
		m.validateAll()
		m.clampScroll()
		return m, nil
	case tea.KeyPressMsg:
		// While a list/map sub-editor is open it swallows every key; when it finishes we
		// either fold its literal back into the leaf (done) or drop it (cancel).
		if m.sub != nil {
			m.sub.update(msg)
			switch {
			case m.sub.done:
				m.rows[m.cursor].field.Value = m.sub.literal()
				m.validate(m.cursor)
				m.sub = nil
				m.clampScroll()
			case m.sub.cancel:
				m.sub = nil
			}
			return m, nil
		}
		switch key := msg.String(); key {
		case "ctrl+c", "esc":
			m.saved = false
			return m, tea.Quit
		case saveKey, altSaveKey:
			return m.save()
		case "up", "shift+tab":
			m.move(-1)
		case "down", "tab":
			m.move(1)
		case "pgup":
			m.page(-1)
		case "pgdown":
			m.page(1)
		case "home":
			m.cursor = m.firstVisible()
		case "end":
			m.lastVisible()
		case actionKey:
			// Arm/disarm this section's structural action (a leaf has none). Applied on save.
			if r := &m.rows[m.cursor]; r.action != "" {
				r.marked = !r.marked
			}
		case "enter", "right", "left", " ", "space":
			// On a section these fold; on a leaf the same keys belong to the field editor
			// (an enum cycles with ←/→/space), so only sections consume them.
			if r := &m.rows[m.cursor]; r.field == nil {
				switch key {
				case "right":
					r.expanded = true
				case "left":
					r.expanded = false
				default: // enter/space toggle
					r.expanded = !r.expanded
				}
				m.clampScroll() // a fold resizes the list under the window
				return m, nil
			}
			// A list/map/set leaf opens its focused sub-editor on enter/→; ←/space do nothing on
			// it (there is no scalar to cycle or type in place).
			if f := m.rows[m.cursor].field; isCollection(f.Kind) {
				if key == "enter" || key == "right" {
					m.sub = newCollEditor(f.Kind, f.Label, f.Value, f.Options)
				}
				return m, nil
			}
			m.edit(msg)
		default:
			if m.rows[m.cursor].field != nil {
				m.edit(msg)
			}
		}
		m.clampScroll() // an edit can raise or clear an error line, changing what fits
	}
	return m, nil
}

// page jumps the cursor a windowful at a time — a config tree is deep enough that stepping
// through it one row at a time is its own annoyance.
func (m *editorModel) page(dir int) {
	for range m.pageSize() {
		before := m.cursor
		m.move(dir)
		if m.cursor == before {
			return // hit an end
		}
	}
}

// lastVisible parks the cursor on the final unfolded row.
func (m *editorModel) lastVisible() {
	vis := m.visibleRows()
	if len(vis) > 0 {
		m.cursor = vis[len(vis)-1]
	}
}

// move steps the cursor to the next visible row in a direction, skipping the subtrees of
// collapsed sections.
func (m *editorModel) move(dir int) {
	for i := m.cursor + dir; i >= 0 && i < len(m.rows); i += dir {
		if m.visible(i) {
			m.cursor = i
			return
		}
	}
}

// visible reports whether row i is shown — i.e. no ancestor of it is collapsed. Walking
// back over the recorded subtree sizes answers this without a parent pointer.
func (m editorModel) visible(i int) bool {
	for j := 0; j < i; j++ {
		if !m.rows[j].expanded && j+m.rows[j].kids >= i {
			return false
		}
	}
	return true
}

func (m editorModel) firstVisible() int {
	for i := range m.rows {
		if m.visible(i) {
			return i
		}
	}
	return 0
}

// save refuses while any leaf is invalid, expanding whatever hides the first offender and
// putting the cursor on it — an error the user can't see is an error they can't fix.
func (m editorModel) save() (tea.Model, tea.Cmd) {
	m.validateAll()
	for i, r := range m.rows {
		if r.err == "" {
			continue
		}
		for j := 0; j < i; j++ {
			if j+m.rows[j].kids >= i {
				m.rows[j].expanded = true
			}
		}
		m.cursor = i
		m.clampScroll() // the offender is useless if it's off-screen
		return m, nil
	}
	m.saved = true
	return m, tea.Quit
}

// edit applies a keystroke to the cursor leaf — the same grammar as Form: enums cycle on
// ←/→/space, text takes runes and backspace.
func (m *editorModel) edit(msg tea.KeyPressMsg) {
	f := m.rows[m.cursor].field
	switch f.Kind {
	case FieldEnum:
		switch msg.String() {
		case "right", " ", "space":
			f.Value = cycleOption(f, 1)
		case "left":
			f.Value = cycleOption(f, -1)
		default:
			return
		}
	case FieldText:
		switch msg.String() {
		case "backspace":
			if r := []rune(f.Value); len(r) > 0 {
				f.Value = string(r[:len(r)-1])
			}
		default:
			if t := msg.Key().Text; t != "" {
				f.Value += t
			} else {
				return
			}
		}
	}
	m.validate(m.cursor)
}

// apply merges a patch into the leaf at its path (zero parts keep the current value). A
// patched Value must be reachable in the options, as in Form — a stale Load can't select
// something the user can no longer cycle to.
func (m *editorModel) apply(p EditorPatch) {
	for i := range m.rows {
		r := &m.rows[i]
		if r.field == nil || !samePath(r.path, p.Path) {
			continue
		}
		f := r.field
		if p.Options != nil {
			f.Options = p.Options
		}
		if p.Hint != "" {
			f.Hint = p.Hint
		}
		if p.Validate != nil {
			f.Validate = p.Validate
		}
		if p.Value != "" && (f.Kind != FieldEnum || contains(f.Options, p.Value)) {
			f.Value = p.Value
			// A value that arrives from Load is the file's truth, not an edit — otherwise
			// every async fill would show up as a pending change.
			r.orig = p.Value
		}
		return
	}
}

func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *editorModel) validate(i int) {
	r := &m.rows[i]
	if r.field == nil || r.field.Validate == nil {
		r.err = ""
		return
	}
	r.err = r.field.Validate(r.field.Value, m.leafValues())
}

func (m *editorModel) validateAll() {
	for i := range m.rows {
		m.validate(i)
	}
}

// leafValues is every leaf's value in row order — the `all` a FormField.Validate closure
// sees, so a cross-field rule can reach its neighbours.
func (m editorModel) leafValues() []string {
	var out []string
	for _, r := range m.rows {
		if r.field != nil {
			out = append(out, r.field.Value)
		}
	}
	return out
}

// changes reports the leaves whose value actually moved.
func (m editorModel) changes() []Change {
	var out []Change
	for _, r := range m.rows {
		if r.field != nil && r.field.Value != r.orig {
			out = append(out, Change{Path: r.path, Old: r.orig, New: r.field.Value})
		}
	}
	return out
}

// actions reports the sections the user armed for their structural command.
func (m editorModel) actions() []Action {
	var out []Action
	for _, r := range m.rows {
		if r.marked && r.action != "" {
			out = append(out, Action{Path: r.path, Label: r.action})
		}
	}
	return out
}

func (m editorModel) View() tea.View {
	if m.sub != nil { // the sub-editor takes the whole panel while it's open
		return tea.NewView(m.sub.view(m.spec.Title))
	}
	labelW := 0
	for i, r := range m.rows {
		if r.field != nil && m.visible(i) {
			labelW = max(labelW, len(r.label)+2*r.depth)
		}
	}
	vis := m.visibleRows()
	end := m.windowEnd(vis, m.top)
	var lines []string
	for _, i := range vis[min(m.top, len(vis)):end] {
		r := m.rows[i]
		cur := i == m.cursor
		indent := strings.Repeat("  ", r.depth)
		if r.field == nil {
			fold := glyph("▾ ", "- ")
			if !r.expanded {
				fold = glyph("▸ ", "+ ")
			}
			head := selHeader
			if r.hue != "" {
				head = lg(r.hue).Bold(true)
			}
			line := cursorGlyph(cur) + " " + indent + head.Render(fold+r.label)
			switch {
			case r.marked: // armed — the caller will run this on save
				line += "  " + formErr.Render(glyph("→ ", "-> ")+r.action)
			case cur && r.action != "": // offer it while the cursor is here
				line += "  " + selFoot.Render(actionKey+" "+r.action)
			}
			lines = append(lines, line)
			continue
		}
		// An UNSET leaf is dim and shows a dash: an empty cell and a real empty string look
		// identical otherwise, and here the difference decides whether a key gets written.
		val, style := r.field.Value, edValue
		if strings.TrimSpace(val) == "" && !cur {
			val, style = glyph("—", "--"), edUnset
		}
		coll := r.field.Kind == FieldList || r.field.Kind == FieldMap
		switch {
		case r.field.Kind == FieldEnum && cur:
			val = glyph("‹ ", "< ") + val + glyph(" ›", " >")
		case coll:
			// no text bar — a collection leaf is opened, not typed into; the literal shows as-is
		case cur:
			val += glyph("▏", "|")
		}
		if strings.TrimSpace(val) == "" {
			val = " "
		}
		line := cursorGlyph(cur) + " " + edKey.Render(padRight(indent+r.label, labelW)) + "  "
		if cur {
			line += selRow.Render(val)
		} else {
			line += style.Render(val)
		}
		if coll && cur { // the affordance: this leaf opens a panel
			line += "  " + selFoot.Render(glyph("↵ edit", "enter edit"))
		}
		// Provenance is why this panel beats the file: which scope a value came from is
		// invisible in the TOML but decides what a lookup resolves to. The caller writes the
		// note (and any marker glyph) — meaning rides the glyph, not a colour, so this stays
		// dim chrome either way.
		if r.origin != "" {
			line += "  " + selFoot.Render(r.origin)
		}
		if r.field.Hint != "" {
			line += "  " + selFoot.Render(r.field.Hint)
		}
		lines = append(lines, line)
		if r.err != "" {
			lines = append(lines, "  "+formErr.Render(glyph("✗ ", "x ")+r.err))
		}
	}

	box := selBox
	if asciiMode() {
		box = box.Border(asciiBorder)
	}
	dot := glyph(" · ", " - ")
	title := m.spec.Title
	if end-m.top < len(vis) {
		title += dot + fmt.Sprintf("%d%s%d of %d", m.top+1, glyph("–", "-"), end, len(vis))
	}
	foot := glyph("↑↓", "u/d") + " move" + dot + "type to edit" + dot +
		glyph("←→", "l/r") + " fold/cycle" + dot + saveHint + dot + "esc cancel"
	if m.loading && m.spec.LoadNote != "" {
		foot += dot + m.spec.LoadNote
	}
	out := selTitle.Render(title) + "\n" + box.Render(strings.Join(lines, "\n")) +
		"\n" + selFoot.Render(foot)
	return tea.NewView(out)
}

var (
	edKey   = lg(HueID)   // cyan key labels, matching Form's field labels
	edValue = lg(HueName) // white — the value is the content, so it carries the plain hue
	edUnset = lg(HueDim)  // a key the file doesn't set
	edOn    = lg(HueLoc)  // blue — a checked set member (nodes are blue in the palette)
)
