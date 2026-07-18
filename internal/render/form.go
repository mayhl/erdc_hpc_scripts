package render

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// FieldKind selects a form field's editor: free text, an enum cycled through fixed
// options, or (Editor only) a string list / string map edited in a focused sub-panel.
// A FieldList/FieldMap value stays the raw TOML literal in Value — the sub-editor only
// parses it on entry and re-serializes on exit, so the caller's write-back is unchanged.
type FieldKind int

const (
	FieldText FieldKind = iota
	FieldEnum
	FieldList // ["a", "b"] — edited as a list of strings
	FieldMap  // { k = "v" } — edited as key = value pairs
)

// FormField is one editable field of a Form. Domain-free — the caller maps values
// in and out; Validate carries any domain rule as a closure.
type FormField struct {
	Label   string
	Value   string // initial value; for FieldEnum, one of Options (else Options[0])
	Kind    FieldKind
	Options []string // FieldEnum: the cycle values
	Hint    string   // dim note rendered after the value (e.g. a unit or an example)
	// Validate returns "" to accept, else the message shown under the field. It sees
	// every field's current value (all, ordered as Fields) so cross-field rules work —
	// e.g. a walltime cap that depends on the selected queue. nil = always valid.
	Validate func(value string, all []string) string
}

// FieldPatch is a late update to one field, delivered by FormSpec.Load — live data
// arriving into an already-usable form (e.g. real queue names once a fetch lands).
// Zero parts are kept: nil Options / "" Value / "" Hint / nil Validate leave that
// part of the field untouched.
type FieldPatch struct {
	Index    int
	Options  []string
	Value    string
	Hint     string
	Validate func(value string, all []string) string
}

// FormSpec configures the editable form: a titled panel of fields the user walks
// with tab/arrows and submits with F2/ctrl+s (or enter on the last field).
type FormSpec struct {
	Title  string
	Fields []FormField
	// Load, when set, runs ONCE off the UI loop at open (it may ssh) and its patches
	// apply when they land — the form is usable immediately, data enriches it later.
	Load func() []FieldPatch
	// LoadNote is the dim footer notice while Load is in flight ("fetching queues…").
	LoadNote string
}

// Form runs the interactive form and returns the field values (ordered as
// spec.Fields) and whether the user submitted (false = cancelled). Like Select it
// is a SELECTOR, not an actuator: the caller owns preview/confirm/act, so nothing
// destructive happens inside the modal. Submit is refused while a field is invalid
// (the cursor jumps to the first offender instead).
func Form(spec FormSpec) ([]string, bool, error) {
	res, err := tea.NewProgram(newFormModel(spec)).Run()
	if err != nil {
		return nil, false, err
	}
	m := res.(formModel)
	if !m.confirmed {
		return nil, false, nil
	}
	return m.values(), true, nil
}

// formModel is the form's private bubbletea model. The cursor field is always
// editable in place (no separate edit mode); errs holds the live validation
// message per field ("" = valid).
type formModel struct {
	spec      FormSpec
	fields    []FormField
	errs      []string
	cursor    int
	loading   bool
	confirmed bool
}

func newFormModel(spec FormSpec) formModel {
	m := formModel{
		spec:    spec,
		fields:  append([]FormField(nil), spec.Fields...),
		errs:    make([]string, len(spec.Fields)),
		loading: spec.Load != nil,
	}
	for i := range m.fields {
		f := &m.fields[i]
		if f.Kind == FieldEnum && f.Value == "" && len(f.Options) > 0 {
			f.Value = f.Options[0]
		}
	}
	m.validateAll()
	return m
}

// formPatchMsg carries Load's patches back onto the UI loop.
type formPatchMsg struct{ patches []FieldPatch }

func (m formModel) Init() tea.Cmd {
	if m.spec.Load == nil {
		return nil
	}
	load := m.spec.Load
	return func() tea.Msg { return formPatchMsg{load()} }
}

func (m formModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case formPatchMsg:
		m.loading = false
		for _, p := range msg.patches {
			m.apply(p)
		}
		m.validateAll()
		return m, nil
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.confirmed = false
			return m, tea.Quit
		case saveKey, altSaveKey:
			return m.submit()
		case "enter":
			if m.cursor == len(m.fields)-1 {
				return m.submit()
			}
			m.cursor++
		case "up", "shift+tab":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "tab":
			if m.cursor < len(m.fields)-1 {
				m.cursor++
			}
		default:
			m.edit(msg)
		}
	}
	return m, nil
}

// submit validates everything; all clean → quit confirmed, else jump to the first
// invalid field so the red message is in view.
func (m formModel) submit() (tea.Model, tea.Cmd) {
	m.validateAll()
	for i, e := range m.errs {
		if e != "" {
			m.cursor = i
			return m, nil
		}
	}
	m.confirmed = true
	return m, tea.Quit
}

// edit applies a keystroke to the cursor field: enum fields cycle on ←/→/space,
// text fields take runes + backspace. The field revalidates on every change.
func (m *formModel) edit(msg tea.KeyPressMsg) {
	f := &m.fields[m.cursor]
	switch f.Kind {
	case FieldEnum:
		switch msg.String() {
		case "right", " ", "space":
			f.Value = m.cycle(f, 1)
		case "left":
			f.Value = m.cycle(f, -1)
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

// cycle steps an enum field's value through its options (wrapping); a value not in
// the list (a pre-seeded literal) restarts at the first option. Shared with Editor,
// whose leaves are the same FormFields.
func (m *formModel) cycle(f *FormField, dir int) string { return cycleOption(f, dir) }

func cycleOption(f *FormField, dir int) string {
	n := len(f.Options)
	if n == 0 {
		return f.Value
	}
	for i, o := range f.Options {
		if o == f.Value {
			return f.Options[((i+dir)%n+n)%n]
		}
	}
	return f.Options[0]
}

// apply merges one patch into its field (zero parts keep the current value); a
// patched Value must exist in the (possibly patched) options to take, so a stale
// Load can't select something the user can no longer cycle to.
func (m *formModel) apply(p FieldPatch) {
	if p.Index < 0 || p.Index >= len(m.fields) {
		return
	}
	f := &m.fields[p.Index]
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
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func (m *formModel) validate(i int) {
	if f := m.fields[i]; f.Validate != nil {
		m.errs[i] = f.Validate(f.Value, m.values())
	} else {
		m.errs[i] = ""
	}
}

func (m *formModel) validateAll() {
	for i := range m.fields {
		m.validate(i)
	}
}

func (m formModel) values() []string {
	out := make([]string, len(m.fields))
	for i, f := range m.fields {
		out[i] = f.Value
	}
	return out
}

var formErr = lg(HueErr) // red validation message

func (m formModel) View() tea.View {
	labelW := 0
	for _, f := range m.fields {
		labelW = max(labelW, len(f.Label))
	}
	var lines []string
	for i, f := range m.fields {
		cur := i == m.cursor
		val := f.Value
		if f.Kind == FieldEnum {
			if cur {
				val = glyph("‹ ", "< ") + val + glyph(" ›", " >")
			}
		} else if cur {
			val += glyph("▏", "|") // text cursor bar
		}
		if val == "" {
			val = " "
		}
		line := cursorGlyph(cur) + " " + selHeader.Render(padRight(f.Label, labelW)) + "  "
		if cur {
			line += selRow.Render(val)
		} else {
			line += val
		}
		if f.Hint != "" {
			line += "  " + selFoot.Render(f.Hint)
		}
		lines = append(lines, line)
		if m.errs[i] != "" {
			lines = append(lines, "  "+formErr.Render(glyph("✗ ", "x ")+m.errs[i]))
		}
	}

	box := selBox
	if asciiMode() {
		box = box.Border(asciiBorder)
	}
	dot := glyph(" · ", " - ")
	foot := glyph("↑↓", "u/d") + "/tab move" + dot + "type to edit" + dot +
		glyph("←→", "l/r") + " cycle" + dot + strings.Replace(saveHint, "save", "submit", 1) + dot + "esc cancel"
	if m.loading && m.spec.LoadNote != "" {
		foot += dot + m.spec.LoadNote
	}
	out := selTitle.Render(m.spec.Title) + "\n" + box.Render(strings.Join(lines, "\n")) +
		"\n" + selFoot.Render(foot)
	return tea.NewView(out)
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
