package hpc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRsyncTransport pins the -e string rsync rides the session's master with: it must name
// the ssh binary, point at the control socket, and force ControlMaster=no so rsync's ssh is a
// CLIENT of the existing master, never a second master.
func TestRsyncTransport(t *testing.T) {
	s := &Session{bin: "ssh", sock: "/tmp/mu-mux-42", target: "u@host"}
	tr := s.RsyncTransport()
	for _, want := range []string{"ssh", "-S /tmp/mu-mux-42", "ControlMaster=no"} {
		if !strings.Contains(tr, want) {
			t.Errorf("RsyncTransport() = %q, missing %q", tr, want)
		}
	}
}

// TestSessionSock: the socket path is the handle the tunnel registry stores.
func TestSessionSock(t *testing.T) {
	s := &Session{sock: "/tmp/mu-tun-abc"}
	if got := s.Sock(); got != "/tmp/mu-tun-abc" {
		t.Errorf("Sock() = %q", got)
	}
}

// TestOpenSessionArgv pins the master's flag set through the MU_SSH stub: both modes hold
// with -M -N and the keepalive opts; only Persist adds -f + ControlPersist=yes (the
// background fork) and names the socket for the ID instead of the pid.
func TestOpenSessionArgv(t *testing.T) {
	cases := []struct {
		name   string
		opts   SessionOpts
		sock   string   // expected socket basename under TMPDIR
		want   []string // must appear in the master argv
		reject []string // must not
	}{
		{
			"foreground",
			SessionOpts{},
			fmt.Sprintf("mu-mux-%d", os.Getpid()),
			[]string{"-q", "-x", "-M", "-N", "ConnectTimeout=10", "ServerAliveInterval=30", "ServerAliveCountMax=5"},
			[]string{"-f", "ControlPersist=yes"},
		},
		{
			"persist",
			SessionOpts{Persist: true, ID: "abc"},
			"mu-tun-abc",
			[]string{"-q", "-x", "-M", "-N", "-f", "ControlPersist=yes"},
			nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := newSSHStub(t)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp) // os.TempDir honors it, so socket paths land in the test dir
			s, err := OpenSession("u@host", c.opts)
			if err != nil {
				t.Fatalf("OpenSession: %v", err)
			}
			defer s.Close()
			master := stub.call(t, "-M")
			joined := " " + strings.Join(master, " ") + " "
			for _, w := range c.want {
				if !strings.Contains(joined, " "+w+" ") {
					t.Errorf("master argv %v missing %q", master, w)
				}
			}
			for _, r := range c.reject {
				if strings.Contains(joined, " "+r+" ") {
					t.Errorf("master argv %v must not carry %q", master, r)
				}
			}
			wantSock := filepath.Join(tmp, c.sock)
			if got := argAfter(master, "-S"); got != wantSock {
				t.Errorf("master -S = %q, want %q", got, wantSock)
			}
			if s.Sock() != wantSock {
				t.Errorf("Sock() = %q, want %q", s.Sock(), wantSock)
			}
			if master[len(master)-1] != "u@host" {
				t.Errorf("master argv %v must end with the target", master)
			}
			// the readiness poll probes the same socket
			if check := stub.call(t, "check"); argAfter(check, "-S") != wantSock {
				t.Errorf("readiness check argv %v not on %q", check, wantSock)
			}
		})
	}
}

// TestOpenSessionCorpseSocket: a leftover persist socket nothing live answers is reaped so
// the new master isn't wedged; one a live master answers is left alone. The stub fails the
// first `-O check` (consuming a flag file) for the corpse case.
func TestOpenSessionCorpseSocket(t *testing.T) {
	plant := func(t *testing.T) (stub *sshStub, sock string) {
		t.Helper()
		stub = newSSHStub(t)
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		sock = filepath.Join(tmp, "mu-tun-old")
		if err := os.WriteFile(sock, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return stub, sock
	}

	t.Run("dead socket reaped", func(t *testing.T) {
		stub, sock := plant(t)
		flag := filepath.Join(t.TempDir(), "check-fail-once")
		if err := os.WriteFile(flag, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("MU_TEST_SSH_CHECK_FAIL_ONCE", flag)
		s, err := OpenSession("u@host", SessionOpts{Persist: true, ID: "old"})
		if err != nil {
			t.Fatalf("OpenSession over a corpse: %v", err)
		}
		defer s.Close()
		if _, err := os.Stat(sock); !os.IsNotExist(err) {
			t.Errorf("corpse socket not reaped (stat err = %v)", err)
		}
		// first invocation is the liveness probe of the planted socket
		calls := stub.calls(t)
		if len(calls) < 2 || argAfter(calls[0], "-O") != "check" || argAfter(calls[0], "-S") != sock {
			t.Errorf("first call not the corpse check: %v", calls)
		}
	})

	t.Run("live socket left alone", func(t *testing.T) {
		_, sock := plant(t)
		s, err := OpenSession("u@host", SessionOpts{Persist: true, ID: "old"})
		if err != nil {
			t.Fatalf("OpenSession: %v", err)
		}
		defer s.Close()
		if _, err := os.Stat(sock); err != nil {
			t.Errorf("live socket must survive: %v", err)
		}
	})
}

// TestSessionRun: stdout passes through on success; on failure the trimmed stderr becomes
// the error, falling back to the exit text when the remote said nothing. The channel rides
// the held socket with the command wrapped in a single-quoted `bash -lc`.
func TestSessionRun(t *testing.T) {
	stub := newSSHStub(t)
	s := &Session{bin: stub.bin, sock: "/tmp/mu-mux-t", target: "u@host"}
	cases := []struct {
		name, stdout, stderr, exit string
		wantOut, wantErr           string
	}{
		{"stdout through", "hi", "", "0", "hi\n", ""},
		{"stderr becomes the error", "", "remote: boom", "1", "", "remote: boom"},
		{"silent failure falls back to exit text", "", "", "2", "", "exit status 2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("MU_TEST_SSH_STDOUT", c.stdout)
			t.Setenv("MU_TEST_SSH_STDERR", c.stderr)
			t.Setenv("MU_TEST_SSH_EXIT", c.exit)
			out, err := s.Run("qstat -x")
			if out != c.wantOut {
				t.Errorf("out = %q, want %q", out, c.wantOut)
			}
			switch {
			case c.wantErr == "" && err != nil:
				t.Errorf("err = %v, want nil", err)
			case c.wantErr != "" && (err == nil || err.Error() != c.wantErr):
				t.Errorf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
	calls := stub.calls(t)
	args := calls[len(calls)-1]
	if got := argAfter(args, "-S"); got != "/tmp/mu-mux-t" {
		t.Errorf("Run -S = %q, want the session socket", got)
	}
	if got, want := args[len(args)-1], "bash -lc 'qstat -x'"; got != want {
		t.Errorf("remote arg = %q, want %q", got, want)
	}
	if args[len(args)-2] != "u@host" {
		t.Errorf("argv %v: target not before the command", args)
	}
}
