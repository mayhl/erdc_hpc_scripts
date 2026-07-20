package hpc

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestControlArgs: ambient multiplexing is on by default (ControlMaster=auto + a socket +
// the default persist), tunable via MU_SSH_CONTROL_PERSIST, and fully off at 0.
func TestControlArgs(t *testing.T) {
	t.Setenv("MU_SSH_CONTROL_PERSIST", "") // treated as unset → the default window
	joined := strings.Join(controlArgs("u@host"), " ")
	for _, want := range []string{"ControlMaster=auto", "ControlPath=", "ControlPersist=" + strconv.Itoa(controlPersist)} {
		if !strings.Contains(joined, want) {
			t.Errorf("controlArgs() = %q, missing %q", joined, want)
		}
	}

	t.Setenv("MU_SSH_CONTROL_PERSIST", "5")
	if got := strings.Join(controlArgs("u@host"), " "); !strings.Contains(got, "ControlPersist=5") {
		t.Errorf("custom persist not honored: %q", got)
	}

	t.Setenv("MU_SSH_CONTROL_PERSIST", "0")
	if a := controlArgs("u@host"); a != nil {
		t.Errorf("persist=0 must disable multiplexing, got %v", a)
	}
}

// TestAmbientTransport: the rsync -e string carries the same control opts as RemoteExec
// (one shared socket per target), and collapses to the plain transport when reuse is off.
func TestAmbientTransport(t *testing.T) {
	t.Setenv("MU_SSH_CONTROL_PERSIST", "")
	got := AmbientTransport("u@host")
	for _, want := range []string{"ControlMaster=auto", "ControlPath=" + controlPath("u@host"), "ControlPersist="} {
		if !strings.Contains(got, want) {
			t.Errorf("AmbientTransport() = %q, missing %q", got, want)
		}
	}

	t.Setenv("MU_SSH_CONTROL_PERSIST", "0")
	if got := AmbientTransport("u@host"); strings.Contains(got, "ControlMaster") {
		t.Errorf("persist=0 must yield the plain transport, got %q", got)
	}
}

// TestControlPath: the socket path is stable per target, distinct across targets, carries the
// mu-cm marker, and stays well under the unix-socket path limit even for a long target.
func TestControlPath(t *testing.T) {
	a := controlPath("user@login.hpc1")
	if a != controlPath("user@login.hpc1") {
		t.Error("controlPath not stable for the same target")
	}
	if a == controlPath("user@login.node-a") {
		t.Error("distinct targets must not share a control socket")
	}
	if !strings.Contains(a, "mu-cm-") {
		t.Errorf("unexpected control path %q", a)
	}
	if long := controlPath(strings.Repeat("x", 300) + "@host"); len(long) > 104 {
		t.Errorf("control path too long for a unix socket: %d (%q)", len(long), long)
	}
}

// TestFirstLine: the terse error summary folded into a bounded remote-exec failure
// is the first non-blank line, trimmed.
func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"\n\n  \nhello\nworld": "hello",
		"  trim me  \n":        "trim me",
		"only":                 "only",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExitText(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{255, "ticket"}, // both 255 branches (no-ticket / auth-or-host) name the ticket
		{127, "not found"},
		{126, "permission denied"},
	}
	for _, c := range cases {
		err := exec.Command("sh", "-c", "exit "+strconv.Itoa(c.code)).Run()
		if got := exitText(err); !strings.Contains(got, c.want) {
			t.Errorf("exit %d → %q, want it to contain %q", c.code, got, c.want)
		}
	}
	// a non-ExitError falls through to its message
	if got := exitText(errors.New("boom")); got != "boom" {
		t.Errorf("plain error: %q", got)
	}
}

// TestLocalExec: the on-cluster exec seam returns a command's stdout on success and a
// terse exit-code reason on failure — separate buffers keep stdout clean and preserve
// the real exit code (no shell pipe).
func TestLocalExec(t *testing.T) {
	out, err := LocalExec("echo hi")
	if err != nil {
		t.Fatalf("LocalExec(echo) err = %v", err)
	}
	if strings.TrimSpace(out) != "hi" {
		t.Errorf("LocalExec(echo) out = %q, want %q", out, "hi")
	}
	// a 127 exit maps through exitText to the not-found reason, not a raw wait-status
	if _, err := LocalExec("exit 127"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("LocalExec(exit 127) err = %v, want it to contain %q", err, "not found")
	}
}

// TestConnectSeconds: the default holds, a valid MU_SSH_CONNECT_TIMEOUT overrides,
// and a non-positive or unparseable value falls back to the default.
func TestConnectSeconds(t *testing.T) {
	if got := connectSeconds(); got != connectTimeout {
		t.Errorf("connectSeconds() default = %d, want %d", got, connectTimeout)
	}
	cases := map[string]int{"5": 5, "0": connectTimeout, "-3": connectTimeout, "nope": connectTimeout}
	for env, want := range cases {
		t.Setenv("MU_SSH_CONNECT_TIMEOUT", env)
		if got := connectSeconds(); got != want {
			t.Errorf("connectSeconds() with %q = %d, want %d", env, got, want)
		}
	}
}

// TestArmSpinner: the stop func is safe and prompt whether the delay-timer already
// fired (slept past spinnerDelay) or not (stopped immediately) — off-TTY here, so
// render.Spinner is a no-op and this exercises the timer/mutex arm-cancel path.
func TestArmSpinner(t *testing.T) {
	armSpinner("host")() // immediate stop cancels the pending timer
	stop := armSpinner("host")
	time.Sleep(spinnerDelay + 50*time.Millisecond) // let the timer fire
	stop()                                         // clears a shown (here no-op) spinner without hanging
}

// TestRemoteExecQuoting: a command's own quotes and pipes must reach the remote bash
// intact — RemoteExec passes it as ONE single-quoted `bash -lc` argv element, never
// re-split by a local shell. Pinned against the literal quoted form via the MU_SSH stub.
func TestRemoteExecQuoting(t *testing.T) {
	stub := newSSHStub(t)
	t.Setenv("MU_SSH_CONTROL_PERSIST", "0") // plain argv: the quote wrap is the subject
	t.Setenv("MU_TEST_SSH_STDOUT", "2")
	out, err := RemoteExec("u@host", `grep 'a b' "$f" | wc -l`)
	if err != nil {
		t.Fatalf("RemoteExec: %v", err)
	}
	if out != "2\n" {
		t.Errorf("out = %q, want %q", out, "2\n")
	}
	calls := stub.calls(t)
	if len(calls) != 1 {
		t.Fatalf("want one ssh invocation, got %v", calls)
	}
	args := calls[0]
	want := `bash -lc 'grep '\''a b'\'' "$f" | wc -l'`
	if got := args[len(args)-1]; got != want {
		t.Errorf("remote arg = %q, want %q", got, want)
	}
	if args[len(args)-2] != "u@host" {
		t.Errorf("argv %v: target not before the command", args)
	}
}

// TestRemoteExecTimeoutControlArgs: the fan-out path now rides the same ambient
// ControlMaster as RemoteExec, and derives ConnectTimeout from the caller's deadline
// rather than the interactive default.
func TestRemoteExecTimeoutControlArgs(t *testing.T) {
	stub := newSSHStub(t)
	t.Setenv("MU_SSH_CONTROL_PERSIST", "") // treated as unset → the default window, reuse on
	if _, err := RemoteExecTimeout("u@host", "true", 5*time.Second); err != nil {
		t.Fatalf("RemoteExecTimeout: %v", err)
	}
	joined := strings.Join(stub.calls(t)[0], " ")
	for _, want := range []string{
		"ConnectTimeout=5",
		"ControlMaster=auto",
		"ControlPath=" + controlPath("u@host"),
		"ControlPersist=" + strconv.Itoa(controlPersist),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}

	// reuse off → no control opts; a sub-second deadline clamps ConnectTimeout to 1
	stub2 := newSSHStub(t)
	t.Setenv("MU_SSH_CONTROL_PERSIST", "0")
	if _, err := RemoteExecTimeout("u@host", "true", 900*time.Millisecond); err != nil {
		t.Fatalf("RemoteExecTimeout: %v", err)
	}
	joined = strings.Join(stub2.calls(t)[0], " ")
	if strings.Contains(joined, "ControlMaster") {
		t.Errorf("persist=0 must drop the control opts: %q", joined)
	}
	if !strings.Contains(joined, "ConnectTimeout=1") {
		t.Errorf("sub-second deadline must clamp ConnectTimeout to 1: %q", joined)
	}
}

// TestRemoteExecTimeoutDeadline: a hung remote (the stub sleeping past the deadline) is
// killed and reported as a clean timeout, not a raw signal error.
func TestRemoteExecTimeoutDeadline(t *testing.T) {
	newSSHStub(t)
	t.Setenv("MU_SSH_CONTROL_PERSIST", "0")
	t.Setenv("MU_TEST_SSH_SLEEP", "5")
	_, err := RemoteExecTimeout("u@host", "true", 150*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout after") {
		t.Errorf("err = %v, want a timeout", err)
	}
}

// TestRemoteExecTimeoutStderrFold: a failure with remote stderr folds its first line into
// the error — never the classify probe (no host is dialed when stderr explains it).
func TestRemoteExecTimeoutStderrFold(t *testing.T) {
	newSSHStub(t)
	t.Setenv("MU_SSH_CONTROL_PERSIST", "0")
	t.Setenv("MU_TEST_SSH_EXIT", "255")
	t.Setenv("MU_TEST_SSH_STDERR", "kinit: no credentials\nsecond line")
	_, err := RemoteExecTimeout("u@host", "true", 5*time.Second)
	if err == nil || err.Error() != "kinit: no credentials" {
		t.Errorf("err = %v, want the first stderr line", err)
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"user@login.example": "login.example",
		"hpc1":               "hpc1",
		"a@b@c":              "c", // strips through the last @
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
