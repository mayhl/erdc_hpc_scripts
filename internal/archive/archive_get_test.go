package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubCall is one recorded stub invocation: the cwd it ran from, its
// ARCHIVE_PROBE value, and the argv joined by spaces.
type stubCall struct{ pwd, probe, argv string }

// stubArchive plants a recording fake of the site archive command: every call
// appends cwd|probe|argv to the log, and the ls subcommand replays the canned
// listing in $MU_TEST_STUB_LS (exiting $MU_TEST_STUB_LS_RC when set).
func stubArchive(t *testing.T) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "archive")
	log = filepath.Join(dir, "log")
	script := `#!/bin/sh
printf '%s|%s|%s\n' "$(pwd -P)" "${ARCHIVE_PROBE:-}" "$*" >> "$MU_TEST_STUB_LOG"
if [ "$1" = ls ]; then
  [ -n "$MU_TEST_STUB_LS_RC" ] && exit "$MU_TEST_STUB_LS_RC"
  printf '%s\n' "$MU_TEST_STUB_LS"
fi
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MU_TEST_STUB_LOG", log)
	t.Setenv("MU_TEST_STUB_LS", "")
	t.Setenv("MU_TEST_STUB_LS_RC", "")
	return bin, log
}

func stubCalls(t *testing.T, log string) []stubCall {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out []stubCall
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		p := strings.SplitN(l, "|", 3)
		if len(p) != 3 {
			t.Fatalf("bad stub log line %q", l)
		}
		out = append(out, stubCall{p[0], p[1], p[2]})
	}
	return out
}

// physical resolves symlinks for cwd comparison (macOS /var/folders → /private/…)
func physical(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGetLeaf(t *testing.T) {
	const dst = "/arch/proj/simulations/funwave/case_a"
	cases := []struct {
		name    string
		ls      string // canned `archive ls` listing
		lsRC    string // non-empty = ls exits with this code
		wantRC  int
		wantGet string // expected get argv; "" = no get invocation
	}{
		{"unsplit", "250.tar", "", 0, "get -C " + dst + " -x 250.tar"},
		{
			"split", "250_t000.tar\n250_t001.tar\n250_rest.tar", "", 0,
			"get -C " + dst + " -x 250_rest.tar 250_t000.tar 250_t001.tar",
		},
		// sibling run 2500 shares the ls wildcard prefix — must not be swept in
		{
			"sibling excluded", "250.tar\n2500.tar\n2500_rest.tar", "", 0,
			"get -C " + dst + " -x 250.tar",
		},
		{"no chunks", "2500.tar", "", 1, ""},
		{"ls fails", "", "3", 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, parent := world(t)
			bin, log := stubArchive(t)
			t.Setenv("MU_TEST_STUB_LS", tc.ls)
			t.Setenv("MU_TEST_STUB_LS_RC", tc.lsRC)
			if rc := getLeaf(bin, parent, "case_a_250"); rc != tc.wantRC {
				t.Fatalf("rc=%d want %d", rc, tc.wantRC)
			}
			calls := stubCalls(t, log)
			if calls[0].argv != "ls "+dst+"/250*.tar" {
				t.Fatalf("ls call: %q", calls[0].argv)
			}
			if tc.wantGet == "" {
				if len(calls) != 1 {
					t.Fatalf("expected the ls probe only, got %v", calls)
				}
				return
			}
			if len(calls) != 2 {
				t.Fatalf("expected ls + one get, got %v", calls)
			}
			get := calls[1]
			if get.argv != tc.wantGet {
				t.Fatalf("get argv: got %q want %q", get.argv, tc.wantGet)
			}
			// one get from the local parent, with the injected -C's size verify on
			if get.pwd != physical(t, parent) || get.probe != "yes" {
				t.Fatalf("get ran as %+v", get)
			}
		})
	}
}

func TestGetLeafProjectsAbsentLeaf(t *testing.T) {
	work, _ := world(t)
	bin, log := stubArchive(t)
	t.Setenv("MU_TEST_STUB_LS", "300.tar")
	wd := filepath.Join(work, "proj/simulations")
	// neither newdir nor the leaf exists locally — projection is a pure rewrite
	if rc := getLeaf(bin, wd, "newdir/case_a_300"); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	parent := filepath.Join(wd, "newdir")
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		t.Fatalf("local parent not created: %v", err)
	}
	calls := stubCalls(t, log)
	if len(calls) != 2 || calls[0].argv != "ls /arch/proj/simulations/newdir/case_a/300*.tar" {
		t.Fatalf("calls: %v", calls)
	}
	if calls[1].argv != "get -C /arch/proj/simulations/newdir/case_a -x 300.tar" ||
		calls[1].pwd != physical(t, parent) {
		t.Fatalf("get ran as %+v", calls[1])
	}
}

func TestGetSplitsLeavesFromRest(t *testing.T) {
	_, parent := world(t)
	bin, log := stubArchive(t)
	t.Setenv("MU_TEST_STUB_LS", "250.tar")
	if rc := get(bin, parent, []string{"case_a_250", "notes.txt"}); rc != 0 {
		t.Fatalf("rc=%d", rc)
	}
	calls := stubCalls(t, log)
	if len(calls) != 3 {
		t.Fatalf("expected ls + leaf get + passthrough get, got %v", calls)
	}
	if calls[1].argv != "get -C /arch/proj/simulations/funwave/case_a -x 250.tar" {
		t.Fatalf("leaf get: %q", calls[1].argv)
	}
	// the non-case arg falls to one passthrough get at wd's projection
	if calls[2].argv != "get -C /arch/proj/simulations/funwave notes.txt" || calls[2].probe != "yes" {
		t.Fatalf("passthrough get: %+v", calls[2])
	}
}

func TestGetLeafFailureShortCircuits(t *testing.T) {
	_, parent := world(t)
	bin, log := stubArchive(t)
	t.Setenv("MU_TEST_STUB_LS", "2500.tar") // sibling only — nothing for 250
	if rc := get(bin, parent, []string{"case_a_250", "notes.txt"}); rc != 1 {
		t.Fatalf("rc=%d", rc)
	}
	// the failed leaf stops the run before the rest passthrough
	if calls := stubCalls(t, log); len(calls) != 1 {
		t.Fatalf("expected the ls probe only, got %v", calls)
	}
}
