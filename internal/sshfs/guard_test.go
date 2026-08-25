package sshfs

import (
	"testing"
	"time"
)

const psFixture = `  368 root               0.0 /usr/local/McAfee/AntiMalware/VShieldScanner
  369 root              31.2 /usr/local/McAfee/AntiMalware/VShieldScanner
  370 root              28.9 /usr/local/McAfee/AntiMalware/VShieldScanner
  381 root               1.3 /usr/local/McAfee/AntiMalware/VShieldScanManager.app/Contents/MacOS/VShieldScanManager async
 1875 alice              0.0 /usr/local/bin/go-nfsv4 /Users/alice/hpc_sshfs/mounts/home_b
 1879 alice              0.4 sshfs -o ssh_command=/x/mu-sshfs-ssh -o reconnect user@b:/p/home/alice /Users/alice/hpc_sshfs/mounts/home_b
 1889 alice              7.5 /usr/local/bin/go-nfsv4 /Users/alice/hpc_sshfs/mounts/home_c
 1910 alice              3.0 sshfs -o ssh_command=/x/mu-sshfs-ssh -o reconnect user@c:/p/home/alice /Users/alice/hpc_sshfs/mounts/home_c
 2000 alice              9.9 /usr/local/bin/go-nfsv4 /Users/alice/other/mounts/foreign
garbage line
`

func TestParsePSAndCPU(t *testing.T) {
	procs := ParsePS(psFixture)
	if len(procs) != 9 {
		t.Fatalf("parsed %d procs, want 9", len(procs))
	}
	if got := ScannerCPU(procs); got < 60.09 || got > 60.11 {
		t.Errorf("ScannerCPU = %v, want 60.1 (manager excluded)", got)
	}
	d := DaemonCPU(procs, "/Users/alice/hpc_sshfs/mounts")
	if got := d["/Users/alice/hpc_sshfs/mounts/home_c"]; got != 10.5 {
		t.Errorf("home_c daemon cpu = %v, want 10.5", got)
	}
	if got := d["/Users/alice/hpc_sshfs/mounts/home_b"]; got != 0.4 {
		t.Errorf("home_b daemon cpu = %v, want 0.4", got)
	}
	if _, ok := d["/Users/alice/other/mounts/foreign"]; ok {
		t.Error("daemon outside mountsRoot must not be keyed")
	}
}

func TestHeldDirs(t *testing.T) {
	names := parseLsofNames("p123\nfcwd\nn/m/home_c\np124\nf3\nn/m/home_b/sub/file.nc\nn/m/home_bx/x\n")
	held := heldDirs([]string{"/m/home_c", "/m/home_b", "/m/home_bx", "/m/home_n"}, names)
	want := map[string]bool{"/m/home_c": true, "/m/home_b": true, "/m/home_bx": true}
	for d, w := range want {
		if held[d] != w {
			t.Errorf("held[%s] = %v, want %v", d, held[d], w)
		}
	}
	if held["/m/home_n"] {
		t.Error("home_n must not read as held")
	}
	if got := heldDirs([]string{"/m/home_b"}, parseLsofNames("n/m/home_bx/x")); got["/m/home_b"] {
		t.Error("prefix match must be on a path boundary")
	}
}

func tick(st *GuardState, scanner float64, now time.Time, ticket bool, obs ...Observation) []Action {
	return Decide(st, scanner, obs, now, DefaultGuardTuning(), ticket)
}

func ops(actions []Action) string {
	s := ""
	for _, a := range actions {
		s += a.Op + ":" + a.Name + " "
	}
	return s
}

func TestDecideAVParkNeedsSustainAndAbsence(t *testing.T) {
	st := LoadGuardStateFor(t)
	t0 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	hot := Observation{Name: "home_c", Mounted: true, DaemonCPU: 12}

	if a := tick(st, 60, t0, true, hot); len(a) != 0 {
		t.Fatalf("first hot tick must not park: %s", ops(a))
	}
	held := hot
	held.Held = true
	if a := tick(st, 60, t0.Add(30*time.Second), true, held); len(a) != 0 {
		t.Fatalf("owner in the mount must block a park: %s", ops(a))
	}
	a := tick(st, 60, t0.Add(time.Minute), true, hot)
	if ops(a) != "park:home_c " {
		t.Fatalf("sustained scan + busy daemon + owner absent must park: %s", ops(a))
	}
	if st.Parked("home_c") != "av" {
		t.Errorf("Parked reason = %q, want av", st.Parked("home_c"))
	}
	// A cold daemon during a hot scan means the AV is elsewhere.
	st2 := LoadGuardStateFor(t)
	cold := Observation{Name: "home_b", Mounted: true, DaemonCPU: 0.3}
	tick(st2, 60, t0, true, cold)
	if a := tick(st2, 60, t0.Add(time.Minute), true, cold); len(a) != 0 {
		t.Errorf("idle daemon must not park: %s", ops(a))
	}
}

func TestDecideRemountAfterCooldownNeedsTicket(t *testing.T) {
	st := LoadGuardStateFor(t)
	t0 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	hot := Observation{Name: "home_c", Mounted: true, DaemonCPU: 12}
	tick(st, 60, t0, true, hot)
	tick(st, 60, t0.Add(30*time.Second), true, hot) // parks
	down := Observation{Name: "home_c"}
	if a := tick(st, 0, t0.Add(60*time.Second), true, down); len(a) != 0 {
		t.Fatalf("cooldown not over: %s", ops(a))
	}
	if a := tick(st, 0, t0.Add(2*time.Minute), false, down); len(a) != 0 {
		t.Fatalf("no ticket → no remount attempt: %s", ops(a))
	}
	if a := tick(st, 0, t0.Add(2*time.Minute+30*time.Second), true, down); ops(a) != "remount:home_c " {
		t.Fatalf("cooldown over + ticket must remount: %s", ops(a))
	}
	// Back up clears the debt but keeps the trip.
	tick(st, 0, t0.Add(3*time.Minute), true, Observation{Name: "home_c", Mounted: true})
	if st.Parked("home_c") != "" || len(st.Mounts["home_c"].Trips) != 1 {
		t.Errorf("remounted: parked=%q trips=%d, want ''/1", st.Parked("home_c"), len(st.Mounts["home_c"].Trips))
	}
}

func TestDecideBenchAfterRepeatedTrips(t *testing.T) {
	st := LoadGuardStateFor(t)
	t0 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	hot := Observation{Name: "home_c", Mounted: true, DaemonCPU: 12}
	down := Observation{Name: "home_c"}
	now := t0
	for i := 0; i < 3; i++ { // park, wait out the cooldown, remount, AV returns
		tick(st, 60, now, true, hot)
		now = now.Add(30 * time.Second)
		if a := tick(st, 60, now, true, hot); ops(a) != "park:home_c " {
			t.Fatalf("trip %d: %s", i+1, ops(a))
		}
		now = now.Add(90 * time.Second)
		a := tick(st, 0, now, true, down)
		if i < 2 && ops(a) != "remount:home_c " {
			t.Fatalf("trip %d: expected remount, got %s", i+1, ops(a))
		}
		if i == 2 {
			if ops(a) != "bench:home_c " {
				t.Fatalf("third trip must bench, got %s", ops(a))
			}
			now = now.Add(30 * time.Second)
			if a := tick(st, 0, now, true, down); len(a) != 0 {
				t.Fatalf("bench warns once: %s", ops(a))
			}
			break
		}
		now = now.Add(30 * time.Second)
		tick(st, 0, now, true, Observation{Name: "home_c", Mounted: true})
		now = now.Add(30 * time.Second)
	}
}

func TestDecideIdleUnmount(t *testing.T) {
	st := LoadGuardStateFor(t)
	t0 := time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)
	up := Observation{Name: "home_c", Mounted: true}
	tick(st, 0, t0, true, up)
	if a := tick(st, 0, t0.Add(29*time.Minute), true, up); len(a) != 0 {
		t.Fatalf("not idle yet: %s", ops(a))
	}
	// Owner activity resets the clock; own reads (daemon busy, scanner cold) count too.
	tick(st, 0, t0.Add(29*time.Minute+30*time.Second), true, Observation{Name: "home_c", Mounted: true, DaemonCPU: 8})
	if a := tick(st, 0, t0.Add(40*time.Minute), true, up); len(a) != 0 {
		t.Fatalf("activity must reset idle: %s", ops(a))
	}
	heldUp := up
	heldUp.Held = true
	if a := tick(st, 0, t0.Add(70*time.Minute), true, heldUp); len(a) != 0 {
		t.Fatalf("held mount never idles out: %s", ops(a))
	}
	if a := tick(st, 0, t0.Add(101*time.Minute), true, up); ops(a) != "idle:home_c " {
		t.Fatalf("idle past the window must unmount: %s", ops(a))
	}
	down := Observation{Name: "home_c"}
	if a := tick(st, 0, t0.Add(200*time.Minute), true, down); len(a) != 0 || st.Parked("home_c") != "idle" {
		t.Fatalf("idle-parked stays down (no remount): %s parked=%q", ops(a), st.Parked("home_c"))
	}
	// Unmounted by hand (never parked) leaves nothing owed; removed from the registry is forgotten.
	st2 := LoadGuardStateFor(t)
	tick(st2, 0, t0, true, Observation{Name: "x", Mounted: true})
	tick(st2, 0, t0.Add(time.Minute), true, Observation{Name: "x"})
	if st2.Parked("x") != "" {
		t.Error("hand-unmounted must not read as parked")
	}
	tick(st2, 0, t0.Add(2*time.Minute), true)
	if _, ok := st2.Mounts["x"]; ok {
		t.Error("unregistered mount must be forgotten")
	}
}

// LoadGuardStateFor gives a test a fresh in-memory state (no file).
func LoadGuardStateFor(t *testing.T) *GuardState {
	t.Helper()
	return &GuardState{Mounts: map[string]*GuardMount{}}
}
