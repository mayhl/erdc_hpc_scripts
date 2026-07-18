package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo builds a git project with a nested case dir; returns (root, caseDir).
func repo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	caseDir := filepath.Join(root, "simulations", "funwave", "case_a")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "input.nml"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-q")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "config", "user.name", "Test")
	git(t, root, "config", "commit.gpgsign", "false")
	return root, caseDir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestFindRoot(t *testing.T) {
	root, caseDir := repo(t)
	got, err := FindRoot(caseDir)
	if err != nil {
		t.Fatal(err)
	}
	// macOS TempDir is a /var → /private/var symlink; compare resolved paths.
	want, _ := filepath.EvalSymlinks(root)
	gotR, _ := filepath.EvalSymlinks(got)
	if gotR != want {
		t.Errorf("root = %s, want %s", gotR, want)
	}
	if _, err := FindRoot(t.TempDir()); err == nil {
		t.Error("no error outside a project")
	}
}

func TestAffinity(t *testing.T) {
	_, caseDir := repo(t)
	study := filepath.Dir(caseDir) // simulations/funwave
	write := func(dir, body string) {
		if err := os.WriteFile(filepath.Join(dir, AffinityFile), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Unmarked subtree submits anywhere.
	if _, _, ok, err := Affinity(caseDir); err != nil || ok {
		t.Errorf("unmarked: ok=%v err=%v, want ok=false", ok, err)
	}

	// A study-dir marker locks the whole sweep — comments and blank lines skipped.
	write(study, "# locked to one node\n\nhpc1\n")
	if c, _, ok, err := Affinity(caseDir); err != nil || !ok || c != "hpc1" {
		t.Errorf("study lock: c=%q ok=%v err=%v, want hpc1", c, ok, err)
	}

	// A per-case marker is deeper, so it wins — splitting the sweep.
	write(caseDir, "hpc2\n")
	if c, _, ok, err := Affinity(caseDir); err != nil || !ok || c != "hpc2" {
		t.Errorf("nearest-ancestor: c=%q ok=%v err=%v, want hpc2", c, ok, err)
	}

	// An empty marker is a malformed lock, not silently unlocked.
	write(caseDir, "\n#only a comment\n")
	if _, _, _, err := Affinity(caseDir); err == nil {
		t.Error("empty marker: want error, got nil")
	}
}

func TestFleet(t *testing.T) {
	root, caseDir := repo(t)
	study := filepath.Dir(caseDir) // simulations/funwave
	caseB := filepath.Join(study, "case_b")
	if err := os.MkdirAll(caseB, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// No markers, no .mu-fleet: the project declares no fleet.
	if nodes, _, err := Fleet(root); err != nil || len(nodes) != 0 {
		t.Errorf("bare: nodes=%v err=%v, want empty", nodes, err)
	}

	// Marker union: split a sweep across two nodes; the fleet is their set, deduped
	// and sorted. A third marker repeating a node collapses.
	write(caseDir, AffinityFile, "hpc2\n")
	write(caseB, AffinityFile, "hpc1\n")
	write(study, AffinityFile, "# study default\nhpc1\n")
	nodes, source, err := Fleet(root)
	if err != nil || source != "markers" {
		t.Fatalf("markers: source=%q err=%v", source, err)
	}
	if strings.Join(nodes, ",") != "hpc1,hpc2" {
		t.Errorf("markers: nodes=%v, want [hpc1 hpc2]", nodes)
	}

	// A .mu-fleet at the root overrides the marker union — the bootstrap escape hatch
	// declaring a node no case is locked to yet.
	write(root, FleetFile, "hpc1\nhpc3\n# a data-only node\nhpc2\n")
	nodes, source, err = Fleet(root)
	if err != nil || source != FleetFile {
		t.Fatalf("override: source=%q err=%v", source, err)
	}
	if strings.Join(nodes, ",") != "hpc1,hpc2,hpc3" {
		t.Errorf("override: nodes=%v, want [hpc1 hpc2 hpc3]", nodes)
	}

	// A .mu-fleet naming no nodes is malformed, not an empty fleet.
	write(root, FleetFile, "# only a comment\n")
	if _, _, err := Fleet(root); err == nil {
		t.Error("empty .mu-fleet: want error, got nil")
	}
}

func TestHomeRel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "projects", "proj_a", "simulations", "case_a")
	rel, err := HomeRel(p)
	if err != nil {
		t.Fatal(err)
	}
	if rel != "projects/proj_a/simulations/case_a" {
		t.Errorf("rel = %s", rel)
	}
	if _, err := HomeRel("/somewhere/else"); err == nil {
		t.Error("no error outside $HOME")
	}
	if _, err := HomeRel(filepath.Dir(home)); err == nil {
		t.Error("no error for $HOME's parent")
	}
}

func TestNewStampNoCommit(t *testing.T) {
	_, caseDir := repo(t)
	s := NewStamp(caseDir)
	if s.Commit != "" {
		t.Errorf("commit = %q before any commit", s.Commit)
	}
	if !s.Dirty {
		t.Error("no-sha stamp must record dirty")
	}
	if s.Case != "simulations/funwave/case_a" {
		t.Errorf("case = %q", s.Case)
	}
}

func TestNewStampCleanAndDirty(t *testing.T) {
	root, caseDir := repo(t)
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "init")

	s := NewStamp(caseDir)
	if len(s.Commit) != 40 {
		t.Errorf("commit = %q", s.Commit)
	}
	if s.Dirty {
		t.Error("clean tree reported dirty")
	}

	// Dirty is case-scoped: edits OUTSIDE the case dir must not taint it.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if NewStamp(caseDir).Dirty {
		t.Error("sibling edit tainted the case stamp")
	}
	if err := os.WriteFile(filepath.Join(caseDir, "input.nml"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !NewStamp(caseDir).Dirty {
		t.Error("case edit not reported dirty")
	}
}

func TestStampRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Stamp{Case: "simulations/funwave/case_a", Commit: strings.Repeat("a", 40), Dirty: true}
	if err := os.WriteFile(filepath.Join(dir, StampFile), []byte(in.TOML()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := ReadStamp(dir)
	if !ok {
		t.Fatal("ReadStamp failed")
	}
	if out != in {
		t.Errorf("round trip: %+v != %+v", out, in)
	}
	if _, ok := ReadStamp(t.TempDir()); ok {
		t.Error("ok for a dir without a stamp")
	}
}

func TestCollectRuns(t *testing.T) {
	base := t.TempDir()
	home, work := filepath.Join(base, "home"), filepath.Join(base, "work")
	root := filepath.Join(home, "proj")
	sim := "simulations/funwave"
	write := func(dir, body string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "run.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// a pulled-back run on the project tree, a live one on staging, and junk
	write(filepath.Join(root, sim, "case_a_100"),
		"jobid = \"100\"\nstarted = \"2026-07-09T10:00:00Z\"\ncluster = \"hpc1\"\ncase = \""+sim+"/case_a\"\ncommit = \"aaaabbbbccccdddd\"\ndirty = false\n")
	write(filepath.Join(work, "proj", sim, "case_a_250"),
		"jobid = \"250\"\nstarted = \"2026-07-10T09:00:00Z\"\nqueue = \"standard\"\ndirty = true\n")
	write(filepath.Join(root, sim, "case_junk_1"), "not toml [[[")

	t.Setenv("HOME", home)
	t.Setenv("WORKDIR", work)
	runs := CollectRuns(RunTrees(root))
	if len(runs) != 2 {
		t.Fatalf("runs: %+v", runs)
	}
	// newest started first; junk skipped
	if runs[0].JobID != "250" || !runs[0].Dirty || runs[0].Queue != "standard" {
		t.Errorf("first: %+v", runs[0])
	}
	if runs[1].JobID != "100" || runs[1].Cluster != "hpc1" || runs[1].Commit != "aaaabbbbccccdddd" {
		t.Errorf("second: %+v", runs[1])
	}
}

// TestArchiveMaster covers the archive-master marker round trip: unset reads not-ok, a set
// value reads back, clear removes it, and a marker with only comments is a malformed error.
func TestArchiveMaster(t *testing.T) {
	root, _ := repo(t)

	if _, ok, err := ArchiveMaster(root); err != nil || ok {
		t.Fatalf("unset: ok=%v err=%v, want ok=false nil", ok, err)
	}

	if err := SetArchiveMaster(root, "hpc1"); err != nil {
		t.Fatal(err)
	}
	node, ok, err := ArchiveMaster(root)
	if err != nil || !ok || node != "hpc1" {
		t.Fatalf("after set: (%q, %v, %v), want (hpc1, true, nil)", node, ok, err)
	}

	// A comment-only marker names no node — a malformed marker, not silently unset.
	if err := os.WriteFile(filepath.Join(root, ArchiveFile), []byte("# only a comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ArchiveMaster(root); err == nil {
		t.Error("comment-only marker: want an error")
	}

	if err := ClearArchiveMaster(root); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ArchiveMaster(root); ok {
		t.Error("after clear: want unset")
	}
	if err := ClearArchiveMaster(root); err != nil {
		t.Errorf("clear when absent should be a no-op, got %v", err)
	}
}
