package render

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain keeps any emitter call in this package from touching the real
// ~/.cache/mayhl_utils/framework.log; individual tests override per-case.
func TestMain(m *testing.M) {
	if os.Getenv("MU_LOG_FILE") == "" {
		_ = os.Setenv("MU_LOG_FILE", filepath.Join(os.TempDir(), "mu-render-test-default.log"))
	}
	os.Exit(m.Run())
}

func TestFrameworkLogFormat(t *testing.T) {
	logf := filepath.Join(t.TempDir(), "framework.log")
	t.Setenv("MU_LOG_FILE", logf)
	ResetLoggerForTest()

	Scoped("mu").Info("hello") // scoped ⇒ logged
	EventOK("cp", "done")      // log-only event
	OK("ephemeral")            // unscoped ⇒ terminal only, NOT logged

	got := readFile(t, logf)
	// [ts] [level(pad5)] [scope] msg
	for _, want := range []string{"[INFO ] [mu] hello", "[OK   ] [cp] done"} {
		if !strings.Contains(got, want) {
			t.Errorf("event log missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ephemeral") {
		t.Errorf("unscoped OK() must not be logged, but was:\n%s", got)
	}
	if !strings.HasPrefix(got, "[20") { // leading ISO timestamp
		t.Errorf("expected leading [timestamp], got:\n%s", got)
	}
}

func TestStep(t *testing.T) {
	logf := filepath.Join(t.TempDir(), "framework.log")
	t.Setenv("MU_LOG_FILE", logf)
	ResetLoggerForTest()

	out, err := Step("cp", "push", func() (int, error) { return 42, nil })
	if err != nil || out != 42 {
		t.Fatalf("Step success: out=%d err=%v", out, err)
	}
	if _, err := Step("cp", "pull", func() (int, error) { return 0, errors.New("boom") }); err == nil {
		t.Fatal("Step must propagate the error")
	}

	got := readFile(t, logf)
	for _, want := range []string{
		"[INFO ] [cp] push",       // start
		"[OK   ] [cp] push (",     // success end carries a duration
		"[INFO ] [cp] pull",       // start
		"[ERROR] [cp] pull: boom", // failure end
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// TestLogBlockGroupsByDay checks the reader's block view: one day header per date,
// time-only rows, and a raw-stamp fallback for an unparseable timestamp.
func TestLogBlockGroupsByDay(t *testing.T) {
	t.Setenv("MU_RENDER", "plain") // deterministic: no ANSI to assert around
	d1 := time.Date(2026, 7, 5, 14, 3, 22, 0, time.Local)
	rows := []LogRow{
		{Time: d1, Level: "OK", Scope: "cp", Msg: "copied"},
		{Time: time.Date(2026, 7, 6, 9, 12, 0, 0, time.Local), Level: "ERROR", Scope: "job", Msg: "cancelled"},
		{Time: time.Date(2026, 7, 6, 9, 15, 0, 0, time.Local), Level: "WARN", Scope: "sshfs", Msg: "slow"},
		{RawTS: "bogus-ts", Level: "INFO", Scope: "x", Msg: "unparsed"}, // zero Time
	}
	out := LogBlock("Event log", rows)

	for _, day := range []string{"5 Jul 2026", "6 Jul 2026"} {
		if n := strings.Count(out, day); n != 1 {
			t.Errorf("%q header count = %d, want 1:\n%s", day, n, out)
		}
	}
	for _, want := range []string{"14:03:22", "copied", "cancelled", "slow", "bogus-ts"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestEmitPayload checks the structured-event write path: a payload is stored as the
// inline tab-suffix with the id injected, and a payload-less emit adds no suffix.
func TestEmitPayload(t *testing.T) {
	logf := filepath.Join(t.TempDir(), "events.log")
	t.Setenv("MU_LOG_FILE", logf)
	ResetLoggerForTest()

	id := Emit("cp", "ok", "copied", map[string]any{"n": 3})
	if id == "" {
		t.Fatal("Emit with payload returned an empty id")
	}
	if got := Emit("cp", "info", "plain", nil); got != "" {
		t.Errorf("Emit without payload returned id %q, want empty", got)
	}

	got := readFile(t, logf)
	if !strings.Contains(got, "copied\t{") {
		t.Errorf("structured line missing tab-delimited payload:\n%s", got)
	}
	if !strings.Contains(got, `"id":"`+id+`"`) || !strings.Contains(got, `"n":3`) {
		t.Errorf("payload JSON missing id/fields:\n%s", got)
	}
	for _, ln := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.Contains(ln, "plain") && strings.Contains(ln, "\t") {
			t.Errorf("no-payload line should carry no tab suffix: %q", ln)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// capErr leaves short errors alone; an over-cap tail keeps the head + LAST lines
// behind an omission marker and points at a full err-<id>.log dump.
func TestCapErr(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MU_LOG_FILE", filepath.Join(dir, "framework.log"))

	short := "boom\nline1\nline2"
	if got := capErr(short); got != short {
		t.Errorf("within-cap message altered: %q", got)
	}

	lines := []string{"remote build failed"}
	for i := 0; i < 40; i++ {
		lines = append(lines, "spew")
	}
	lines = append(lines, "Error: the verdict")
	msg := strings.Join(lines, "\n")

	got := strings.Split(capErr(msg), "\n")
	if got[0] != "remote build failed" {
		t.Errorf("head line lost: %q", got[0])
	}
	if !strings.Contains(got[1], "line(s) omitted") {
		t.Errorf("omission marker missing: %q", got[1])
	}
	if got[len(got)-2] != "Error: the verdict" {
		t.Errorf("tail must end with the message's last line, got %q", got[len(got)-2])
	}
	last := got[len(got)-1]
	if !strings.HasPrefix(last, "full output: ") {
		t.Fatalf("dump pointer missing: %q", last)
	}
	dump, err := os.ReadFile(strings.TrimPrefix(last, "full output: "))
	if err != nil {
		t.Fatalf("dump file unreadable: %v", err)
	}
	if strings.TrimSuffix(string(dump), "\n") != msg {
		t.Error("dump file doesn't carry the full uncut message")
	}
	if len(got) != 2+errTailCap+1 { // head + marker + tail cap + pointer
		t.Errorf("capped block is %d lines, want %d", len(got), 2+errTailCap+1)
	}
}
