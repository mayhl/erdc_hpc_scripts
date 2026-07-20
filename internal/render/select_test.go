package render

import "testing"

func row(id string, cells ...string) SelectRow { return SelectRow{ID: id, Cells: cells} }

// A live refresh must: pick up new rows, drop gone ones, and keep the ID-keyed
// selection + the cursor's ID across the rebuild.
func TestRefreshKeepsSelectionByID(t *testing.T) {
	rows1 := []SelectRow{row("100", "100", "a"), row("200", "200", "b")}
	rows2 := []SelectRow{row("200", "200", "b"), row("300", "300", "c")} // 100 gone, 300 new
	spec := SelectSpec{Verb: "kill", Columns: []string{"ID", "CMD"}, Fetch: func() []SelectRow { return rows2 }}
	m := newSelectModel(spec, rows1)

	m.selected["200"] = true
	m.cursor = 1 // on ID 200

	m.applyRows(rows2)

	if len(m.rows) != 2 || m.rows[0].ID != "200" {
		t.Fatalf("refresh didn't swap in the new snapshot: %+v", m.rows)
	}
	has := func(id string) bool {
		for _, r := range m.rows {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	if has("100") {
		t.Error("gone row 100 still present after refresh")
	}
	if !has("300") {
		t.Error("new row 300 missing after refresh")
	}
	if !m.selected["200"] {
		t.Error("selection for ID 200 lost across refresh")
	}
	if got := m.rows[m.visible[m.cursor]].ID; got != "200" {
		t.Errorf("cursor didn't follow ID 200, landed on %s", got)
	}
}

// refresh must respect an active filter (e.g. resumed after the filter box closes).
func TestRefreshHonorsFilter(t *testing.T) {
	rows := []SelectRow{row("1", "1", "funwave"), row("2", "2", "python")}
	spec := SelectSpec{Verb: "kill", Columns: []string{"ID", "CMD"}, Fetch: func() []SelectRow { return rows }}
	m := newSelectModel(spec, rows)
	m.filter = "python"
	m.recompute()
	if len(m.visible) != 1 || m.rows[m.visible[0]].ID != "2" {
		t.Fatalf("filter not applied: %+v", m.visible)
	}
	m.applyRows(rows)
	if len(m.visible) != 1 || m.rows[m.visible[0]].ID != "2" {
		t.Errorf("refresh dropped the active filter: visible=%+v", m.visible)
	}
}

// A read-only (viewer) model must render without panicking through the read-only
// branches (blank marks, viewer title/footer) and carry no selection.
func TestSelectReadOnlyViewRenders(t *testing.T) {
	spec := SelectSpec{
		ReadOnly: true, Title: "Event log",
		Columns: []string{"TIME", "MSG"},
		Fetch:   func() []SelectRow { return nil },
	}
	m := newSelectModel(spec, []SelectRow{row("1", "09:12", "hi"), row("2", "09:13", "bye")})
	_ = m.View() // must not panic on the read-only rendering branches
	if len(m.selected) != 0 {
		t.Errorf("read-only model should carry no selection, got %v", m.selected)
	}
}

// A detailMsg opens the inspect overlay; a blank fetch result falls back to a notice
// so the overlay is never empty.
func TestDetailMsgOpensOverlay(t *testing.T) {
	spec := SelectSpec{Verb: "cancel", Columns: []string{"ID"}, Fetch: func() []SelectRow { return nil }}
	m := newSelectModel(spec, []SelectRow{row("1", "1")})
	m.detailLoading = true

	nm, _ := m.Update(detailMsg{text: "the card"})
	m = nm.(selectModel)
	if m.detailLoading {
		t.Error("detailLoading not cleared when the fetch landed")
	}
	if m.detail != "the card" {
		t.Errorf("overlay text = %q, want %q", m.detail, "the card")
	}

	m.detailLoading = true
	nm, _ = m.Update(detailMsg{text: "   "})
	m = nm.(selectModel)
	if m.detail == "" || m.detail == "   " {
		t.Errorf("blank fetch should fall back to a notice, got %q", m.detail)
	}
}

// The facet key cycles the list through a column's distinct values (all → v1 → … → all)
// and the recompute filters visible rows to the active value.
func TestSelectFacetCycle(t *testing.T) {
	rows := []SelectRow{row("a", "a", "CPU"), row("b", "b", "GPU"), row("c", "c", "CPU")}
	m := newSelectModel(SelectSpec{Columns: []string{"NAME", "CLASS"}, FacetCol: 2}, rows)

	if got := m.facetValues(); len(got) != 2 || got[0] != "CPU" || got[1] != "GPU" {
		t.Fatalf("facetValues = %v, want [CPU GPU]", got)
	}
	steps := []struct {
		want    string
		visible int
	}{
		{"CPU", 2}, // a, c
		{"GPU", 1}, // b
		{"", 3},    // back to all
	}
	for i, s := range steps {
		m.facetVal = m.nextFacet()
		m.recompute()
		if m.facetVal != s.want {
			t.Errorf("step %d facetVal = %q, want %q", i, m.facetVal, s.want)
		}
		if len(m.visible) != s.visible {
			t.Errorf("step %d visible = %d, want %d", i, len(m.visible), s.visible)
		}
	}
}
