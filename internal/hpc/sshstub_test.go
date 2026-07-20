package hpc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// sshStub is a recording stand-in for the transport ssh: a script installed as MU_SSH that
// appends each invocation's argv to a log and behaves per MU_TEST_SSH_* env — exit code,
// stdout/stderr payloads, a sleep for deadline tests, and a consume-once failure for the
// corpse-socket `-O check`. Every exec path in this package resolves the transport via
// config.SSHCommand(), so the whole plane runs offline against it.
type sshStub struct {
	bin, log string
}

const sshStubScript = `#!/bin/sh
# record argv as one \037-separated line; built first, single write, so concurrent
# invocations (the master and the socket poll) never interleave a record
sep=$(printf '\037')
line=
for a in "$@"; do line="$line$a$sep"; done
printf '%s\n' "$line" >> "$MU_TEST_SSH_LOG"
case " $* " in *" -O "*)
	# control-socket ops: fail once while the flag file exists (the corpse check), else live
	if [ -n "$MU_TEST_SSH_CHECK_FAIL_ONCE" ] && [ -e "$MU_TEST_SSH_CHECK_FAIL_ONCE" ]; then
		rm -f "$MU_TEST_SSH_CHECK_FAIL_ONCE"
		exit 1
	fi
	exit 0
	;;
esac
# exec so a context kill hits the sleeper itself, not a shell wrapping it
if [ -n "$MU_TEST_SSH_SLEEP" ]; then exec sleep "$MU_TEST_SSH_SLEEP"; fi
if [ -n "$MU_TEST_SSH_STDOUT" ]; then printf '%s\n' "$MU_TEST_SSH_STDOUT"; fi
if [ -n "$MU_TEST_SSH_STDERR" ]; then printf '%s\n' "$MU_TEST_SSH_STDERR" >&2; fi
exit "${MU_TEST_SSH_EXIT:-0}"
`

// One script shared per test process: the FIRST exec of a freshly written script pays an
// ~0.5s AV/Gatekeeper scan on macOS, so writing it per-test both pads every test and makes
// tight-deadline cases flaky. Behavior is all env-driven, so sharing the binary is safe;
// TestMain reaps the dir.
var (
	sshStubOnce sync.Once
	sshStubDir  string
	sshStubBin  string
	sshStubErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if sshStubDir != "" {
		os.RemoveAll(sshStubDir)
	}
	os.Exit(code)
}

// newSSHStub installs the shared stub as MU_SSH for the test, with a per-test argv log.
func newSSHStub(t *testing.T) *sshStub {
	t.Helper()
	sshStubOnce.Do(func() {
		sshStubDir, sshStubErr = os.MkdirTemp("", "mu-sshstub")
		if sshStubErr != nil {
			return
		}
		sshStubBin = filepath.Join(sshStubDir, "ssh-stub")
		sshStubErr = os.WriteFile(sshStubBin, []byte(sshStubScript), 0o755)
		_ = exec.Command(sshStubBin).Run() // pay the first-exec scan here, not in a test
	})
	if sshStubErr != nil {
		t.Fatal(sshStubErr)
	}
	s := &sshStub{bin: sshStubBin, log: filepath.Join(t.TempDir(), "argv.log")}
	t.Setenv("MU_SSH", s.bin)
	t.Setenv("MU_TEST_SSH_LOG", s.log)
	return s
}

// calls parses the log into one argv slice per invocation, oldest first; nil before any call.
func (s *sshStub) calls(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(s.log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out [][]string
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if ln == "" {
			continue
		}
		out = append(out, strings.Split(strings.TrimSuffix(ln, "\x1f"), "\x1f"))
	}
	return out
}

// call returns the single logged invocation whose argv contains flag.
func (s *sshStub) call(t *testing.T, flag string) []string {
	t.Helper()
	var hits [][]string
	for _, c := range s.calls(t) {
		for _, a := range c {
			if a == flag {
				hits = append(hits, c)
				break
			}
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly one call with %q, got %d in %v", flag, len(hits), s.calls(t))
	}
	return hits[0]
}

// argAfter returns the argv value following flag, "" when absent.
func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
