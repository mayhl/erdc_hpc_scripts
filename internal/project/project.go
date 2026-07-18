// Package project implements the project-layer verbs over the mirror-namespace
// contract: locating the project root (the git repo), naming paths in the shared
// $HOME-relative namespace, and the submit-origin stamp that iterate-mode submits
// ship to the cluster ($WORK staging is not a checkout — the stamp is the only
// carrier of the inputs' commit).
package project

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// StampFile is the submit-origin record `mu project submit` drops in $WORK
// staging; `mu job prep` folds it into run.toml when staging has no git.
const StampFile = ".mu-origin.toml"

// AffinityFile marks a subtree's node lock — the node/system a case (or a whole
// study dir) belongs to. Nearest-ancestor wins: a per-case marker splits a sweep
// across nodes, a study-dir marker locks the sweep whole. This is the DECLARED
// intent (enforced on submit); run.toml's `cluster` is the OBSERVED record.
const AffinityFile = ".mu-node"

// FleetFile is the optional project-root override naming the nodes a project's
// SHARED data fans out to. It covers what the AffinityFile marker-union can't: the
// BOOTSTRAP span (declare the fleet before any case is locked, to pre-stage shared
// data ahead of first run) and a data-only node (staged but not yet run on). One
// node token per line; blank lines and # comments skipped.
const FleetFile = ".mu-fleet"

// ArchiveFile names the project's designated archive-master node — the single cluster
// whose per-cluster HSM ($ARCHIVE) is the project's archive source-of-truth. A cross-
// cluster project has a separate archive on each DSRC, so archiving from every cluster
// would scatter and duplicate the project's archived data; the marker picks one. Project-
// root only (not walked up like AffinityFile), one node token, blank/# lines skipped —
// DECLARED intent, warned on when `mu archive` runs from a different cluster.
const ArchiveFile = ".mu-archive"

// FindRoot walks up from path to the enclosing git repo root — the project root
// per the structure contract (no project.toml marker until the sim manager needs
// one).
func FindRoot(path string) (string, error) {
	dir, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%s is not inside a git project", path)
		}
		dir = parent
	}
}

// Affinity resolves dir's declared node lock by walking up to the project root
// (inclusive) and returning the nearest (deepest) AffinityFile's node — the same
// longest-prefix idiom as FindRoot / mirror_set. ok=false when no marker lies
// between dir and the root: an unlocked subtree submits anywhere. The marker value
// is a node token, compared directly to --node; an empty marker is an error, not
// silently unlocked.
func Affinity(dir string) (node, marker string, ok bool, err error) {
	root, err := FindRoot(dir)
	if err != nil {
		return "", "", false, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", false, err
	}
	for {
		path := filepath.Join(abs, AffinityFile)
		c, found, err := readAffinity(path)
		if err != nil {
			return "", "", false, err
		}
		if found {
			return c, path, true, nil
		}
		if abs == root {
			return "", "", false, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", "", false, nil
		}
		abs = parent
	}
}

// readAffinity reads the first non-blank, non-comment line of an AffinityFile;
// found=false when the file is absent (an unmarked dir), an error when it exists
// but declares no node (a malformed lock we refuse to ignore).
func readAffinity(path string) (node string, found bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line, true, nil
	}
	return "", false, fmt.Errorf("%s declares no node", path)
}

// Fleet resolves the set of nodes a project's SHARED data spans, in override order:
// the FleetFile at the root if present (an explicit list — the bootstrap/data-only
// escape hatch), else the union of every AffinityFile marker under the tree (the
// nodes cases are actually locked to). The result is deduplicated and sorted; source
// names which path answered. An empty result means the project declares no fleet —
// there is nothing to fan out to. Note this is the per-project fleet, distinct from
// config.Fleet (the cross-project mstat query scope).
func Fleet(root string) (nodes []string, source string, err error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	if list, ok, ferr := readFleetFile(filepath.Join(abs, FleetFile)); ferr != nil {
		return nil, "", ferr
	} else if ok {
		return list, FleetFile, nil
	}
	// Marker union: walk the tree collecting each AffinityFile's node token. .git is
	// skipped (no markers live there); a malformed marker aborts the walk, same as
	// Affinity refusing to silently ignore one.
	set := map[string]bool{}
	werr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != AffinityFile {
			return nil
		}
		node, found, rerr := readAffinity(p)
		if rerr != nil {
			return rerr
		}
		if found {
			set[node] = true
		}
		return nil
	})
	if werr != nil {
		return nil, "", werr
	}
	for n := range set {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	return nodes, "markers", nil
}

// readFleetFile reads a FleetFile's node tokens — every non-blank, non-comment line.
// found=false when the file is absent; an error when it exists but names no node (a
// malformed override we refuse to treat as an empty fleet).
func readFleetFile(path string) (nodes []string, found bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		nodes = append(nodes, line)
	}
	if len(nodes) == 0 {
		return nil, false, fmt.Errorf("%s names no nodes", path)
	}
	sort.Strings(nodes)
	return nodes, true, nil
}

// ArchiveMaster reads the project's designated archive-master node from the root
// ArchiveFile. ok=false when no marker is set (archiving is then unguarded — a single-
// cluster project needs no master); an error when the file exists but names no node (a
// malformed marker we refuse to treat as unset, same as the other markers).
func ArchiveMaster(root string) (node string, ok bool, err error) {
	path := filepath.Join(root, ArchiveFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line, true, nil
	}
	return "", false, fmt.Errorf("%s declares no node", path)
}

// SetArchiveMaster writes node as the project's archive master, overwriting any existing
// marker. The marker is a tracked declaration of intent, so it is written deliberately (by
// `mu project archive-master`), never as a sync side effect.
func SetArchiveMaster(root, node string) error {
	body := "# mu archive-master — the cluster this project archives from (single HSM source-of-truth).\n" + node + "\n"
	return os.WriteFile(filepath.Join(root, ArchiveFile), []byte(body), 0o644)
}

// ClearArchiveMaster removes the archive-master marker; a no-op (nil) when none is set.
func ClearArchiveMaster(root string) error {
	err := os.Remove(filepath.Join(root, ArchiveFile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// HomeRel names path in the shared relative namespace: its path relative to
// $HOME — the rel that addresses the same thing on every system's tiers.
func HomeRel(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(home, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside $HOME — the mirror namespace is $HOME-relative", path)
	}
	return rel, nil
}

// Stamp is the submit-origin provenance: the fields prep can't reconstruct on
// the cluster. Dirty is case-scoped (uncommitted edits under the case dir), the
// same rule prep applies when it has a checkout.
type Stamp struct {
	Case   string `toml:"case,omitempty"`
	Commit string `toml:"commit,omitempty"`
	Dirty  bool   `toml:"dirty"`
}

// NewStamp reads the case's provenance from the submitting machine's checkout.
// No commits yet (or no git) → commit absent and dirty true: without a sha
// nothing is reproducible, so the record must not look clean.
func NewStamp(caseDir string) Stamp {
	s := Stamp{Dirty: true}
	if rel, ok := gitOut(caseDir, "rev-parse", "--show-prefix"); ok {
		s.Case = strings.TrimSuffix(rel, "/")
	}
	if commit, ok := gitOut(caseDir, "rev-parse", "HEAD"); ok {
		s.Commit = commit
		status, _ := gitOut(caseDir, "status", "--porcelain", "--", ".")
		s.Dirty = status != ""
	}
	return s
}

// TOML renders the stamp as the StampFile payload.
func (s Stamp) TOML() string {
	b, err := toml.Marshal(s)
	if err != nil {
		return ""
	}
	return "# submit-origin provenance — written by `mu project submit` on the submitting machine\n" + string(b)
}

// ReadStamp loads dir's StampFile; ok=false when absent or unparseable —
// provenance degrades to fewer run.toml fields, never to an error.
func ReadStamp(dir string) (Stamp, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, StampFile))
	if err != nil {
		return Stamp{}, false
	}
	var s Stamp
	if err := toml.Unmarshal(raw, &s); err != nil {
		return Stamp{}, false
	}
	return s, true
}

// gitOut runs git in dir and returns trimmed stdout; ok=false outside a repo or
// without git.
func gitOut(dir string, args ...string) (string, bool) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
