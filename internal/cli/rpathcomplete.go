package cli

// Cached remote-path completion, shared by `mu sshfs add/set` (directories) and
// `mu cp push/pull` (directories + files). Cobra runs completion as a fresh `mu __complete`
// process per TAB, so there is no in-memory state to carry a listing between tabs — the
// cache MUST be on disk. And completion must never block or prompt: a slow ssh hangs the
// shell, and a first-contact ssh can pop a CAC/PIN prompt the TAB can't answer. So a miss
// fires ONE probe (hpc.RemoteProbe) that runs ONLY over an already-open ControlMaster socket
// (it stats the socket and bails otherwise — never a fresh connect); a cold host yields no
// completion instead of a hang. Any live mount / cp / tunnel keeps a master warm, so in
// practice completion is live while you are working a node.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// rpathCacheTTL is short: a remote dir changes as you create run/output dirs during a
// session, and a completion is only advisory — a stale entry just fails on use.
const rpathCacheTTL = 10 * time.Minute

// splitRemotePath splits what's typed so far into the directory to list and the basename
// prefix to filter by. A remote path starts with / ~ or $ (a bare word has no anchorable
// root — don't guess), and we only act once a slash marks the directory boundary.
func splitRemotePath(toComplete string) (dir, prefix string, ok bool) {
	if toComplete == "" {
		return "", "", false
	}
	switch toComplete[0] {
	case '/', '~', '$':
	default:
		return "", "", false
	}
	i := strings.LastIndexByte(toComplete, '/')
	if i < 0 {
		return "", "", false // "~" or "$WORKDIR" with no slash yet — wait for it
	}
	return toComplete[:i+1], toComplete[i+1:], true
}

// rpathSafe rejects a dir carrying shell metacharacters: the listing expands ~ and $VARS on
// the remote login shell (unquoted, so $WORKDIR resolves), so anything past path/var chars
// could inject — such a dir simply gets no completion.
func rpathSafe(dir string) bool {
	for _, r := range dir {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("/._-~$", r):
		default:
			return false
		}
	}
	return true
}

// filterRemoteEntries keeps `ls -p` lines that match prefix, dropping ./ and ../; dirsOnly
// discards plain files (a trailing / marks a directory). Each survivor is rejoined with the
// dir so the shell replaces the whole path token.
func filterRemoteEntries(lines []string, dir, prefix string, dirsOnly bool) []string {
	var out []string
	for _, e := range lines {
		if e == "" || e == "./" || e == "../" {
			continue
		}
		if dirsOnly && !strings.HasSuffix(e, "/") {
			continue
		}
		if !strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, dir+e)
	}
	return out
}

// rpathCachePath is where a (node, dir) listing lives — disposable ~/.cache, keyed by a
// digest of node+dir so a deep path stays a short filename.
func rpathCachePath(node, dir string) string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	h := sha256.Sum256([]byte(node + "\x00" + dir))
	return filepath.Join(base, "mayhl_utils", "rpath", node, hex.EncodeToString(h[:8]))
}

// readRpathCache returns the cached listing if it exists and is within the TTL.
func readRpathCache(path string) ([]string, bool) {
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) > rpathCacheTTL {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return splitLines(string(b)), true
}

func writeRpathCache(path string, lines []string) {
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// completeRemotePath is the shared cobra completer for a remote-path arg on a node: cache
// read (instant), else one non-blocking master-only probe (never prompts, never hangs),
// then filter. dirsOnly restricts to directories (sshfs mount targets); cp wants files too.
func completeRemotePath(node, toComplete string, dirsOnly bool) ([]string, cobra.ShellCompDirective) {
	none := cobra.ShellCompDirectiveNoFileComp
	dir, prefix, ok := splitRemotePath(toComplete)
	if !ok || !rpathSafe(dir) {
		return nil, none
	}
	target, err := hpc.Resolve(node)
	if err != nil {
		return nil, none
	}
	cache := rpathCachePath(node, dir)
	lines, hit := readRpathCache(cache)
	if !hit {
		// A dir with ~ or $ needs a login shell to expand (bash -lc so ~ and $WORKDIR
		// resolve; dir is rpathSafe so the unquoted expansion can't inject). A plain path
		// runs a bare ls instead — same result, minus the ~1.4s login-profile sourcing that
		// alone pushed the round-trip past the probe deadline.
		remoteCmd := "ls -1Ap -- " + dir // dir is rpathSafe (no spaces/globs) — no quoting needed
		if strings.ContainsAny(dir, "~$") {
			remoteCmd = "bash -lc " + shell.Quote("ls -1Ap -- "+dir)
		}
		out, probeOK := hpc.RemoteProbe(target, remoteCmd)
		if !probeOK {
			return nil, none // cold host / no master — no completion, no hang
		}
		lines = splitLines(out)
		writeRpathCache(cache, lines)
	}
	comps := filterRemoteEntries(lines, dir, prefix, dirsOnly)
	// A directory completion ends in / — no trailing space, so a second TAB descends.
	return comps, none | cobra.ShellCompDirectiveNoSpace
}
