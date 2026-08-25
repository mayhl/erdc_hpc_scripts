// Package sshfs is the engine for the local-only `mu sshfs` mount plane: a file
// registry of named HPC mounts plus timeout-bounded mount-state inspection, so a
// hung/dead mount reports a status instead of freezing the terminal. Ports the
// retired Python sshfs.py; command orchestration (resolve, Kerberos, spinner)
// lives in internal/cli, rendering in internal/render.
package sshfs

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mayhl/mayhl_utils/internal/config"
)

// Mount is one registry entry: where it points, whether it's read-only, and the
// groups it belongs to (a mount can be in several; empty for most).
type Mount struct {
	Node   string
	Path   string
	RO     bool
	Groups []string
}

const statTimeout = 4 * time.Second // slower listing → treat the mount as hung

// Root is the mount parent dir (config [sshfs] root, default ~/hpc_sshfs),
// with ~ expanded.
func Root() string {
	return expandHome(config.SSHFSRoot())
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// RegistryPath is the tab-separated registry file; MountsRoot is the parent of
// all mount dirs (nested under mounts/ so a mount named "mounts" can't collide
// with the registry file); MountDir is one mount's local dir.
func RegistryPath() string        { return filepath.Join(Root(), "registry") }
func MountsRoot() string          { return filepath.Join(Root(), "mounts") }
func MountDir(name string) string { return filepath.Join(MountsRoot(), name) }

// ReadRegistry parses the registry file into {name: Mount}. Lines are
// `name<TAB>node<TAB>path[<TAB>ro|rw[<TAB>groups]]`; blanks and #-comments are
// skipped. A missing file is an empty registry, not an error. Field 4 is the
// ro/rw marker (only "ro" means read-only); field 5, when present, is a
// comma-separated group list — so a groups-bearing rw mount carries an explicit
// "rw" in field 4 to hold the position. Old 3/4-field lines still parse.
func ReadRegistry() map[string]Mount {
	out := map[string]Mount{}
	f, err := os.Open(RegistryPath())
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s := sc.Text()
		if t := strings.TrimSpace(s); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		parts := strings.Split(s, "\t")
		if len(parts) >= 3 {
			ro := len(parts) >= 4 && strings.TrimSpace(parts[3]) == "ro"
			var groups []string
			if len(parts) >= 5 {
				for _, g := range strings.Split(parts[4], ",") {
					if g = strings.TrimSpace(g); g != "" {
						groups = append(groups, g)
					}
				}
			}
			out[strings.TrimSpace(parts[0])] = Mount{
				Node:   strings.TrimSpace(parts[1]),
				Path:   strings.TrimSpace(parts[2]),
				RO:     ro,
				Groups: groups,
			}
		}
	}
	return out
}

// WriteRegistry rewrites the registry file, entries sorted by name.
func WriteRegistry(entries map[string]Mount) error {
	path := RegistryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# managed by `mu sshfs add` / `mu sshfs rm` / `mu sshfs group` — do not hand-edit lightly\n")
	b.WriteString("# name\tnode\tremote-path\t[ro|rw]\t[group,group,…]\n")
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m := entries[n]
		b.WriteString(n + "\t" + m.Node + "\t" + m.Path)
		// Groups need an explicit ro/rw marker in field 4 to hold field 5's position;
		// without groups, keep the terse form (append "\tro" only when read-only).
		switch {
		case len(m.Groups) > 0:
			access := "rw"
			if m.RO {
				access = "ro"
			}
			b.WriteString("\t" + access + "\t" + strings.Join(m.Groups, ","))
		case m.RO:
			b.WriteString("\tro")
		}
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// CompleteMount returns registered names starting with prefix (shell completion).
func CompleteMount(prefix string) []string {
	var out []string
	for n := range ReadRegistry() {
		if strings.HasPrefix(n, prefix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// --- mount state (all timeout-bounded — never block on a hung mount) ---------

// runOut runs a command with a timeout, returning its stdout and whether it
// succeeded (exit 0, no timeout).
func runOut(timeout time.Duration, name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &buf
	err := cmd.Run()
	return buf.String(), err == nil && ctx.Err() == nil
}

// IsMounted reports whether mdir is an active mountpoint, by parsing `mount`
// output — it never touches the filesystem, so a hung mount can't block it.
func IsMounted(mdir string) bool {
	out, ok := runOut(5*time.Second, "mount")
	if !ok {
		return false
	}
	needle := " on " + mdir + " ("
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, needle) {
			return true
		}
	}
	return false
}

func responds(mdir string) bool {
	_, ok := runOut(statTimeout, "ls", mdir)
	return ok
}

// Status is "mounted" | "hung" | "unmounted", safe even against a hung mount.
func Status(name string) string {
	mdir := MountDir(name)
	if !IsMounted(mdir) {
		return "unmounted"
	}
	if responds(mdir) {
		return "mounted"
	}
	return "hung"
}

// Umount unmounts mdir (plain umount, then diskutil force). Returns false if it
// couldn't. A not-currently-mounted dir is a success.
func Umount(mdir string) bool {
	if !IsMounted(mdir) {
		return true
	}
	for _, c := range [][]string{{"umount", mdir}, {"diskutil", "unmount", "force", mdir}} {
		if _, ok := runOut(10*time.Second, c[0], c[1:]...); ok {
			return true
		}
	}
	return false
}

// sshShim is the ssh transport wrapper sshfs's ssh_command points at. It gates
// every ssh — including sshfs's own `-o reconnect` retries — on a live Kerberos
// ticket: with none (offline, or the CAC not yet unlocked) it exits fast instead
// of letting ssh reach GSSAPI, which on macOS auto-fires PKINIT and pops a CAC PIN
// prompt. During a VPN outage those prompts storm and fight the VPN client for the
// single CAC reader, blocking the very re-auth that would restore the network.
// `klist -s` is local and read-only — it never itself triggers PKINIT; gated only
// when klist exists so a non-Kerberos setup still mounts. Keepalive/ConnectTimeout
// options live here (not in MountArgs) so they apply to the real ssh it exec's;
// MU_SSH is honored at run time to match the rest of the transport plane.
const sshShim = `#!/bin/sh
# managed by mu — regenerated on every mount; do not hand-edit
if command -v klist >/dev/null 2>&1; then
	klist -s 2>/dev/null || exit 255
fi
exec ${MU_SSH:-ssh} -o ConnectTimeout=10 -o ServerAliveInterval=15 -o ServerAliveCountMax=3 "$@"
`

// SSHShimPath is the on-disk ssh transport shim — state, not cache: a mount's
// ssh_command points here, so losing it breaks reconnects. One shim, all mounts.
func SSHShimPath() string {
	return filepath.Join(stateDir(), "mayhl_utils", "bin", "mu-sshfs-ssh")
}

// stateDir is XDG_STATE_HOME (default ~/.local/state) — shim + guard state live here.
func stateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state")
}

// EnsureSSHShim writes the ssh transport shim (idempotent — overwrites so option
// changes propagate) and returns its path, ready to hand to MountArgs.
func EnsureSSHShim() (string, error) {
	p := SSHShimPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(sshShim), 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// MountArgs builds the sshfs argument vector (after the "sshfs" prog): the ssh
// transport is the ticket-gated shim (sshCmd, from EnsureSSHShim), plus reconnect,
// defer_permissions, optional read-only. Verbose appends ssh -v; sshfs word-splits
// ssh_command, so it rides through as a trailing token.
func MountArgs(sshCmd, target, rpath, mdir string, ro, verbose bool) []string {
	if verbose {
		sshCmd += " -v"
	}
	args := []string{"-o", "ssh_command=" + sshCmd, "-o", "reconnect", "-o", "defer_permissions"}
	if ro {
		args = append(args, "-o", "ro")
	}
	return append(args, target+":"+rpath, mdir)
}
