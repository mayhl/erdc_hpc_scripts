package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/queue"
)

// The tunnel registry.
//
// A backgrounded tunnel outlives the mu that made it: the ssh master persists, the job keeps
// running, and the process that knew about both is gone. So the facts that make it
// closeable — which job, on which system, through which control socket, on which port — have
// to survive on disk. That file IS the tunnel, as far as `ls` and `close` are concerned.
//
// State, not cache: losing it doesn't cost a re-fetch, it strands a running job and a held
// port with no handle on either. Hence XDG_STATE_HOME, not XDG_CACHE_HOME.

// tunnelRec is one live tunnel. ID is mu's OWN handle, minted at submit — not the scheduler
// job id, which mu doesn't know until after submit and which is clumsy to type. It's the
// registry key, the name the job wears in the queue, and what you pass to `close`.
type tunnelRec struct {
	ID         string    `json:"id"`          // mu handle, e.g. "3f9a" (job name = mu-3f9a)
	System     string    `json:"system"`      // the cluster, as config names it
	Job        string    `json:"job"`         // scheduler job id
	Host       string    `json:"host"`        // compute node the job landed on
	Target     string    `json:"target"`      // ssh target of the login node
	Sock       string    `json:"sock"`        // the ssh ControlMaster socket
	LocalPort  int       `json:"local_port"`  // what you point a browser at
	RemotePort int       `json:"remote_port"` // what the service listens on, ON the node
	Walltime   string    `json:"walltime"`    // as requested; "" when the script decided
	Script     string    `json:"script"`      // "" in adopt mode; the staged path when Staged
	Staged     bool      `json:"staged"`      // mu pushed Script here, so `close` removes it
	Started    time.Time `json:"started"`
	// Running is when the job reached R, which is when its walltime clock starts — Started is
	// the submit, and the queue wait between them can be long. Only this one dates the job's
	// death (see expired); zero on records written before it existed, and in adopt mode it's
	// when MU saw the job running, not when it started, so it dates the death LATE. Late is the
	// safe direction: a live tunnel is never mistaken for a corpse.
	Running time.Time `json:"running,omitempty"`
}

// jobName is what mu-created ephemeral jobs (tunnel, shell) wear in the queue: mu-<id>,
// deliberately saying NOTHING about the type or the port. A job name is visible cluster-wide
// (qstat/squeue), so "mu-tun-8888" would advertise an open tunnel and its port to every other
// user — the port and the type belong only in the local registry, never on the scheduler.
func jobName(id string) string { return "mu-" + id }

// tunnelSockID names the tunnel's ssh control socket: node-port, NOT the tunnel id.
// Open and reattach must mint the SAME name — `close` and the stale-socket reap look
// for the socket where the original master put it, so this is a cross-verb contract.
func tunnelSockID(system string, localPort int) string {
	return fmt.Sprintf("%s-%d", system, localPort)
}

// newTunnelID mints a short random handle (2 bytes → 4 hex). Random, not sequential: the
// name must carry no information — a counter would leak how many tunnels you've opened.
func newTunnelID() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000" // vanishingly unlikely; a fixed fallback beats failing a submit
	}
	return hex.EncodeToString(b[:])
}

// URL is where the tunnel answers.
func (t tunnelRec) URL() string { return fmt.Sprintf("http://localhost:%d", t.LocalPort) }

// tunnelDir is where the registry lives — STATE, not cache: see the note above.
func tunnelDir() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "mayhl_utils", "tunnels")
}

// tunnelPath keys a tunnel by its mu handle — unique by construction, and the string you'd
// type at `close`.
func tunnelPath(id string) string {
	safe := strings.NewReplacer("/", "_", " ", "_").Replace(id)
	return filepath.Join(tunnelDir(), safe+".json")
}

// saveTunnel records a live tunnel. A registry mu couldn't write is worth failing over —
// unlike a cache, the alternative is a job and a port nobody can find again.
func saveTunnel(t tunnelRec) error {
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	p := tunnelPath(t.ID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// loadTunnels reads the registry, newest first. An unreadable entry is skipped, not fatal:
// one corrupt file must not hide the tunnels you can still close.
func loadTunnels() []tunnelRec {
	ents, err := os.ReadDir(tunnelDir())
	if err != nil {
		return nil
	}
	var out []tunnelRec
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(tunnelDir(), e.Name()))
		if err != nil {
			continue
		}
		var t tunnelRec
		if json.Unmarshal(b, &t) == nil && t.Job != "" {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// findTunnel resolves what the user typed to one record. The mu handle is the intended key,
// but the scheduler job id is accepted too (with or without its system) — you might read it
// off qstat rather than `ls`. An ambiguous match is an error, not a guess.
func findTunnel(ref string) (tunnelRec, error) {
	var hits []tunnelRec
	for _, t := range loadTunnels() {
		switch {
		case t.ID == ref,
			t.Job == ref, strings.HasPrefix(t.Job, ref+"."), t.System+"/"+t.Job == ref:
			hits = append(hits, t)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return tunnelRec{}, usageErr("no tunnel %q — `mu job tunnel ls` shows the open ones", ref)
	default:
		return tunnelRec{}, usageErr("%q matches several tunnels — use the id from `mu job tunnel ls`", ref)
	}
}

func forgetTunnel(t tunnelRec) { _ = os.Remove(tunnelPath(t.ID)) }

// masterAlive reports whether the tunnel's ssh master is still up.
//
// The forward is a channel ON the master, so this — not the scheduler — is what says whether
// the TUNNEL is live: a laptop that slept dropped the master while the job ran happily on. And
// it costs nothing to ask. `-O check` queries the master through its unix socket, a local
// process: no network, no ticket, no pkinit. That's what lets `ls` render a registry full of
// corpses without dialling a cluster.
//
// Socket gone ⇒ master gone (ssh unlinks it on exit). Socket present but unanswered ⇒ a corpse
// left by a kill, which is exactly what OpenSession reaps before opening its own.
func masterAlive(t tunnelRec) bool {
	if t.Sock == "" {
		return false
	}
	if _, err := os.Stat(t.Sock); err != nil {
		return false
	}
	return exec.Command(config.SSHCommand(), "-q", "-S", t.Sock, "-O", "check", t.Target).Run() == nil
}

// expired reports whether the tunnel's JOB has certainly ended, from the clock alone — the
// scheduler kills at walltime, so a walltime spent since the job started running means there is
// nothing left to reattach to. Proof rather than a guess, and free: no query, no ticket.
//
// It answers false whenever it can't prove death: an unknown walltime (the script named its own
// and mu never learned the number) or an unset Running (a record from before mu tracked it).
// Unprovable is not dead — dropping a record whose job still runs strands the job, which is the
// one thing this registry exists to prevent.
func expired(t tunnelRec) bool {
	if t.Running.IsZero() {
		return false
	}
	secs, ok := queue.ParseWalltime(strings.TrimSpace(t.Walltime))
	if !ok {
		return false
	}
	return time.Since(t.Running) > time.Duration(secs)*time.Second
}

// portFree reports whether a local port can be listened on. Binding is the only honest test
// — /proc and lsof both lie about what a bind will actually do.
func portFree(p int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// pickLocalPort chooses the local end of the forward.
//
// It starts at the REMOTE port, so `-p 8888` gives you localhost:8888 and the URL is the one
// you'd guess. Only when that's taken does it walk upward. A port the user NAMED (-l) is
// never silently moved: refusing is the honest answer, since quietly forwarding 3123 to 3124
// would leave them staring at whatever else holds 3123.
func pickLocalPort(want, remote int) (int, error) {
	if want != 0 {
		if want < 1024 {
			return 0, usageErr("local port %d is privileged (<1024) — pick one above 1024", want)
		}
		if !portFree(want) {
			return 0, usageErr("local port %d is already in use — pick another, or omit -l and mu will take the next free port above --port", want)
		}
		return want, nil
	}
	start := max(remote, 1024)
	for p := start; p < start+200 && p < 65535; p++ {
		if portFree(p) {
			return p, nil
		}
	}
	return 0, runErr("no free local port in %d..%d", start, start+200)
}
