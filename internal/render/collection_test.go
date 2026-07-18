package render

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// collKey builds the KeyPressMsg the sub-editor sees for a named key or rune (a rune arrives
// with its Text set, which is what edit mode reads).
func collKey(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "f2":
		return tea.KeyPressMsg{Code: tea.KeyF2}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func send(e *collEditor, keys ...string) {
	for _, k := range keys {
		e.update(collKey(k))
	}
}

func TestCollList(t *testing.T) {
	// parse an existing literal, delete the first item, add a new one, done.
	e := newCollEditor(FieldList, "nodes", `["node-a", "node-b"]`, nil)
	if len(e.rows) != 2 || e.rows[0].val != "node-a" {
		t.Fatalf("parse: %+v", e.rows)
	}
	send(e, "d")                // remove node-a (cursor on row 0)
	send(e, "a", "n", "e", "w") // add row, type "new"
	send(e, "f2")
	if !e.done || e.cancel {
		t.Fatalf("done=%v cancel=%v", e.done, e.cancel)
	}
	if got := e.literal(); got != `["node-b", "new"]` {
		t.Fatalf("literal = %q", got)
	}
}

func TestCollListEmptyClears(t *testing.T) {
	// deleting every item serializes to "" so the leaf reads as unset, not `[]`.
	e := newCollEditor(FieldList, "nodes", `["only"]`, nil)
	send(e, "d", "f2")
	if got := e.literal(); got != "" {
		t.Fatalf("empty list literal = %q, want empty", got)
	}
	// an unset leaf opened and closed untouched is also "".
	e2 := newCollEditor(FieldList, "nodes", "", nil)
	send(e2, "f2")
	if got := e2.literal(); got != "" {
		t.Fatalf("untouched-unset literal = %q, want empty", got)
	}
}

func TestCollMap(t *testing.T) {
	e := newCollEditor(FieldMap, "submit_queue", `{ default = "standard", debug = "debug" }`, nil)
	if len(e.rows) != 2 || e.rows[1].key != "debug" || e.rows[1].val != "debug" {
		t.Fatalf("parse: %+v", e.rows)
	}
	// edit the value of row 0 (default): enter edit mode on the value column, retype.
	send(e, "right") // focus value column
	send(e, "enter") // edit the value cell
	send(e, "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace")
	send(e, "b", "i", "g")
	send(e, "enter") // commit cell
	if got := e.literal(); got != `{ default = "big", debug = "debug" }` {
		t.Fatalf("literal = %q", got)
	}
}

func TestCollMapAddPair(t *testing.T) {
	e := newCollEditor(FieldMap, "queue_class", "", nil)
	// navigate to the add row and add a pair: key then value across the two columns.
	send(e, "a")              // add row, lands editing the key cell
	send(e, "s", "t", "d")    // key = "std"
	send(e, "enter")          // commit key, nav mode on the same row
	send(e, "right", "enter") // move to value column, edit it
	send(e, "b", "i", "g")    // value = "big"
	send(e, "f2")
	if got := e.literal(); got != `{ std = "big" }` {
		t.Fatalf("literal = %q", got)
	}
}

func TestCollCancel(t *testing.T) {
	e := newCollEditor(FieldList, "nodes", `["a"]`, nil)
	send(e, "d")   // would empty it...
	send(e, "esc") // ...but cancel discards
	if !e.cancel || e.done {
		t.Fatalf("cancel=%v done=%v", e.cancel, e.done)
	}
}

func TestCollSet(t *testing.T) {
	// universe of four machines, two currently in the fleet; toggle one off and one on.
	uni := []string{"n1", "n2", "n3", "n4"}
	e := newCollEditor(FieldSet, "fleet", `["n1", "n3"]`, uni)
	if len(e.rows) != 4 || !e.rows[0].on || e.rows[1].on || !e.rows[2].on {
		t.Fatalf("seed: %+v", e.rows)
	}
	send(e, " ")         // toggle n1 off (cursor row 0)
	send(e, "down", " ") // toggle n2 on (row 1)
	send(e, "f2")
	if got := e.literal(); got != `["n2", "n3"]` {
		t.Fatalf("literal = %q", got)
	}
}

func TestCollSetKeepsUnknownMember(t *testing.T) {
	// a fleet member no longer in any cluster still shows, checked, so it isn't silently lost.
	e := newCollEditor(FieldSet, "fleet", `["n1", "ghost"]`, []string{"n1", "n2"})
	if len(e.rows) != 3 || e.rows[2].val != "ghost" || !e.rows[2].on {
		t.Fatalf("ghost not preserved: %+v", e.rows)
	}
	send(e, "f2")
	if got := e.literal(); got != `["n1", "ghost"]` {
		t.Fatalf("literal = %q", got)
	}
}

func TestCollRoundTrip(t *testing.T) {
	for _, lit := range []string{`["a", "b", "c"]`} {
		if got := formatList(parseList(lit)); got != lit {
			t.Errorf("list round-trip %q -> %q", lit, got)
		}
	}
	if got := formatMap(parseMap(`{ a = "1", b = "2" }`)); got != `{ a = "1", b = "2" }` {
		t.Errorf("map round-trip -> %q", got)
	}
}
