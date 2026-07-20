package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// mu job harness — drive a compute-node shell opened by `mu job harness open`.
//
// These verbs do ONLY local tmux IPC (send-keys/capture-pane) against the shared `mu-harness`
// socket — no ssh, no scheduler, no pkinit. Because they never authenticate, they are safe to
// run non-interactively (e.g. from a script), unlike `mu job shell/sub/tunnel`. You open the
// session (and authenticate) with `mu job harness open`; these drive it.
//
// The loop is the one proven in the prototype: flush any half-typed input (C-u), send the
// command anchored to a pinned dir and tagged with a random-nonce completion sentinel, then
// poll capture-pane (-J, so a wrapped narrow pane can't split the sentinel) until the sentinel
// prints, and slice the output between the typed line and the sentinel line.

// harnessErrCode marks a harness-MECHANISM failure (no session, timeout, tmux missing, a guard
// refusal) — kept distinct from 1/2 so a caller can tell it apart from the command's OWN exit
// code, which passes through unchanged via codeErr.
const harnessErrCode = 125

func harnessErr(format string, a ...any) error {
	return &exitErr{code: harnessErrCode, msg: fmt.Sprintf(format, a...)}
}

func jobHarnessCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "harness",
		Short: "Drive a compute-node shell opened by `mu job harness open` (local tmux; no ssh/pkinit).",
		Long: "Run commands in, and read output from, the tmux session `mu job harness open`\n" +
			"created on the shared `mu-harness` socket. These verbs touch only local tmux — no ssh,\n" +
			"no pkinit — so you can drive the pane from a script while you stay attached.\n\n" +
			"    mu job harness open -N wheat -q standard      # compute node (authenticate)\n" +
			"    mu job harness login -N wheat                 # login node: internet, no queue\n" +
			"    mu job harness run wheat 'make'               # drive the compute pane\n" +
			"    mu job harness run --login wheat 'make'       # drive the login pane\n\n" +
			"<id> is the cluster (compute → mu-shell-<cluster>; --login → mu-login-<cluster>) or a\n" +
			"full session name; see `mu job harness ls`. A login pane keeps the login node's internet\n" +
			"egress for compiling/fetching that a compute node lacks — the two coexist per cluster.\n" +
			"Front-door: `mharness <id> <cmd>` = `mu job harness run`; `mlogin <id>` = login open.",
		Args: cobra.NoArgs,
	}
	c.AddCommand(jobHarnessOpenCmd(), jobHarnessLoginCmd(), jobHarnessRunCmd(), jobHarnessKeyCmd(), jobHarnessCaptureCmd(), jobHarnessAttachCmd(), jobHarnessLsCmd(), jobHarnessPinCmd())
	return c
}

func jobHarnessOpenCmd() *cobra.Command {
	var o shellAlloc
	var dir string
	c := &cobra.Command{
		Use:   "open",
		Short: "Open an interactive allocation inside a shared-socket tmux session (you authenticate).",
		Long: "Like `mu job shell`, but the allocation runs inside a tmux session on the\n" +
			"`mu-harness` socket, so you can drive the pane with `mu job harness run`\n" +
			"while you stay attached. You authenticate in the pane; --dir starts the pane\n" +
			"in that directory (submitting the allocation from it) and anchors driven commands\n" +
			"there.\n\n" +
			"    mu job harness open -N wheat -q standard --dir ~/proj",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Outer run: re-exec THIS invocation inside tmux. The inner (MU_HARNESS_INNER set)
			// falls through to the normal allocation in the pane. Preflight the scheduler config
			// here first — inside the pane it would fail pre-auth and the pane would close before
			// the owner could read it (a silent no-op).
			if os.Getenv("MU_HARNESS_INNER") == "" {
				if err := preflightAlloc(&o); err != nil {
					return err
				}
				return launchHarness(dir, harnessSession(o.node))
			}
			return runShellAlloc(&o, dir)
		},
	}
	addShellAllocFlags(c, &o)
	c.Flags().StringVar(&dir, "dir", "", "starting directory for the pane (attach + interactive land here) and the anchor for driven commands; default: the pane's pwd on first run")
	return c
}

func jobHarnessLoginCmd() *cobra.Command {
	var node, dir string
	c := &cobra.Command{
		Use:   "login",
		Short: "Open a login-node shell inside a shared-socket tmux session (internet egress, no scheduler).",
		Long: "Like `mu job harness open`, but the pane runs on the cluster's LOGIN node over\n" +
			"`ssh -t` with no scheduler allocation — so it keeps the login node's internet egress\n" +
			"for compiling and fetching dependencies a compute node can't reach. You authenticate\n" +
			"in the pane; --dir starts the pane in that directory and anchors driven commands there.\n" +
			"Drive it with the `--login` form of the run/capture/attach verbs (session\n" +
			"mu-login-<cluster>).\n\n" +
			"    mu job harness login -N wheat --dir ~/proj\n" +
			"    mu job harness run --login wheat 'make'\n\n" +
			"For compute (used with no internet), see `mu job harness open`. Front-door: `mlogin`.\n" +
			"NOTE: for compile/fetch only — running compute here violates login-node policy.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// Outer run: re-exec THIS invocation inside tmux. The inner (MU_HARNESS_INNER set)
			// falls through to the login-node ssh in the pane. Preflight --dir/target here so a
			// bad cluster fails loud instead of dying in the pane that closes on exit.
			if os.Getenv("MU_HARNESS_INNER") == "" {
				if err := preflightLogin(node); err != nil {
					return err
				}
				return launchHarness(dir, harnessLoginSession(node))
			}
			return loginInteractive(node, dir)
		},
	}
	c.Flags().StringVarP(&node, "node", "N", "", "cluster to target (required)")
	c.Flags().StringVar(&dir, "dir", "", "starting directory for the pane (attach + interactive land here) and the anchor for driven commands; default: the pane's pwd on first run")
	completeNodeFlag(c)
	return c
}

func jobHarnessRunCmd() *cobra.Command {
	var timeout time.Duration
	var dir string
	var allowAbs, login bool
	c := &cobra.Command{
		Use:   "run <id> <cmd>...",
		Short: "Send a command to the pane, wait, print its output, and pass through its exit code.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			tmuxBin, err := harnessTmux()
			if err != nil {
				return err
			}
			session, err := resolveHarnessSession(tmuxBin, args[0], login)
			if err != nil {
				return err
			}
			cmdStr := strings.Join(args[1:], " ")
			if !allowAbs && os.Getenv("MUH_ALLOW_ABS") == "" {
				if bad := guardPath(cmdStr); bad != "" {
					return harnessErr("refused — command names a path outside the anchor: %s\n"+
						"  (pass --allow-abs or set MUH_ALLOW_ABS=1 for a legitimate absolute path, e.g. `module load /abs`)", bad)
				}
			}
			anchor, err := harnessAnchor(tmuxBin, session, dir)
			if err != nil {
				return err
			}
			out, rc, err := harnessSendWait(tmuxBin, session, wrapAnchored(anchor, cmdStr), timeout)
			if err != nil {
				return err
			}
			if out != "" {
				fmt.Print(out)
				if !strings.HasSuffix(out, "\n") {
					fmt.Println()
				}
			}
			return codeErr(rc) // the command's OWN exit becomes mu's; 0 → nil
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "how long to wait for the command to finish before giving up")
	c.Flags().StringVar(&dir, "dir", "", "anchor directory for this and later runs (default: the pane's pwd, pinned on first run)")
	c.Flags().BoolVar(&allowAbs, "allow-abs", false, "permit an absolute path / ~ / .. in the command (also MUH_ALLOW_ABS=1)")
	c.Flags().BoolVar(&login, "login", false, "target the login-node harness (mu-login-<id>) instead of the compute one")
	return c
}

func jobHarnessKeyCmd() *cobra.Command {
	var login bool
	c := &cobra.Command{
		Use:   "key <id> <key>...",
		Short: "Send raw keys to the pane (C-c, q, …) to recover a stuck prompt or pager.",
		Long: "Send key(s) straight to the pane with NO anchor, sentinel, or guard — the escape\n" +
			"hatch for when `run` can't help because the pane is not at a shell prompt:\n\n" +
			"    mu job harness key wheat C-c        # break a stuck command or quote continuation (dquote>)\n" +
			"    mu job harness key wheat q          # quit a pager (less / man / git without --no-pager)\n" +
			"    mu job harness key wheat n Enter    # answer a [y/N] prompt\n\n" +
			"Each arg is a tmux key name or literal (see `man tmux` SEND-KEYS): C-c, C-u, C-d, Enter,\n" +
			"Escape, Space, or a bare character. Fire-and-forget (a stuck pane emits no completion\n" +
			"sentinel) — follow with `capture` to see the result, then resume with `run`.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			tmuxBin, err := harnessTmux()
			if err != nil {
				return err
			}
			session, err := resolveHarnessSession(tmuxBin, args[0], login)
			if err != nil {
				return err
			}
			// Pass the key args to send-keys VERBATIM — no C-u flush (it would scroll a pager),
			// no sentinel wrap (a pager/continuation would swallow it), no guard (these are keys,
			// not a path). tmux reads each arg as a key name (C-c, Enter) or literal (q, n).
			sendArgs := append([]string{"-L", harnessSocket, "send-keys", "-t", session}, args[1:]...)
			if err := exec.Command(tmuxBin, sendArgs...).Run(); err != nil {
				return harnessErr("cannot send keys to session %q: %s", session, err)
			}
			render.Detail(fmt.Sprintf("sent %s to %s — `capture %s` to see the result", strings.Join(args[1:], " "), session, args[0]))
			return nil
		},
	}
	c.Flags().BoolVar(&login, "login", false, "target the login-node harness (mu-login-<id>) instead of the compute one")
	return c
}

func jobHarnessCaptureCmd() *cobra.Command {
	var login bool
	c := &cobra.Command{
		Use:   "capture <id>",
		Short: "Print the harness pane (what the owner sees).",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			tmuxBin, err := harnessTmux()
			if err != nil {
				return err
			}
			session, err := resolveHarnessSession(tmuxBin, args[0], login)
			if err != nil {
				return err
			}
			out, err := exec.Command(tmuxBin, "-L", harnessSocket, "capture-pane", "-p", "-J", "-S", "-", "-t", session).Output()
			if err != nil {
				return harnessErr("cannot capture session %q: %s", session, err)
			}
			fmt.Print(string(out))
			return nil
		},
	}
	c.Flags().BoolVar(&login, "login", false, "target the login-node harness (mu-login-<id>) instead of the compute one")
	return c
}

func jobHarnessAttachCmd() *cobra.Command {
	var login bool
	c := &cobra.Command{
		Use:   "attach <id>",
		Short: "Attach your terminal to the harness pane (detach with C-b d).",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			tmuxBin, err := harnessTmux()
			if err != nil {
				return err
			}
			session, err := resolveHarnessSession(tmuxBin, args[0], login)
			if err != nil {
				return err
			}
			cmd := exec.Command(tmuxBin, "-L", harnessSocket, "attach", "-t", session)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			// Drop TMUX so attach works from inside another tmux (own socket, but tmux refuses a
			// nested attach while $TMUX is set).
			cmd.Env = envWithout(os.Environ(), "TMUX", "TMUX_PANE")
			if err := cmd.Run(); err != nil {
				return harnessErr("attach failed: %s", err)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&login, "login", false, "target the login-node harness (mu-login-<id>) instead of the compute one")
	return c
}

func jobHarnessLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List open harness sessions and their anchors.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			tmuxBin, err := harnessTmux()
			if err != nil {
				return err
			}
			sessions := harnessSessions(tmuxBin)
			if len(sessions) == 0 {
				render.Info("no harness sessions open")
				return nil
			}
			render.Info(fmt.Sprintf("%d harness session(s) on socket %s:", len(sessions), harnessSocket))
			for _, s := range sessions {
				anchor := readHarnessAnchor(s)
				if anchor == "" {
					anchor = "(unset — pins on first run)"
				}
				render.Detail(fmt.Sprintf("%-22s %s", s, anchor))
			}
			return nil
		},
	}
}

func jobHarnessPinCmd() *cobra.Command {
	var login bool
	c := &cobra.Command{
		Use:   "pin <id> [dir]",
		Short: "Set the anchor directory for a session (default: the pane's pwd).",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			tmuxBin, err := harnessTmux()
			if err != nil {
				return err
			}
			session, err := resolveHarnessSession(tmuxBin, args[0], login)
			if err != nil {
				return err
			}
			var dir string
			if len(args) == 2 {
				dir = args[1]
			} else if dir, err = harnessPanePwd(tmuxBin, session); err != nil {
				return err
			}
			if err := writeHarnessAnchor(session, dir); err != nil {
				return harnessErr("cannot save anchor: %s", err)
			}
			render.Info(fmt.Sprintf("pinned %q -> %s", session, dir))
			return nil
		},
	}
	c.Flags().BoolVar(&login, "login", false, "target the login-node harness (mu-login-<id>) instead of the compute one")
	return c
}

// --- tmux + session plumbing ---

// harnessTmux resolves the tmux binary. tmux is a mise-managed tool, and a shell opened before
// `mise use -g tmux` (or without the mise shim on PATH) won't find it — so fall back to asking
// mise directly, and `mu job harness` works regardless of the caller's PATH.
func harnessTmux() (string, error) {
	if bin, err := exec.LookPath("tmux"); err == nil {
		return bin, nil
	}
	if mise, err := exec.LookPath("mise"); err == nil {
		if out, err := exec.Command(mise, "which", "tmux").Output(); err == nil {
			if p := strings.TrimSpace(string(out)); p != "" {
				return p, nil
			}
		}
	}
	return "", usageErr("tmux not found — install it (`mise use -g tmux`) or put it on PATH")
}

// harnessSessions lists the session names on the shared socket (empty if no server is up).
func harnessSessions(tmuxBin string) []string {
	out, err := exec.Command(tmuxBin, "-L", harnessSocket, "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			names = append(names, l)
		}
	}
	return names
}

// harnessHasSession reports whether a session of exactly this name is on the socket.
func harnessHasSession(tmuxBin, name string) bool {
	for _, s := range harnessSessions(tmuxBin) {
		if s == name {
			return true
		}
	}
	return false
}

// resolveHarnessSession accepts a full session name or a cluster id and returns whichever EXISTS
// on the socket. A bare id maps through harnessSession → mu-shell-<id> for the compute pane, or
// harnessLoginSession → mu-login-<id> when login is set. Exact match, not tmux's prefix/fnmatch
// target logic, so a short id can't silently hit the wrong session.
func resolveHarnessSession(tmuxBin, id string, login bool) (string, error) {
	derive := harnessSession
	if login {
		derive = harnessLoginSession
	}
	have := harnessSessions(tmuxBin)
	for _, want := range []string{id, derive(id)} {
		for _, s := range have {
			if s == want {
				return s, nil
			}
		}
	}
	if login {
		return "", harnessErr("no login harness for %q — open one with `mu job harness login -N %s`, or see `mu job harness ls`", id, id)
	}
	return "", harnessErr("no harness session for %q — open one with `mu job harness open -N %s ...`, or see `mu job harness ls`", id, id)
}

// --- anchor persistence (state, keyed by session; STATE_HOME like the tunnel registry) ---

func harnessStateDir() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "mayhl_utils", "harness")
}

func harnessAnchorPath(session string) string {
	safe := strings.NewReplacer("/", "_", " ", "_").Replace(session)
	return filepath.Join(harnessStateDir(), safe+".anchor")
}

func readHarnessAnchor(session string) string {
	b, err := os.ReadFile(harnessAnchorPath(session))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeHarnessAnchor(session, dir string) error {
	p := harnessAnchorPath(session)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(dir+"\n"), 0o644)
}

// harnessAnchor resolves the directory commands run in: an explicit --dir (persisted), else the
// stored anchor, else the pane's current pwd, auto-pinned on first use.
func harnessAnchor(tmuxBin, session, dir string) (string, error) {
	if dir != "" {
		if err := writeHarnessAnchor(session, dir); err != nil {
			return "", harnessErr("cannot save anchor: %s", err)
		}
		return dir, nil
	}
	if a := readHarnessAnchor(session); a != "" {
		return a, nil
	}
	pwd, err := harnessPanePwd(tmuxBin, session)
	if err != nil {
		return "", err
	}
	if err := writeHarnessAnchor(session, pwd); err != nil {
		return "", harnessErr("cannot save anchor: %s", err)
	}
	render.Detail(fmt.Sprintf("harness: anchored %q to %s", session, pwd))
	return pwd, nil
}

// --- the send/wait/slice loop ---

func harnessNonce() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b[:])
}

// wrapAnchored runs cmd inside ( cd anchor && { cmd; } ) so any cd it does can't drift the pane,
// and a vanished anchor short-circuits to a non-zero exit instead of running in the wrong dir.
func wrapAnchored(anchor, cmd string) string {
	return "( cd " + shell.Quote(anchor) + " && { " + cmd + " ; } )"
}

// harnessSendWait sends raw to the pane (flushing stray input first), waits up to timeout for a
// nonce completion sentinel, and returns the sliced output and the command's exit code.
func harnessSendWait(tmuxBin, session, raw string, timeout time.Duration) (string, int, error) {
	tag := "__MUH_" + harnessNonce()
	// Flush any half-typed input a human left, so it can't prepend to our command; no-op at a
	// clean prompt.
	_ = exec.Command(tmuxBin, "-L", harnessSocket, "send-keys", "-t", session, "C-u").Run()
	line := raw + ` ; echo "` + tag + `__$?__"`
	if err := exec.Command(tmuxBin, "-L", harnessSocket, "send-keys", "-t", session, line, "Enter").Run(); err != nil {
		return "", 0, harnessErr("cannot send to session %q: %s", session, err)
	}
	done := regexp.MustCompile(regexp.QuoteMeta(tag) + `__(\d+)__`)
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command(tmuxBin, "-L", harnessSocket, "capture-pane", "-p", "-J", "-S", "-", "-t", session).Output()
		if err == nil {
			if m := done.FindSubmatch(out); m != nil {
				rc, _ := strconv.Atoi(string(m[1]))
				return harnessSlice(string(out), tag), rc, nil
			}
		}
		if time.Now().After(deadline) {
			return "", 0, harnessErr("timed out after %s waiting on session %q (an unexpected prompt, or the command is still running)", timeout, session)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// harnessSlice returns the pane text between the typed command line (carrying the literal $?)
// and the sentinel output line (carrying the numeric exit code) for this tag.
func harnessSlice(pane, tag string) string {
	typed := tag + "__$?__"
	sentinel := regexp.MustCompile(regexp.QuoteMeta(tag) + `__\d+__`)
	var b strings.Builder
	started := false
	for _, ln := range strings.Split(pane, "\n") {
		if !started {
			if strings.Contains(ln, typed) {
				started = true
			}
			continue
		}
		if sentinel.MatchString(ln) {
			break
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.String()
}

// harnessPanePwd reads the pane's current working directory via a bare pwd round-trip (no
// anchor), for auto-pinning. Refuses a non-absolute result rather than pinning garbage.
func harnessPanePwd(tmuxBin, session string) (string, error) {
	out, _, err := harnessSendWait(tmuxBin, session, "pwd", 10*time.Second)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	pwd := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(pwd, "/") {
		return "", harnessErr("could not read the pane's working directory (got %q) — pass --dir", pwd)
	}
	return pwd, nil
}

// guardPath returns the first whitespace token that names a path outside the anchor (absolute,
// ~, or a .. escape), or "" if clean. Accident-prevention, NOT a sandbox: a program the command
// runs can still reach anywhere the uid can — that's the file-channel lock's job, not this.
func guardPath(cmd string) string {
	for _, tok := range strings.Fields(cmd) {
		switch {
		case strings.HasPrefix(tok, "/"), strings.HasPrefix(tok, "~"):
			return tok
		case tok == "..", strings.HasPrefix(tok, "../"), strings.HasSuffix(tok, "/.."), strings.Contains(tok, "/../"):
			return tok
		}
	}
	return ""
}
