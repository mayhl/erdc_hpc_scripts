package hpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// RemoteExec runs remoteCmd on target's login node over the transport ssh (MU_SSH),
// inside a login bash so /etc/profile.d puts the scheduler on PATH — `bash -lc`, the
// same reason the shell dispatcher uses bash not zsh. stdout is returned; the
// cluster's benign dbus/X11 login-profile noise is dropped from stderr
// (MU_SSH_STDERR_FILTER), the rest passed through so real errors still surface. The
// command is single-quoted so its own quotes/pipes reach the remote bash intact.
// A bounded ConnectTimeout fails a dead host fast rather than hanging, and a latency
// spinner reassures once the call outlasts spinnerDelay.
func RemoteExec(target, remoteCmd string) (string, error) {
	ssh := config.SSHCommand()
	arg := "bash -lc " + shell.Quote(remoteCmd)
	args := []string{"-q", "-o", fmt.Sprintf("ConnectTimeout=%d", connectSeconds())}
	args = append(args, controlArgs(target)...)
	args = append(args, target, arg)
	cmd := exec.Command(ssh, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	stop := armSpinner(hostOf(target))
	err := cmd.Run()
	stop()
	if s := filterStderr(stderr.String()); s != "" {
		fmt.Fprint(os.Stderr, s)
	}
	if err != nil {
		return stdout.String(), errors.New(classify(target, err))
	}
	return stdout.String(), nil
}

// RemoteProbe runs remoteCmd on target for completion/autocomplete. It rides an EXISTING
// ControlMaster socket if one is up (instant, already authenticated) but NEVER creates a
// master and NEVER prompts, so a cold host fails fast instead of blocking a TAB or
// triggering a CAC/PIN prompt. We gate on the socket itself: ControlMaster=no still opens a
// FRESH connection when the socket is absent (a ~2.6s connect+login round-trip that blows the
// deadline anyway), so we stat controlPath first and bail if there's no live master — that
// makes "master-only" real rather than an accident of the timeout. BatchMode+ConnectTimeout
// stay as belt-and-suspenders against a racing socket teardown. Returns the output and
// whether it succeeded within the deadline. The transport is MU_SSH, same as RemoteExec.
func RemoteProbe(target, remoteCmd string) (string, bool) {
	if _, err := os.Stat(controlPath(target)); err != nil {
		return "", false // no ambient master → no completion (never a fresh connect)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	args := []string{
		"-q",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=1",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=" + controlPath(target),
		target, remoteCmd,
	}
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, config.SSHCommand(), args...)
	cmd.Stdout = &buf
	err := cmd.Run()
	return buf.String(), err == nil && ctx.Err() == nil
}

// connectTimeout (seconds) bounds ssh's connect phase for the interactive
// single-host path, so a dead login node fails fast (via classify → "unreachable")
// instead of hanging on TCP defaults. Overridable via MU_SSH_CONNECT_TIMEOUT. It
// bounds only connection setup, not auth or the remote command's runtime, so a long
// qstat is never cut off. The fan-out path (RemoteExecTimeout) sets its own.
const connectTimeout = 10

func connectSeconds() int {
	if v := os.Getenv("MU_SSH_CONNECT_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return connectTimeout
}

// controlPersist (seconds) is how long an ambient RemoteExec master lingers after its last use,
// warm for the next verb. Short: long enough that a workflow's back-to-back calls (a submit
// then its sweep, a fleet of reads) reuse one auth, short enough not to hold an idle connection
// to a login node. 0 via MU_SSH_CONTROL_PERSIST disables reuse entirely (fresh ssh per call, the
// old behavior) — the escape hatch for a site where multiplexing misbehaves.
const controlPersist = 30

func controlPersistSeconds() int {
	if v := os.Getenv("MU_SSH_CONTROL_PERSIST"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n // 0 disables; negative is clamped away below
		}
	}
	return controlPersist
}

// controlArgs turns on OpenSSH connection multiplexing for RemoteExec: every call to the same
// target within the persist window rides ONE ControlMaster, so the whole run authenticates once
// instead of once per command — the cost that matters on a Kerberos cluster. ssh owns the
// master's lifecycle (auto-create on first use with ControlMaster=auto, persist a bounded time
// after the last client, remove the socket on exit), so there is nothing for mu to open or
// close. Empty (reuse off) when the persist window is non-positive.
func controlArgs(target string) []string {
	persist := controlPersistSeconds()
	if persist <= 0 {
		return nil
	}
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + controlPath(target),
		"-o", fmt.Sprintf("ControlPersist=%d", persist),
	}
}

// controlPath is the ambient master's control socket: stable per target (so calls reuse it) and
// short (a 12-hex digest of the target under the temp dir, well inside the ~104-char unix-socket
// path limit — the target itself, user@host, could blow it).
func controlPath(target string) string {
	h := sha256.Sum256([]byte(target))
	return filepath.Join(os.TempDir(), "mu-cm-"+hex.EncodeToString(h[:6]))
}

// AmbientTransport is the rsync -e ssh command that joins target's ambient ControlMaster:
// the transfer becomes a channel on the same socket every RemoteExec call shares, so a
// flow's exec round-trips and rsync legs all cost ONE auth. rsync splits -e on whitespace
// itself (no shell), so the flags ride the string safely. Plain transport (fresh
// connection per rsync, the old behavior) when reuse is off (MU_SSH_CONTROL_PERSIST=0).
func AmbientTransport(target string) string {
	t := strings.TrimSpace(config.SSHCommand() + " " + config.SSHTransferOpts())
	if opts := controlArgs(target); len(opts) > 0 {
		t += " " + strings.Join(opts, " ")
	}
	return t
}

// spinnerDelay is how long a remote call must run before the latency spinner
// appears — long enough that a fast LAN/cached call never flickers one, short
// enough to reassure on a slow login that mu isn't wedged.
const spinnerDelay = 400 * time.Millisecond

// armSpinner shows a "querying <host>" spinner if the caller hasn't stopped it
// within spinnerDelay, and returns a stop func that cancels the pending spinner
// (never shown) or clears it (shown). Single-host only — the concurrent fan-out
// would interleave frames. TTY-gating lives in render.Spinner (a no-op off-TTY).
func armSpinner(host string) func() {
	var (
		mu   sync.Mutex
		sp   *render.Spinner
		done bool
	)
	timer := time.AfterFunc(spinnerDelay, func() {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return
		}
		sp = render.NewSpinner("querying " + host + "…")
		sp.Start()
	})
	return func() {
		mu.Lock()
		defer mu.Unlock()
		done = true
		timer.Stop()
		if sp != nil {
			sp.Stop()
		}
	}
}

// LocalExec is the on-cluster counterpart to RemoteExec: it runs remoteCmd on the
// current login node with no ssh (mu is already on the box). Same login bash (`bash -lc`)
// as the remote arm so the scheduler command resolves identically — whether it's a PATH
// binary or a profile-defined wrapper — and the same benign login-profile noise is
// filtered from stderr. Two guarantees are inherited from RemoteExec by construction, not
// a shell pipe: only the interactive shell shows the login banner (a non-interactive
// `bash -lc` triggers no MOTD, the local mirror of the dispatcher's `ssh -q`), and stdout
// and stderr stay in SEPARATE buffers so login noise never contaminates the parsed stdout
// and the command's real exit code survives (a `2>&1 | grep` would lose both — the
// dispatcher avoids it with `2> >(grep …)` process substitution; here Go buffers do it).
// No reachability probe on the error path: there's no host to dial, so a failure is just
// its exit-code text.
func LocalExec(remoteCmd string) (string, error) {
	cmd := exec.Command("bash", "-lc", remoteCmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if s := filterStderr(stderr.String()); s != "" {
		fmt.Fprint(os.Stderr, s)
	}
	if err != nil {
		return stdout.String(), errors.New(exitText(err))
	}
	return stdout.String(), nil
}

// RemoteExecTimeout is RemoteExec bounded by a deadline — for concurrent
// cross-cluster fan-out where a wedged or unreachable host must never hang the
// whole collate. ssh ConnectTimeout fails an unreachable host fast; the context
// deadline kills anything still running past timeout. Unlike RemoteExec it does NOT
// print stderr (concurrent hosts would interleave) — a failure's first real stderr
// line is folded into the returned error, and a deadline hit returns a clean
// "timeout after …".
func RemoteExecTimeout(target, remoteCmd string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ssh := config.SSHCommand()
	arg := "bash -lc " + shell.Quote(remoteCmd)
	connTO := int(timeout.Seconds())
	if connTO < 1 {
		connTO = 1
	}
	// Same ambient ControlMaster as RemoteExec: the collate fan-out otherwise pays a
	// full Kerberos auth per call — twice per target when the hooks fetch runs beside
	// the snapshot. A deadline-killed client can't strand the master (ssh forks it,
	// ControlPersist expires it).
	args := []string{"-q", "-o", fmt.Sprintf("ConnectTimeout=%d", connTO)}
	args = append(args, controlArgs(target)...)
	args = append(args, target, arg)
	cmd := exec.CommandContext(ctx, ssh, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return stdout.String(), fmt.Errorf("timeout after %s", timeout)
	}
	if err != nil {
		if msg := firstLine(filterStderr(stderr.String())); msg != "" {
			return stdout.String(), errors.New(msg)
		}
		return stdout.String(), errors.New(classify(target, err))
	}
	return stdout.String(), nil
}

// firstLine returns the first non-blank line of s, trimmed — a terse error summary.
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// reachTimeout bounds the on-failure reachability probe.
const reachTimeout = 3 * time.Second

// classify turns a bare remote-exec failure (no remote stderr to show) into a human
// reason. It runs ONLY on the error path: a quick TCP dial of the login node's ssh
// port distinguishes an unreachable host (down/network) from a host that answered
// but couldn't log us in (auth/ticket). Assumes direct-to-login-node ssh (no proxy),
// so the dialed endpoint is the one ssh uses.
func classify(target string, err error) string {
	if !reachable(hostOf(target)) {
		return "unreachable (host down or network)"
	}
	return exitText(err)
}

// hostOf strips any "user@" from an ssh target, leaving the host to dial.
func hostOf(target string) string {
	if i := strings.LastIndex(target, "@"); i >= 0 {
		return target[i+1:]
	}
	return target
}

// reachable reports whether the host's ssh port accepts a TCP connection.
func reachable(host string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), reachTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// exitText names a failure once the host is known reachable, so the cause is the
// login itself. ssh's catch-all 255 then means a Kerberos problem — a missing ticket
// (the common, fixable case, detected locally) or one the server rejected. ON an HPC
// node the local-cache diagnosis doesn't apply: there's often no cache at all (the
// credential arrived by delegation with the login), and pkinit/--renew don't exist
// there — so the hint points at delegation instead.
func exitText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 255:
			if os.Getenv("BC_HOST") != "" || os.Getenv("MU_SYSTEM") == "hpc" {
				return "authentication failed — credentials may not delegate to that host from here"
			}
			if info, ok := Ticket(); ok && !info.Present {
				return "no Kerberos ticket — run `mu hpc ticket --renew`"
			}
			return "authentication failed — ticket expired or rejected (check `mu hpc ticket`)"
		case 127:
			return "scheduler command not found on PATH"
		case 126:
			return "permission denied"
		}
	}
	return err.Error()
}

// StderrNoise is the pattern for the cluster's benign login-profile chatter (the dbus/X11
// lines) — noise every remote session emits and no one wants to read. Overridable via
// MU_SSH_STDERR_FILTER, matching the shell dispatcher's grep filter. nil when the pattern
// is unparseable: filter nothing rather than swallow something. Exported because an
// INTERACTIVE session (`mu job shell`) sees the same noise on its pty and has to drop it
// itself — RemoteExec's own capture-and-filter can't reach a stream mu is passing through.
func StderrNoise() *regexp.Regexp {
	pat := os.Getenv("MU_SSH_STDERR_FILTER")
	if pat == "" {
		pat = `dbus-update-activation-environment|^Cannot continue`
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil
	}
	return re
}

// filterStderr drops that noise from a captured stderr so only real remote errors surface.
func filterStderr(s string) string {
	if s == "" {
		return ""
	}
	re := StderrNoise()
	if re == nil {
		return s
	}
	var keep []string
	for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(ln) == "" || re.MatchString(ln) {
			continue
		}
		keep = append(keep, ln)
	}
	if len(keep) == 0 {
		return ""
	}
	return strings.Join(keep, "\n") + "\n"
}
