package cli

// Mount execution machinery for the sshfs commands: the bounded, spinner-wrapped run of
// the `sshfs` binary and its fast-fail stderr scanner. sshfs.go owns the command wiring
// and registry/group editing; this file owns the actual mount attempt. Split out because
// mounting is the one part that shells out, times out, and parses stderr — a distinct
// concern from the CRUD over the registry.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/sshfs"
)

// runMountBatch mounts several names in sequence. Non-verbose, it collapses per-mount
// chatter into one "Mounting <name>  N/M" progress line (the settle-spinner, relabelled)
// with each mount's ✓/✗ result accumulating above it, then a final "mounted X/Y" summary.
// Verbose skips the progress line and streams each mount in full (for host-key/Kerberos
// prompts and debugging). Sequential, never parallel: a mount can prompt and draws its own
// spinner, so concurrent would garble both. Returns non-zero if any mount failed.
func runMountBatch(names []string, verbose bool) int {
	total := len(names)
	failed := 0
	for i, name := range names {
		var rc int
		if verbose {
			rc = runMount(name, true, "", false)
		} else {
			rc = runMount(name, false, fmt.Sprintf("Mounting %s  %d/%d", name, i+1, total), true)
		}
		if rc != 0 {
			failed++
		}
	}
	if failed > 0 {
		render.Warn(fmt.Sprintf("mounted %d/%d (%d failed)", total-failed, total, failed))
		return 1
	}
	render.OK(fmt.Sprintf("mounted %d/%d", total, total))
	return 0
}

// Mount deadlines: a healthy sshfs daemonizes within a second or two, so these only
// bite a genuine hang. Verbose gets a longer window to answer a host-key/Kerberos
// prompt before the deadline.
const (
	mountTimeout        = 30 * time.Second
	mountTimeoutVerbose = 2 * time.Minute
)

// fatalMountErrs are sshfs/ssh stderr lines that mean the mount will never succeed —
// seeing one lets runMount fail immediately instead of waiting out the deadline
// (sshfs prints these but doesn't exit, so the line is the signal, not an exit code).
var fatalMountErrs = []string{
	"No such file or directory",
	"Not a directory",
	"Permission denied",
	"Connection refused",
	"Connection reset",
	"Could not resolve hostname",
	"Host key verification failed",
}

// stderrScanner captures sshfs stderr and signals (once) the first line matching a
// fatal pattern. Used in non-verbose mode so the mount fails fast on a bad path and
// that line — not raw noise on the spinner — is what the user sees.
type stderrScanner struct {
	buf   bytes.Buffer
	fatal chan string
	sent  bool
}

func (w *stderrScanner) Write(p []byte) (int, error) {
	w.buf.Write(p)
	if !w.sent {
		s := w.buf.String()
		for _, pat := range fatalMountErrs {
			if i := strings.Index(s, pat); i >= 0 {
				w.sent = true
				w.fatal <- fatalLine(s, i)
				break
			}
		}
	}
	return len(p), nil
}

// fatalLine returns the trimmed line of s containing byte offset i.
func fatalLine(s string, i int) string {
	start := strings.LastIndexByte(s[:i], '\n') + 1
	end := strings.IndexByte(s[i:], '\n')
	if end < 0 {
		end = len(s)
	} else {
		end += i
	}
	return strings.TrimSpace(s[start:end])
}

// runBounded runs cmd, killing it if it doesn't finish within timeout (so a wedged
// sshfs — dead server, missing remote path — can't hang the terminal). fatal, if
// non-nil, fails the mount as soon as sshfs prints a known-fatal stderr line, so a
// bad path doesn't wait out the whole deadline. Returns the exit code, whether it hit
// the timeout, and the fatal line if seen. No Setpgid: cmd stays in the terminal's
// foreground group so prompts remain answerable.
func runBounded(cmd *exec.Cmd, timeout time.Duration, fatal <-chan string) (int, bool, string) {
	if err := cmd.Start(); err != nil {
		render.Err(err.Error())
		return 1, false, ""
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return procExit(err), false, ""
	case msg := <-fatal:
		_ = cmd.Process.Kill()
		<-done
		return 1, false, msg
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return 1, true, ""
	}
}

// runMount ports sshfs.py's mount: idempotent, remounts a stale/hung mount, keeps
// stdin/stderr on the terminal so host-key/Kerberos prompts are answerable, and
// shows a spinner while sshfs settles (non-verbose only). spinLabel overrides the
// settle-spinner text (batch mode passes a "Mounting <name>  N/M" progress label);
// "" uses the default. quiet drops the pre-mount "connecting" line so a batch shows
// just its progress bar + per-mount result lines.
func runMount(name string, verbose bool, spinLabel string, quiet bool) int {
	if _, err := exec.LookPath("sshfs"); err != nil {
		render.Err("sshfs not found — install fuse-t + sshfs to use mu sshfs")
		return 3
	}
	m, ok := sshfs.ReadRegistry()[name]
	if !ok {
		render.Err(fmt.Sprintf("unknown mount: %s (see `mu sshfs list`)", name))
		return 2
	}
	mdir := sshfs.MountDir(name)

	switch sshfs.Status(name) {
	case "mounted":
		// Idempotent, but never silent: a batch prints one line per name, so a skip
		// here read as "the total clobbered my mount line".
		render.OK(name + " already mounted")
		return 0
	case "hung":
		render.Warn(name + ": stale mount — remounting")
		if !sshfs.Umount(mdir) {
			render.Err(fmt.Sprintf("%s: couldn't unmount (hung); try `diskutil unmount force %s` or a restart", name, mdir))
			return 1
		}
	}

	target, err := hpc.Resolve(m.Node)
	if err != nil {
		render.Err(err.Error())
		return 2
	}
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		render.Err(err.Error())
		return 1
	}
	// A dead ticket fails here, not 30s later under the sshfs spinner with a
	// misleading "server unreachable" — the offline/expired-CAC case.
	if err := hpc.EnsureTicket(); err != nil {
		render.Err(err.Error())
		return 1
	}

	roTag := ""
	if m.RO {
		roTag = " (ro)"
	}
	if !quiet {
		render.Info(fmt.Sprintf("connecting %s → %s%s", name, m.Node, roTag))
	}
	if verbose {
		render.Detail("  local   " + mdir)
		render.Detail("  remote  " + m.Path)
	}

	shim, err := sshfs.EnsureSSHShim()
	if err != nil {
		render.Err(err.Error())
		return 1
	}
	cmd := exec.Command("sshfs", sshfs.MountArgs(shim, target, m.Path, mdir, m.RO, verbose)...)
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	// Bound the mount so a wedged sshfs can't hang the terminal (the old failure: a bad
	// remote path left it spinning until ^Z). Non-verbose captures stderr so it can
	// fail fast on a known-fatal line and show it (instead of raw noise on the spinner);
	// verbose streams stderr to the terminal and gets a longer deadline for a prompt.
	timeout := mountTimeout
	var rc int
	var timedOut bool
	var reason string
	if verbose {
		cmd.Stderr = os.Stderr
		timeout = mountTimeoutVerbose
		rc, timedOut, reason = runBounded(cmd, timeout, nil)
	} else {
		w := &stderrScanner{fatal: make(chan string, 1)}
		cmd.Stderr = w
		label := spinLabel
		if label == "" {
			label = "mounting " + name + "…"
		}
		sp := render.NewSpinner(label)
		sp.Start()
		rc, timedOut, reason = runBounded(cmd, timeout, w.fatal)
		sp.Stop()
	}

	switch {
	case reason != "": // sshfs reported a fatal error — no point waiting out the deadline
		render.Err(fmt.Sprintf("%s: mount failed — %s", name, reason))
		_ = sshfs.Umount(mdir)
		return 1
	case timedOut:
		render.Err(fmt.Sprintf("%s: mount timed out after %s — server slow or unreachable; check `mu sshfs list` and retry with -v", name, timeout))
		_ = sshfs.Umount(mdir) // tear down a half-open mount so it isn't left hung
		return 1
	}
	if rc == 0 && sshfs.IsMounted(mdir) {
		msg := "mounted " + name
		if verbose {
			msg += " → " + m.Path
		}
		render.OK(msg + roTag)
		return 0
	}
	render.Err(fmt.Sprintf("%s: mount failed (sshfs exited %d) — retry with `mu sshfs mount %s -v`", name, rc, name))
	return 1
}

// procExit extracts a child process's exit code from exec.Cmd.Run's error.
func procExit(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}
