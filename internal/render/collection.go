package render

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// collEditor is the focused sub-panel that edits a FieldList, FieldMap or FieldSet leaf: a
// small list of rows the user walks. It owns no disk state — it parses the leaf's TOML
// literal on open and re-serializes it on done, so the Editor's Change/write-back path is
// identical to the old raw-text edit; the widget only stops you fat-fingering the brackets
// and quotes. A FieldSet is a subset TOGGLED out of a fixed universe (Options): no free text,
// just space to check/uncheck — used for fleet, whose members are the fleet's known machines.
//
// Modeless typing collides with row commands (a node named "node-d" versus `d` = delete),
// so the free list/map IS modal: nav mode walks rows (a add, d remove, ← → pick a map column,
// ↵ edit the cell), and an explicit edit mode takes the keystrokes. It's the one place the
// house's modeless rule doesn't fit — the collision is real, not stylistic. A FieldSet has no
// typing, so it stays modeless.
type collEditor struct {
	kind    FieldKind // FieldList, FieldMap or FieldSet
	label   string
	rows    []collRow
	cursor  int // 0..len(rows); == len(rows) is the "+ add" row (list/map only)
	col     int // map only: 0 = key, 1 = value
	editing bool
	done    bool // F2/ctrl+s — write the literal back
	cancel  bool // esc — discard, leave the leaf untouched
}

// collRow is one row: for a list its val; a map its key=val; a set its val + whether it's on.
type collRow struct {
	key, val string
	on       bool // set only
}

func (e *collEditor) isMap() bool { return e.kind == FieldMap }
func (e *collEditor) isSet() bool { return e.kind == FieldSet }

// newCollEditor seeds the rows from a leaf's literal (an empty/unset leaf opens empty). For a
// FieldSet the rows are the universe (options) with the literal's members pre-checked, plus
// any current member outside the universe appended checked — so a stale name isn't silently
// dropped just because it left the cluster config.
func newCollEditor(kind FieldKind, label, lit string, options []string) *collEditor {
	e := &collEditor{kind: kind, label: label}
	switch kind {
	case FieldMap:
		for _, p := range parseMap(lit) {
			e.rows = append(e.rows, collRow{key: p[0], val: p[1]})
		}
	case FieldSet:
		chosen := map[string]bool{}
		for _, v := range parseList(lit) {
			chosen[v] = true
		}
		seen := map[string]bool{}
		for _, o := range options {
			if !seen[o] {
				seen[o] = true
				e.rows = append(e.rows, collRow{val: o, on: chosen[o]})
			}
		}
		for _, v := range parseList(lit) { // current members no longer in the universe
			if !seen[v] {
				seen[v] = true
				e.rows = append(e.rows, collRow{val: v, on: true})
			}
		}
	default: // FieldList
		for _, v := range parseList(lit) {
			e.rows = append(e.rows, collRow{val: v})
		}
	}
	return e
}

// cell is the string the cursor edits — the key or value of the focused row, or nil on the
// add row (which has no cell to type into).
func (e *collEditor) cell() *string {
	if e.cursor < 0 || e.cursor >= len(e.rows) {
		return nil
	}
	r := &e.rows[e.cursor]
	if e.isMap() && e.col == 0 {
		return &r.key
	}
	return &r.val
}

func (e *collEditor) addRow() {
	e.rows = append(e.rows, collRow{})
	e.cursor = len(e.rows) - 1
	e.col = 0
	e.editing = true // land straight in the new cell — adding then typing is one motion
}

func (e *collEditor) delRow() {
	if e.cursor >= len(e.rows) {
		return
	}
	e.rows = append(e.rows[:e.cursor], e.rows[e.cursor+1:]...)
	if e.cursor > len(e.rows) {
		e.cursor = len(e.rows)
	}
}

// update applies one keystroke. A FieldSet only toggles (no rows added/removed/typed); the
// free list/map is modal — in edit mode every rune lands in the cell (so a value may contain
// any letter), and nav mode reads a/d/arrows as commands.
func (e *collEditor) update(msg tea.KeyPressMsg) {
	key := msg.String()
	if e.isSet() {
		switch key {
		case "esc", "ctrl+c":
			e.cancel = true
		case saveKey, altSaveKey:
			e.done = true
		case "up", "shift+tab":
			if e.cursor > 0 {
				e.cursor--
			}
		case "down", "tab":
			if e.cursor < len(e.rows)-1 {
				e.cursor++
			}
		case " ", "space", "enter", "x":
			if e.cursor < len(e.rows) {
				e.rows[e.cursor].on = !e.rows[e.cursor].on
			}
		}
		return
	}
	if e.editing {
		switch key {
		case "enter", "tab", "esc":
			e.editing = false // commit the cell, back to nav (esc keeps the typing — forgiving)
		case saveKey, altSaveKey:
			e.editing, e.done = false, true
		case "backspace":
			if c := e.cell(); c != nil {
				if r := []rune(*c); len(r) > 0 {
					*c = string(r[:len(r)-1])
				}
			}
		default:
			if t := msg.Key().Text; t != "" {
				if c := e.cell(); c != nil {
					*c += t
				}
			}
		}
		return
	}
	switch key {
	case "esc", "ctrl+c":
		e.cancel = true
	case saveKey, altSaveKey:
		e.done = true
	case "up", "shift+tab":
		if e.cursor > 0 {
			e.cursor--
		}
	case "down", "tab":
		if e.cursor < len(e.rows) {
			e.cursor++
		}
	case "left":
		if e.isMap() {
			e.col = 0
		}
	case "right":
		if e.isMap() {
			e.col = 1
		}
	case "a":
		e.addRow()
	case "d":
		e.delRow()
	case "enter":
		if e.cursor == len(e.rows) {
			e.addRow()
		} else {
			e.editing = true
		}
	}
}

// literal re-serializes the rows to the single-line TOML form the leaf stores. Empty rows
// (a key or item left blank) are dropped, and a wholly empty collection serializes to ""
// — which the Editor diffs as "unset", so opening and closing an unset leaf is no change.
func (e *collEditor) literal() string {
	if e.isMap() {
		pairs := make([][2]string, len(e.rows))
		for i, r := range e.rows {
			pairs[i] = [2]string{r.key, r.val}
		}
		return formatMap(pairs)
	}
	var items []string
	for _, r := range e.rows {
		if !e.isSet() || r.on { // a set contributes only its checked rows
			items = append(items, r.val)
		}
	}
	return formatList(items)
}

func (e *collEditor) view(parentTitle string) string {
	dot := glyph(" · ", " - ")
	keyW := 3
	if e.isMap() {
		for _, r := range e.rows {
			keyW = max(keyW, len(r.key))
		}
	}
	var lines []string
	for i, r := range e.rows {
		cur := i == e.cursor
		var cell string
		switch {
		case e.isSet():
			mark := glyph("☐ ", "[ ] ")
			if r.on {
				mark = glyph("☑ ", "[x] ")
			}
			// checked = blue (the node hue), unchecked = dim, so the set reads at a glance; the
			// cursor row keeps the reverse highlight. Meaning still rides the ☑/☐ glyph, not the
			// colour (per the house colour policy).
			switch {
			case cur:
				ms := edUnset
				if r.on {
					ms = edOn
				}
				cell = ms.Render(mark) + selRow.Render(r.val)
			case r.on:
				cell = edOn.Render(mark + r.val)
			default:
				cell = edUnset.Render(mark + r.val)
			}
		case e.isMap():
			cell = e.renderCell(r.key, cur && e.col == 0, keyW) + edKey.Render(" = ") +
				e.renderCell(r.val, cur && e.col == 1, 0)
		default:
			cell = e.renderCell(r.val, cur, 0)
		}
		lines = append(lines, cursorGlyph(cur)+" "+cell)
	}
	if !e.isSet() { // a set's universe is fixed — no "+ add" affordance
		add := glyph("+ ", "+ ") + "add"
		if e.isMap() {
			add += " pair"
		}
		onAdd := e.cursor == len(e.rows)
		addLine := cursorGlyph(onAdd) + " " + selFoot.Render(add)
		if onAdd {
			addLine = cursorGlyph(true) + " " + selRow.Render(add)
		}
		lines = append(lines, addLine)
	}
	if len(e.rows) == 0 && e.isSet() {
		lines = append(lines, selFoot.Render("(no machines in any [[cluster]])"))
	}

	box := selBox
	if asciiMode() {
		box = box.Border(asciiBorder)
	}
	var foot string
	switch {
	case e.isSet():
		foot = glyph("↑↓", "u/d") + " move" + dot + "space toggle" + dot + saveHint + dot + "esc cancel"
	case e.editing:
		foot = "type" + dot + glyph("↵", "enter") + " done editing" + dot + saveHint + dot + "esc"
	default:
		foot = glyph("↑↓", "u/d") + " move" + dot + glyph("↵", "enter") + " edit" + dot +
			"a add" + dot + "d remove"
		if e.isMap() {
			foot += dot + glyph("←→", "l/r") + " key/val"
		}
		foot += dot + saveHint + dot + "esc cancel"
	}
	title := parentTitle + dot + e.label
	if e.isSet() {
		on := 0
		for _, r := range e.rows {
			if r.on {
				on++
			}
		}
		title += dot + fmt.Sprintf("%d/%d selected", on, len(e.rows))
	}
	out := selTitle.Render(title) + "\n" + box.Render(strings.Join(lines, "\n")) +
		"\n" + selFoot.Render(foot)
	return out
}

// renderCell styles one key/value cell: dim dash for an empty non-focused cell (so a blank
// reads as blank), a text bar when it's the cell being typed, the row highlight otherwise.
func (e *collEditor) renderCell(v string, focused bool, pad int) string {
	editing := focused && e.editing
	style := edValue
	if strings.TrimSpace(v) == "" && !editing {
		v, style = glyph("—", "--"), edUnset
	}
	if editing {
		v += glyph("▏", "|")
	}
	if pad > 0 {
		v = padRight(v, pad)
	}
	if focused {
		return selRow.Render(v)
	}
	return style.Render(v)
}

// parseList / parseMap split a single-line TOML string-array / inline-table literal into its
// members — enough for the config's flat name arrays and string maps, not a general parser.
// unquoteLit / quoteLit handle the simple identifier-ish strings these hold (no escapes).
func parseList(lit string) []string {
	s := strings.TrimSpace(lit)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := unquoteLit(strings.TrimSpace(p)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func parseMap(lit string) [][2]string {
	s := strings.TrimSpace(lit)
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	var out [][2]string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		eq := strings.Index(p, "=")
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(p[:eq])
		if k != "" {
			out = append(out, [2]string{k, unquoteLit(strings.TrimSpace(p[eq+1:]))})
		}
	}
	return out
}

func formatList(items []string) string {
	var xs []string
	for _, it := range items {
		if s := strings.TrimSpace(it); s != "" {
			xs = append(xs, quoteLit(s))
		}
	}
	if len(xs) == 0 {
		return ""
	}
	return "[" + strings.Join(xs, ", ") + "]"
}

func formatMap(pairs [][2]string) string {
	var xs []string
	for _, p := range pairs {
		if k := strings.TrimSpace(p[0]); k != "" {
			xs = append(xs, k+" = "+quoteLit(strings.TrimSpace(p[1])))
		}
	}
	if len(xs) == 0 {
		return ""
	}
	return "{ " + strings.Join(xs, ", ") + " }"
}

func unquoteLit(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func quoteLit(s string) string { return `"` + s + `"` }
