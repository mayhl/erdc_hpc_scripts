package render

import "testing"

func TestFitTunnelColumns(t *testing.T) {
	cols := []col[TunnelRow]{
		{"ID", func(r TunnelRow) string { return r.ID }},
		{"PORT", func(r TunnelRow) string { return r.Port }},
		{"System", func(r TunnelRow) string { return r.System }},
		{"Job", func(r TunnelRow) string { return r.Job }},
		{"Node", func(r TunnelRow) string { return r.Node }},
		{"State", func(r TunnelRow) string { return r.State }},
		{"Wall left", func(r TunnelRow) string { return r.WallLeft }},
	}
	// short cells so the header widths govern: full width = 1 + Σ(len(header)+3)
	// = 1 + 5+7+9+6+7+8+12 = 55; dropping Node frees 7 (→48), then Job frees 6 (→42).
	rows := []TunnelRow{{ID: "a", Port: "1", System: "s", Job: "j", Node: "n", State: "x", WallLeft: "w"}}
	drop := []string{"Node", "Job"}

	// The five essentials must never drop, at any width.
	essentials := []string{"ID", "PORT", "System", "State", "Wall left"}

	cases := []struct {
		name              string
		avail             int
		wantNode, wantJob bool
	}{
		{"wide keeps all", 1000, true, true},
		{"piped keeps all", 0, true, true},          // unknown width → nothing dropped
		{"tight drops node only", 52, false, true},  // 55>52 → drop Node → 48<=52
		{"narrow drops node+job", 45, false, false}, // →48>45 → drop Job → 42<=45
	}
	for _, c := range cases {
		keep := fitTunnelColumns(cols, rows, drop, c.avail)
		if keep["Node"] != c.wantNode || keep["Job"] != c.wantJob {
			t.Errorf("%s (avail=%d): Node=%v Job=%v, want %v/%v",
				c.name, c.avail, keep["Node"], keep["Job"], c.wantNode, c.wantJob)
		}
		for _, e := range essentials {
			if !keep[e] {
				t.Errorf("%s (avail=%d): dropped essential column %q", c.name, c.avail, e)
			}
		}
	}
}
