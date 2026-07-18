package cli

import (
	"strings"
	"testing"
)

// TestParseDeletions covers the --delete dry itemize: only *deleting lines yield paths,
// directory removals (trailing slash) are dropped, and non-deletion itemize/stats noise is
// ignored.
func TestParseDeletions(t *testing.T) {
	out := strings.Join([]string{
		"cd+++++++++ ./",       // a create — ignored
		"*deleting   old/a.nc", // a file removal
		"*deleting   old/b.nc", // a file removal
		"*deleting   old/",     // a dir removal — skipped
		"<f+++++++++ new.nc",   // a transfer line — ignored
		"Number of files: 3",   // stats noise — ignored
	}, "\n")

	got := parseDeletions([]byte(out))
	want := []string{"old/a.nc", "old/b.nc"}
	if len(got) != len(want) {
		t.Fatalf("parseDeletions = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("parseDeletions[%d] = %q, want %q", i, got[i], w)
		}
	}
}

// TestPruneManifest covers the delete-side manifest trim: pruned paths drop, survivors keep
// their order, an unknown path is a no-op, and an emptied manifest is a valid empty record.
func TestPruneManifest(t *testing.T) {
	base := syncManifest{File: []manifestFile{
		{Path: "a.nc"}, {Path: "b.nc"}, {Path: "sub/c.nc"},
	}}

	got := pruneManifest(base, []string{"b.nc", "nope.nc"})
	want := []string{"a.nc", "sub/c.nc"}
	if len(got.File) != len(want) {
		t.Fatalf("kept %d entries, want %d (%v)", len(got.File), len(want), got.File)
	}
	for i, w := range want {
		if got.File[i].Path != w {
			t.Errorf("File[%d].Path = %q, want %q", i, got.File[i].Path, w)
		}
	}

	// The source manifest is untouched (pure — no aliasing of its backing array).
	if len(base.File) != 3 {
		t.Errorf("base mutated: %d entries, want 3", len(base.File))
	}

	// Pruning every path leaves an empty but valid record.
	empty := pruneManifest(base, []string{"a.nc", "b.nc", "sub/c.nc"})
	if len(empty.File) != 0 {
		t.Errorf("fully pruned manifest has %d entries, want 0", len(empty.File))
	}
}
