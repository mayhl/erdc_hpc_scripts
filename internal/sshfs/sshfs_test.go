package sshfs

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mayhl/mayhl_utils/internal/config"
)

// setRoot points the sshfs root at a temp dir via a throwaway config.toml — the
// engine's only root source now that the MU_SSHFS_ROOT env fallback is retired.
// ResetForTest drops the memoized config so the new file is read.
func setRoot(t *testing.T, root string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("[sshfs]\nroot = %q\n", root)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MU_CONFIG_FILE", cfgPath)
	t.Setenv("MU_ROOT", "")
	config.ResetForTest()
}

// TestRegistryRoundTrip writes a registry (incl. a read-only entry and a path
// with spaces) and reads it back, verifying tab-separated parsing and the ro flag.
func TestRegistryRoundTrip(t *testing.T) {
	root := t.TempDir()
	setRoot(t, root)

	in := map[string]Mount{
		"proj":    {Node: "alpha", Path: "/p/work/proj", RO: false},
		"data_ro": {Node: "beta", Path: "/archive/data set", RO: true},
		// groups on an rw mount (an explicit "rw" marker must hold field 4's position)
		// and on a ro mount, incl. multi-membership.
		"grp_rw": {Node: "gamma", Path: "/p/work/g", RO: false, Groups: []string{"a", "b"}},
		"grp_ro": {Node: "delta", Path: "/p/work/d", RO: true, Groups: []string{"x"}},
	}
	if err := WriteRegistry(in); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := ReadRegistry()
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch:\n got %#v\nwant %#v", got, in)
	}

	// Registry file lands at $root/registry, mounts nest under $root/mounts/<name>.
	if RegistryPath() != filepath.Join(root, "registry") {
		t.Errorf("RegistryPath = %s", RegistryPath())
	}
	if MountDir("proj") != filepath.Join(root, "mounts", "proj") {
		t.Errorf("MountDir = %s", MountDir("proj"))
	}
}

// TestReadRegistrySkipsCommentsAndBlanks ensures header comments and blank lines
// are ignored, and a bare (non-ro) 3-field line parses as read-write.
func TestReadRegistrySkipsCommentsAndBlanks(t *testing.T) {
	root := t.TempDir()
	setRoot(t, root)
	body := "# a comment\n\nfoo\talpha\t/home/foo\nbar\tbeta\t/scratch/bar\tro\n"
	if err := os.WriteFile(filepath.Join(root, "registry"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := ReadRegistry()
	if len(reg) != 2 {
		t.Fatalf("want 2 entries, got %d: %v", len(reg), reg)
	}
	if reg["foo"].RO || !reg["bar"].RO {
		t.Errorf("ro flags wrong: %v", reg)
	}
}

// TestReadRegistryGroups checks the 5-field group encoding parses, an explicit "rw"
// marker keeps a group-bearing rw mount read-write, and an old 4-field ro line (no
// group field) still reads with nil groups (backward compat).
func TestReadRegistryGroups(t *testing.T) {
	root := t.TempDir()
	setRoot(t, root)
	body := "g_rw\talpha\t/p/a\trw\tproj,navy\n" +
		"g_ro\tbeta\t/p/b\tro\tproj\n" +
		"old_ro\tgamma\t/p/c\tro\n"
	if err := os.WriteFile(filepath.Join(root, "registry"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := ReadRegistry()
	if got := reg["g_rw"]; got.RO || !reflect.DeepEqual(got.Groups, []string{"proj", "navy"}) {
		t.Errorf("g_rw = %#v", got)
	}
	if got := reg["g_ro"]; !got.RO || !reflect.DeepEqual(got.Groups, []string{"proj"}) {
		t.Errorf("g_ro = %#v", got)
	}
	if got := reg["old_ro"]; !got.RO || got.Groups != nil {
		t.Errorf("old_ro backward-compat = %#v", got)
	}
}

func TestMountArgs(t *testing.T) {
	const shim = "/state/mayhl_utils/bin/mu-sshfs-ssh"
	got := MountArgs(shim, "me@hpc2.example", "/data", "/local/mnt", true, false)
	want := []string{
		"-o", "ssh_command=" + shim,
		"-o", "reconnect", "-o", "defer_permissions", "-o", "ro",
		"me@hpc2.example:/data", "/local/mnt",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MountArgs =\n %q\nwant %q", got, want)
	}
	// verbose appends ssh -v as a trailing token; rw omits the ro option.
	v := MountArgs(shim, "me@hpc2.example", "/data", "/local/mnt", false, true)
	if v[1] != "ssh_command="+shim+" -v" {
		t.Errorf("verbose ssh_command = %q", v[1])
	}
	for _, a := range v {
		if a == "ro" {
			t.Errorf("rw mount should not carry -o ro: %q", v)
		}
	}
}

func TestEnsureSSHShim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p, err := EnsureSSHShim()
	if err != nil {
		t.Fatalf("EnsureSSHShim: %v", err)
	}
	if p != SSHShimPath() {
		t.Errorf("path = %q, want %q", p, SSHShimPath())
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat shim: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("shim not executable: %v", fi.Mode())
	}
	body, _ := os.ReadFile(p)
	for _, want := range []string{"klist -s", "ConnectTimeout=10", "${MU_SSH:-ssh}"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("shim missing %q:\n%s", want, body)
		}
	}
}
