package cli

import (
	"testing"

	"github.com/mayhl/mayhl_utils/internal/queue"
)

func TestCancelCmd(t *testing.T) {
	if got := cancelCmd("pbs", []string{"1284570.hpc1", "1284571.hpc1"}); got != `qdel '1284570' '1284571'` {
		t.Errorf("pbs batch: %q", got)
	}
	if got := cancelCmd("slurm", []string{"9001", "9002"}); got != `scancel '9001' '9002'` {
		t.Errorf("slurm batch: %q", got)
	}
	// PBS array brackets survive the leading-segment trim and stay quoted against globs.
	if got := cancelCmd("pbs", []string{"1284[7].hpc1"}); got != `qdel '1284[7]'` {
		t.Errorf("array id quoting: %q", got)
	}
	if got := cancelCmd("", []string{"1"}); got != "" {
		t.Errorf("unknown scheduler should yield empty cmd, got %q", got)
	}
}

// Collate picker rows must qualify the row ID by cluster (short ids collide across
// systems) and lead with the SYSTEM cell the facet cycles.
func TestCollateSelectRows(t *testing.T) {
	jobs := []queue.Job{
		{ID: "9001", ShortID: "9001", Name: "a", Cluster: "alpha"},
		{ID: "9001", ShortID: "9001", Name: "b", Cluster: "bravo"},
	}
	rows := collateSelectRows(jobs)
	if rows[0].ID != "alpha/9001" || rows[1].ID != "bravo/9001" {
		t.Errorf("row ids not cluster-qualified: %q, %q", rows[0].ID, rows[1].ID)
	}
	if rows[0].Cells[0] != "alpha" || rows[0].Cells[1] != "9001" {
		t.Errorf("SYSTEM must lead the cells: %v", rows[0].Cells)
	}
}

// clustersOf keeps first-seen order so per-cluster cancel batches follow the view.
func TestClustersOf(t *testing.T) {
	jobs := []queue.Job{
		{ID: "1", Cluster: "alpha"}, {ID: "2", Cluster: "bravo"}, {ID: "3", Cluster: "alpha"},
	}
	got := clustersOf(jobs)
	if len(got) != 2 || got[0] != "alpha" || got[1] != "bravo" {
		t.Errorf("clustersOf = %v, want [alpha bravo]", got)
	}
}

// jobPreview shows only what the columns don't: id line always; info/time lines only
// when the snapshot reported something for them.
func TestJobPreview(t *testing.T) {
	full := queue.Job{
		ID: "1284570.hpc1", Name: "wave_run", Nodes: "4",
		Reason: "(Priority)", Submit: "07-19T14:02", Start: "07-20T08:00",
	}
	want := "1284570.hpc1 — wave_run\n4 node(s) · waiting: Priority\nsubmit 07-19T14:02 · start 07-20T08:00"
	if got := jobPreview(full); got != want {
		t.Errorf("full preview:\n got %q\nwant %q", got, want)
	}
	// A running SLURM job's Reason is a bare nodelist, not a parenthesized reason.
	running := queue.Job{ID: "9001", Name: "sim", Reason: "r1n[0-3]"}
	if got := jobPreview(running); got != "9001 — sim\nnodes: r1n[0-3]" {
		t.Errorf("nodelist preview: %q", got)
	}
	// PBS reports none of the extras — the pane still shows the full-id line.
	bare := queue.Job{ID: "77.hpc1", Name: "a"}
	if got := jobPreview(bare); got != "77.hpc1 — a" {
		t.Errorf("bare preview: %q", got)
	}
}
