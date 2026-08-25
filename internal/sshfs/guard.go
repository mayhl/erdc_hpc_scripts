package sshfs

// The sshfs guard: an unattended tick (a launchd agent, every ~30 s) that parks the
// mounts the laptop's AV is grinding through and unmounts the ones nobody has touched
// for a while. Both hazards only bite while a mount is up idle: McAfee traverses fuse-t
// mounts end to end (multi-GB NetCDF over the wire, tape staged just to scan it — the
// VShieldScanner pin), and an idle mount's `-o reconnect` retries are what pop CAC
// prompts during a VPN drop. This file is the engine — sampling, state, and the pure
// decision; the command + launchd wiring live in internal/cli.
//
// Detection is a conjunction, because no single signal is attributable unprivileged:
// the scanner's %cpu (root, but visible via ps) says the AV is busy without saying
// where; the per-mount fuse-t daemons (go-nfsv4 + sshfs, user-owned, mount dir in argv)
// say which mount is being read without saying by whom; lsof (own processes only)
// says whether the OWNER is in the mount. AV-busy + daemon-busy + owner-absent = park.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GuardTuning is the guard's knobs. Zero Idle disables idle-unmount.
type GuardTuning struct {
	ScannerHot   float64       // AV scanner %cpu (summed) that counts as "scanning"
	DaemonHot    float64       // per-mount daemon %cpu that counts as "being read"
	SustainTicks int           // scanner must be hot this many consecutive ticks
	Park         time.Duration // how long an AV-parked mount stays down before remounting
	Idle         time.Duration // unmount after this long with no owner activity (0 = never)
	TripLimit    int           // AV parks within TripWindow before the mount is benched
	TripWindow   time.Duration
}

// DefaultGuardTuning: ps's %cpu is a decaying ~1-min average, so 20% summed across the
// scanner processes is well clear of its background hum (~5-10%) yet far below the ~90%
// of a mount traversal; two 30 s ticks filter a scan of local files that merely passes
// through. A minute down lets a scheduled on-demand walk skip the (now empty) dir and
// move on; three parks in 15 min means it keeps coming back, so stop cycling.
func DefaultGuardTuning() GuardTuning {
	return GuardTuning{
		ScannerHot: 20, DaemonHot: 5, SustainTicks: 2,
		Park: time.Minute, Idle: 30 * time.Minute,
		TripLimit: 3, TripWindow: 15 * time.Minute,
	}
}

// GuardMount is the guard's memory of one registered mount.
type GuardMount struct {
	LastActive time.Time   `json:"last_active,omitempty"` // last tick the owner was seen using it
	Parked     time.Time   `json:"parked,omitempty"`      // when the guard took it down; zero = not parked
	Reason     string      `json:"reason,omitempty"`      // "av" | "idle"
	Trips      []time.Time `json:"trips,omitempty"`       // AV parks (pruned to TripWindow)
	Benched    bool        `json:"benched,omitempty"`     // trip limit hit: stays down until mounted by hand (warned once)
}

// GuardState is the on-disk tick-to-tick state (STATE, not cache: it holds which mounts
// the guard owes a remount).
type GuardState struct {
	ScannerHotTicks int                    `json:"scanner_hot_ticks"`
	Mounts          map[string]*GuardMount `json:"mounts"`
}

// GuardStatePath is the guard's state file, beside the ssh shim.
func GuardStatePath() string { return filepath.Join(stateDir(), "mayhl_utils", "sshfs-guard.json") }

// LoadGuardState reads the state; missing or unreadable → empty (the guard re-learns).
func LoadGuardState() *GuardState {
	st := &GuardState{Mounts: map[string]*GuardMount{}}
	b, err := os.ReadFile(GuardStatePath())
	if err == nil {
		_ = json.Unmarshal(b, st)
	}
	if st.Mounts == nil {
		st.Mounts = map[string]*GuardMount{}
	}
	return st
}

// SaveGuardState writes the state atomically enough for a 30 s tick (temp + rename).
func SaveGuardState(st *GuardState) error {
	p := GuardStatePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Parked returns the guard's reason a mount is down ("av" | "idle"), or "" if the guard
// didn't take it down — the `mu sshfs list` overlay.
func (st *GuardState) Parked(name string) string {
	if m, ok := st.Mounts[name]; ok && !m.Parked.IsZero() {
		return m.Reason
	}
	return ""
}

// --- sampling ---------------------------------------------------------------

// Proc is one row of `ps -axo pid=,user=,%cpu=,command=`.
type Proc struct {
	PID  int
	User string
	CPU  float64
	Cmd  string
}

// SampleProcs snapshots every process (all users — the scanner is root's).
func SampleProcs() []Proc {
	out, ok := runOut(5*time.Second, "ps", "-axo", "pid=,user=,%cpu=,command=")
	if !ok {
		return nil
	}
	return ParsePS(out)
}

// ParsePS parses ps output into Procs; malformed lines are skipped. Pure.
func ParsePS(out string) []Proc {
	var procs []Proc
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		cpu, err2 := strconv.ParseFloat(f[2], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		procs = append(procs, Proc{PID: pid, User: f[1], CPU: cpu, Cmd: strings.Join(f[3:], " ")})
	}
	return procs
}

// scannerProcs are the AV's on-access/on-demand scan workers (McAfee/Trellix on the
// DoD image); matched on the executable's basename so the install path doesn't matter.
var scannerProcs = map[string]bool{"VShieldScanner": true}

// mountDaemons are fuse-t's per-mount processes, each carrying its mount dir in argv.
var mountDaemons = map[string]bool{"go-nfsv4": true, "sshfs": true}

func procName(cmd string) string {
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return ""
	}
	return filepath.Base(f[0])
}

// ScannerCPU sums the AV scanner workers' %cpu.
func ScannerCPU(procs []Proc) float64 {
	var sum float64
	for _, p := range procs {
		if scannerProcs[procName(p.Cmd)] {
			sum += p.CPU
		}
	}
	return sum
}

// DaemonCPU sums each mount's fuse-t daemon %cpu, keyed by mount dir: a mount whose
// daemons are busy is being read by someone. Only dirs under mountsRoot count.
func DaemonCPU(procs []Proc, mountsRoot string) map[string]float64 {
	out := map[string]float64{}
	prefix := mountsRoot + string(filepath.Separator)
	for _, p := range procs {
		if !mountDaemons[procName(p.Cmd)] {
			continue
		}
		for _, tok := range strings.Fields(p.Cmd) {
			if strings.HasPrefix(tok, prefix) {
				out[tok] += p.CPU
				break
			}
		}
	}
	return out
}

// HeldByUser reports, per dir, whether one of the OWNER's processes has a file or its
// cwd on that filesystem — the "don't yank it out from under me" check. One lsof call
// for all dirs (~1 s), timeout-bounded; `-a -u <uid>` ANDs the user filter so a root
// scanner holding files never reads as owner activity. lsof exits 1 when nothing is
// open, which runOut reports as not-ok — the same answer as "held by nobody".
func HeldByUser(dirs []string) map[string]bool {
	held := map[string]bool{}
	if len(dirs) == 0 {
		return held
	}
	args := append([]string{"-a", "-u", strconv.Itoa(os.Getuid()), "-F", "n", "+f", "--"}, dirs...)
	out, ok := runOut(10*time.Second, "lsof", args...)
	if !ok {
		return held
	}
	return heldDirs(dirs, parseLsofNames(out))
}

// parseLsofNames pulls the `n<path>` fields out of `lsof -F n` output. Pure.
func parseLsofNames(out string) []string {
	var names []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "n") {
			names = append(names, ln[1:])
		}
	}
	return names
}

// heldDirs marks each dir that an open path sits on (the dir itself, or under it). Pure.
func heldDirs(dirs, names []string) map[string]bool {
	held := map[string]bool{}
	for _, d := range dirs {
		for _, n := range names {
			if n == d || strings.HasPrefix(n, d+string(filepath.Separator)) {
				held[d] = true
				break
			}
		}
	}
	return held
}

// --- decision ---------------------------------------------------------------

// Observation is one registered mount as seen this tick.
type Observation struct {
	Name      string
	Mounted   bool
	DaemonCPU float64
	Held      bool
}

// Action is one thing the tick should do. Op: "park" (AV) | "idle" | "remount" | "bench".
type Action struct {
	Name, Op, Why string
}

// Decide advances the state one tick and returns the actions to take. Pure (clock and
// ticket state injected). Mounts absent from obs (removed from the registry) are
// forgotten. The caller performs the actions; a failed park should clear Parked and a
// failed remount should re-stamp it so the next attempt waits out another cooldown.
func Decide(st *GuardState, scannerCPU float64, obs []Observation, now time.Time, tun GuardTuning, ticketOK bool) []Action {
	if scannerCPU >= tun.ScannerHot {
		st.ScannerHotTicks++
	} else {
		st.ScannerHotTicks = 0
	}
	scannerHot := st.ScannerHotTicks >= tun.SustainTicks

	var actions []Action
	seen := map[string]bool{}
	for _, o := range obs {
		seen[o.Name] = true
		m := st.Mounts[o.Name]
		if m == nil {
			m = &GuardMount{}
			st.Mounts[o.Name] = m
		}
		m.Trips = pruneTrips(m.Trips, now, tun.TripWindow)

		if !o.Mounted {
			switch {
			case m.Parked.IsZero(): // down by the owner's hand — nothing owed
				m.LastActive = time.Time{}
			case m.Reason != "av": // idle-parked stays down until an hmt
			case len(m.Trips) >= tun.TripLimit:
				if !m.Benched {
					m.Benched = true
					actions = append(actions, Action{
						o.Name, "bench",
						fmt.Sprintf("parked %d× in %s — staying down until you mount it by hand", len(m.Trips), tun.TripWindow),
					})
				}
			case now.Sub(m.Parked) >= tun.Park && ticketOK:
				actions = append(actions, Action{o.Name, "remount", "cooldown over"})
			}
			continue
		}

		// Up (by us or by hand): whatever we owed is settled; trips stay so a mount the
		// AV keeps returning to still benches.
		m.Parked, m.Reason, m.Benched = time.Time{}, "", false
		active := o.Held || (o.DaemonCPU >= tun.DaemonHot && scannerCPU < tun.ScannerHot)
		if active || m.LastActive.IsZero() {
			m.LastActive = now
		}
		switch {
		case scannerHot && o.DaemonCPU >= tun.DaemonHot && !o.Held:
			m.Trips = append(m.Trips, now)
			m.Parked, m.Reason = now, "av"
			actions = append(actions, Action{
				o.Name, "park",
				fmt.Sprintf("AV scan (scanner %.0f%%, daemon %.0f%%)", scannerCPU, o.DaemonCPU),
			})
		case tun.Idle > 0 && !o.Held && now.Sub(m.LastActive) >= tun.Idle:
			m.Parked, m.Reason = now, "idle"
			actions = append(actions, Action{
				o.Name, "idle",
				fmt.Sprintf("unused for %s", now.Sub(m.LastActive).Round(time.Minute)),
			})
		}
	}
	for name := range st.Mounts {
		if !seen[name] {
			delete(st.Mounts, name)
		}
	}
	return actions
}

// pruneTrips drops trips older than window.
func pruneTrips(trips []time.Time, now time.Time, window time.Duration) []time.Time {
	var out []time.Time
	for _, t := range trips {
		if now.Sub(t) < window {
			out = append(out, t)
		}
	}
	return out
}
