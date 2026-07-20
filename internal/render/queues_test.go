package render

import (
	"strings"
	"testing"
)

func TestQueueLoad(t *testing.T) {
	cases := []struct{ run, pend, want string }{
		{"0", "0", "unused"},   // idle
		{"8", "0", "low"},      // running, no backlog
		{"12", "3", "low"},     // 3 <= 12
		{"4", "8", "med"},      // 8 <= 3*4, > 4
		{"2", "10", "high"},    // 10 <= 10*2, > 3*2
		{"1", "20", "extreme"}, // 20 > 10*1
		{"0", "5", "extreme"},  // starved: pending, nothing running
		{"--", "--", "--"},     // not numeric → unknown
		{"", "", "--"},
	}
	for _, c := range cases {
		if got, _ := QueueLoad(c.run, c.pend); got != c.want {
			t.Errorf("QueueLoad(%q,%q) = %q, want %q", c.run, c.pend, got, c.want)
		}
	}
}

func TestPlanQueueCols(t *testing.T) {
	rows := []QueueRow{{Name: "standard", Class: "CPU", MaxJobs: "50", MaxCores: "4096", Run: "1", Pend: "0"}}
	headers := func(cols []col[QueueRow]) string {
		h := make([]string, len(cols))
		for i, c := range cols {
			h[i] = c.header
		}
		return strings.Join(h, " ")
	}

	t.Setenv("COLUMNS", "200") // wide: full default set, no MaxNodes (unconfigured)
	if got := headers(planQueueCols(rows, false)); got != "Queue Class Walltime MaxJobs MaxCores Run Pend Load" {
		t.Errorf("wide default: %q", got)
	}

	t.Setenv("COLUMNS", "40") // narrow: shed MaxJobs/Run/Pend down to the protected set
	if got := headers(planQueueCols(rows, false)); got != "Queue Class Walltime MaxCores Load" {
		t.Errorf("narrow default: %q", got)
	}

	t.Setenv("COLUMNS", "200") // -a shows everything unshed, incl Type + State
	if got := headers(planQueueCols(rows, true)); got != "Queue Class Type Walltime MaxJobs MaxCores Run Pend Load State" {
		t.Errorf("all view: %q", got)
	}

	// With MaxNodes configured, it's the size column and MaxCores becomes shed-able.
	nodeRows := []QueueRow{{Name: "standard", Class: "CPU", MaxCores: "4096", MaxNodes: "32", Run: "1", Pend: "0"}}
	t.Setenv("COLUMNS", "48")
	if got := headers(planQueueCols(nodeRows, false)); got != "Queue Class Walltime MaxNodes Load" {
		t.Errorf("narrow w/ nodes (MaxCores shed, MaxNodes kept): %q", got)
	}
}

func TestQueueState(t *testing.T) {
	cases := []struct{ e, r, want string }{
		{"Y", "Y", "● up"},
		{"Y", "N", "○ stopped"},
		{"N", "N", "○ disabled"},
		{"N", "Y", "○ disabled"}, // disabled wins over running
		{"-", "-", "--"},         // not reported
		{"", "", "--"},
	}
	for _, c := range cases {
		if got, _ := QueueState(c.e, c.r); got != c.want {
			t.Errorf("QueueState(%q,%q) = %q, want %q", c.e, c.r, got, c.want)
		}
	}
}
