package cli

import (
	"reflect"
	"testing"
)

// staleNames keeps only the "hung" mounts, preserving input order, and skips the
// "mounted"/"unmounted" ones — so `mount --stale` revives what died without touching
// healthy or never-up mounts.
func TestStaleNames(t *testing.T) {
	status := map[string]string{
		"live": "mounted", "dead": "hung", "gone": "unmounted", "wedged": "hung",
	}
	got := staleNames([]string{"live", "dead", "gone", "wedged"}, func(n string) string {
		return status[n]
	})
	want := []string{"dead", "wedged"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("staleNames = %v, want %v", got, want)
	}
	if s := staleNames([]string{"live", "gone"}, func(n string) string { return status[n] }); len(s) != 0 {
		t.Errorf("no hung mounts should yield empty, got %v", s)
	}
}

// mountKind splits active fuse mounts three ways: a registered dir is "known" (by name),
// an unregistered dir UNDER mu's mounts root is "stale" (a mu mount whose registry entry
// is gone), and anything else is "foreign" (mounted outside mu, never to be touched).
func TestMountKind(t *testing.T) {
	root := "/home/u/hpc_sshfs/mounts"
	reg := map[string]string{root + "/scratch": "scratch"}
	cases := []struct {
		dir, wantKind, wantName string
	}{
		{root + "/scratch", "known", "scratch"}, // registered
		{root + "/oldrun", "stale", "oldrun"},   // under root, unregistered
		{"/Volumes/usb", "foreign", ""},         // mounted outside mu
		{root + "-sibling/x", "foreign", ""},    // prefix look-alike must NOT count as under root
	}
	for _, c := range cases {
		if k, n := mountKind(c.dir, reg, root); k != c.wantKind || n != c.wantName {
			t.Errorf("mountKind(%q) = (%q,%q), want (%q,%q)", c.dir, k, n, c.wantKind, c.wantName)
		}
	}
}

// The scanner must catch a fatal sshfs line even when it arrives split across writes,
// and hand back just that line (trimmed), so runMount can fail fast and show it.
func TestStderrScannerFatalLine(t *testing.T) {
	w := &stderrScanner{fatal: make(chan string, 1)}
	// sshfs prints the target then the error; simulate a mid-pattern chunk boundary.
	_, _ = w.Write([]byte("me@host.example.mil:/dummy/path: No such "))
	_, _ = w.Write([]byte("file or directory\n"))
	select {
	case got := <-w.fatal:
		want := "me@host.example.mil:/dummy/path: No such file or directory"
		if got != want {
			t.Fatalf("fatal line = %q, want %q", got, want)
		}
	default:
		t.Fatal("expected a fatal signal, got none")
	}
}

// Benign stderr (e.g. a login banner) must not trip the fatal path.
func TestStderrScannerIgnoresBenign(t *testing.T) {
	w := &stderrScanner{fatal: make(chan string, 1)}
	_, _ = w.Write([]byte("Warning: Permanently added 'host' to known hosts.\n"))
	if w.sent {
		t.Fatal("benign line tripped the fatal signal")
	}
}
