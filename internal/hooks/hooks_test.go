package hooks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mayhl/mayhl_utils/internal/config"
)

// world builds the two-tier layout: a git project on "home" carrying a progress
// hook, and an iterate-mode run dir on "work" that is NOT a checkout — discovery
// must chain through the swap mirror. Returns the run dir and the hook path.
func world(t *testing.T) (runDir, hook string) {
	t.Helper()
	// first-exec of a fresh script can take seconds under the laptop AV — don't
	// let the contract timeout flake the suite
	old := Timeout
	Timeout = 60 * time.Second
	t.Cleanup(func() { Timeout = old })
	base := t.TempDir()
	home, work := filepath.Join(base, "home"), filepath.Join(base, "work")
	sim := "proj/simulations/funwave"
	caseHome := filepath.Join(home, sim, "case_a")
	runDir = filepath.Join(work, sim, "case_a_250")
	hooksDir := filepath.Join(home, "proj/scripts/funwave/hooks")
	for _, p := range []string{
		caseHome, runDir, hooksDir,
		filepath.Join(home, "proj/.git"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hook = filepath.Join(hooksDir, "progress")
	script := "#!/bin/sh\necho '{\"pct\": 38, \"eta\": \"17:20\"}'\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("WORKDIR", work)
	t.Setenv("ARCHIVE_HOME", "/arch")
	t.Setenv("MU_CONFIG_FILE", filepath.Join(base, "nonexistent.toml"))
	t.Setenv("MU_ROOT", "")
	config.ResetForTest()
	return runDir, hook
}

func TestModel(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"/h/proj/simulations/funwave/case_a_250", "funwave", true},
		{"/h/proj/simulations/ww3-funwave/deep/case_b", "ww3-funwave", true},
		{"/h/proj/simulations/data/bathy", "", false},
		{"/h/proj/scripts/funwave", "", false},
	}
	for _, c := range cases {
		got, ok := Model(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("Model(%q) = %q,%v want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFindChainsThroughSwap(t *testing.T) {
	runDir, hook := world(t)
	got, ok := Find(runDir, "progress")
	if !ok || got != hook {
		t.Fatalf("Find = %q,%v want %q", got, ok, hook)
	}
	if _, ok := Find(runDir, "walltime"); ok {
		t.Fatal("found a hook that doesn't exist")
	}
}

func TestFindDirect(t *testing.T) {
	// a $HOME-side case dir is inside the checkout itself — tier 1, no mirror hop
	runDir, hook := world(t)
	home := os.Getenv("HOME")
	caseDir := filepath.Join(home, "proj/simulations/funwave/case_a")
	if got, ok := Find(caseDir, "progress"); !ok || got != hook {
		t.Fatalf("Find(case) = %q,%v want %q", got, ok, hook)
	}
	_ = runDir
}

func TestList(t *testing.T) {
	runDir, _ := world(t)
	if got := List(runDir); len(got) != 1 || got[0] != "progress" {
		t.Fatalf("List = %v", got)
	}
}

func TestExecContract(t *testing.T) {
	runDir, hook := world(t)
	r := Exec(hook, runDir, "250")
	if r.Exit != 0 || r.Err != "" {
		t.Fatalf("exec: %+v", r)
	}
	if pct, ok := r.Data["pct"].(float64); !ok || pct != 38 {
		t.Fatalf("data: %+v", r.Data)
	}
}

func TestExecSemantics(t *testing.T) {
	runDir, _ := world(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// warn (exit 2) still parses; fail (exit 1) doesn't; garbage stdout = fail
	warn := Exec(write("warn", "#!/bin/sh\necho '{\"pct\": 9}'\nexit 2\n"), runDir, "1")
	if warn.Exit != 2 || warn.Data["pct"].(float64) != 9 {
		t.Fatalf("warn: %+v", warn)
	}
	fail := Exec(write("fail", "#!/bin/sh\necho '{\"pct\": 9}'\nexit 3\n"), runDir, "1")
	if fail.Exit != 3 || fail.Data != nil {
		t.Fatalf("fail: %+v", fail)
	}
	junk := Exec(write("junk", "#!/bin/sh\necho not-json\n"), runDir, "1")
	if junk.Exit == 0 || junk.Data != nil {
		t.Fatalf("junk: %+v", junk)
	}
	// hook sees MU_JOBID and runs with CWD = the run dir (symlink-resolved —
	// macOS /var → /private/var)
	resolved, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		t.Fatal(err)
	}
	env := Exec(write("env", "#!/bin/sh\nprintf '{\"id\": \"%s\", \"cwd\": \"%s\"}' \"$MU_JOBID\" \"$PWD\"\n"), runDir, "77")
	if env.Data["id"] != "77" || env.Data["cwd"] != resolved {
		t.Fatalf("env: %+v", env.Data)
	}
}

func TestExecPack(t *testing.T) {
	old := PackTimeout
	PackTimeout = 60 * time.Second // AV can stall a fresh script's first exec
	t.Cleanup(func() { PackTimeout = old })
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// a well-formed manifest parses into the typed groups, CWD = the leaf
	ok := write("pack", "#!/bin/sh\necho '{\"tars\":[{\"suffix\":\"t000\",\"members\":[\"output/f_000*.nc\"]}]}'\n")
	m, err := ExecPack(ok, dir)
	if err != nil {
		t.Fatalf("ExecPack: %v", err)
	}
	if len(m.Tars) != 1 || m.Tars[0].Suffix != "t000" || m.Tars[0].Members[0] != "output/f_000*.nc" {
		t.Fatalf("manifest: %+v", m)
	}

	// non-zero exit and non-JSON stdout are both errors → caller degrades to one tar
	if _, err := ExecPack(write("boom", "#!/bin/sh\nexit 1\n"), dir); err == nil {
		t.Fatal("expected an error on non-zero exit")
	}
	if _, err := ExecPack(write("junk", "#!/bin/sh\necho not-json\n"), dir); err == nil {
		t.Fatal("expected an error on non-JSON stdout")
	}

	// a hook that outruns the budget is an error, not a hang
	PackTimeout = 100 * time.Millisecond
	if _, err := ExecPack(write("slow", "#!/bin/sh\nsleep 5\n"), dir); err == nil {
		t.Fatal("expected a timeout error")
	}
}
