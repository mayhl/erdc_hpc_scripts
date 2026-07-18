package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/project"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// syncManifestName is the manifest's basename, dropped at each tier's remote dest. It
// travels with the staged data, so the cluster tree self-describes what shared data is
// there and from which source version — the answer to "what data version is staged where"
// without re-reading the bytes.
const syncManifestName = ".mu-sync.toml"

const manifestHeader = "# mu project sync — staged shared data + source provenance.\n" +
	"# Accumulating and add-only: each file is recorded once at push time; entries never change.\n\n"

// manifestFile is one staged file's provenance line. Size + mtime identify the version
// cheaply; sha256 is present only when the push ran with --verify (its digests are reused
// here, never re-hashed). Commit/dirty pin the project source that produced the file.
type manifestFile struct {
	Path   string `toml:"path"`
	Size   int64  `toml:"size"`
	Mtime  string `toml:"mtime"`
	SHA256 string `toml:"sha256,omitempty"`
	Commit string `toml:"commit,omitempty"`
	Dirty  bool   `toml:"dirty"`
	Synced string `toml:"synced"`
	Host   string `toml:"host,omitempty"`
}

// syncManifest is a tier's accumulating record — one [[file]] per staged file.
type syncManifest struct {
	File []manifestFile `toml:"file"`
}

// mergeManifest folds new entries into an existing manifest, keyed by path: a fresh file
// appends, a re-pushed one (the --force overwrite case, the sole exception to add-only)
// replaces its entry. The result is sorted by path so the manifest stays diff-stable across
// pushes. Pure — the read/write round trip lives in writeManifest so this stays testable.
func mergeManifest(existing syncManifest, entries []manifestFile) syncManifest {
	byPath := make(map[string]int, len(existing.File))
	for i, f := range existing.File {
		byPath[f.Path] = i
	}
	for _, e := range entries {
		if i, ok := byPath[e.Path]; ok {
			existing.File[i] = e
			continue
		}
		byPath[e.Path] = len(existing.File)
		existing.File = append(existing.File, e)
	}
	sort.SliceStable(existing.File, func(i, j int) bool { return existing.File[i].Path < existing.File[j].Path })
	return existing
}

// writeManifest records what a push just staged into each tier's remote manifest. It runs
// after a successful transfer, so a failure here is advisory — the data is already on the
// cluster; a warn is enough, never a command failure. digests holds the sha256 of each
// pushed file (keyed by absolute local path) when --verify ran, else nil — the manifest
// then records size + mtime only, the cheap default.
func writeManifest(target, root, node string, results []syncResult, o projSyncOpts, digests map[string]string) {
	stamp := project.NewStamp(root)
	now := time.Now().UTC().Format(time.RFC3339)
	host, _ := os.Hostname()

	for _, res := range results {
		files := res.newPaths
		if o.force {
			files = append(append([]string{}, res.newPaths...), res.updates...)
		}
		if len(files) == 0 {
			continue
		}

		entries := make([]manifestFile, 0, len(files))
		for _, rel := range files {
			fi, err := os.Stat(filepath.Join(res.localAbs, rel))
			if err != nil {
				continue
			}
			entries = append(entries, manifestFile{
				Path:   rel,
				Size:   fi.Size(),
				Mtime:  fi.ModTime().UTC().Format(time.RFC3339),
				SHA256: digests[filepath.Join(res.localAbs, rel)],
				Commit: stamp.Commit,
				Dirty:  stamp.Dirty,
				Synced: now,
				Host:   host,
			})
		}
		if len(entries) == 0 {
			continue
		}

		m := mergeManifest(readManifest(target, res.dest), entries)
		if err := saveManifest(target, res.dest, m); err != nil {
			render.Warn(fmt.Sprintf("manifest: %s: %s (data pushed, provenance not recorded)", node, err))
		}
	}
}

// pruneManifest drops the given paths from a manifest, returning the trimmed record. The
// mirror of mergeManifest for the delete side: prune removes files, so their provenance
// entries go too, keeping the manifest honest about what is staged. Pure — the read/save
// round trip lives in updatePruneManifest so this stays testable.
func pruneManifest(existing syncManifest, deleted []string) syncManifest {
	drop := make(map[string]bool, len(deleted))
	for _, p := range deleted {
		drop[p] = true
	}
	kept := make([]manifestFile, 0, len(existing.File))
	for _, f := range existing.File {
		if !drop[f.Path] {
			kept = append(kept, f)
		}
	}
	return syncManifest{File: kept}
}

// updatePruneManifest trims the pruned files' entries from a tier's remote manifest after a
// successful delete. Advisory — the files are already gone, so a failure warns but never
// fails the prune. A manifest emptied of every entry is still written (an empty file list),
// not removed, so the tier keeps its provenance file.
func updatePruneManifest(target, node, dest string, deleted []string) {
	m := readManifest(target, dest)
	if len(m.File) == 0 {
		return // nothing recorded (pushed before manifests, or never) — nothing to trim
	}
	m = pruneManifest(m, deleted)
	if err := saveManifest(target, dest, m); err != nil {
		render.Warn(fmt.Sprintf("manifest: %s: %s (files pruned, manifest not updated)", node, err))
	}
}

// readManifest fetches the tier's existing manifest, returning an empty one when it is
// absent or unparseable — a first push has none, and a garbled file must not strand the
// merge (the fresh entries still land, replacing it).
func readManifest(target, dest string) syncManifest {
	path := dest + "/" + syncManifestName
	out, err := hpc.RemoteExec(target, fmt.Sprintf("cat %s 2>/dev/null || true", shell.Quote(path)))
	if err != nil {
		return syncManifest{}
	}
	var m syncManifest
	_ = toml.Unmarshal([]byte(out), &m)
	return m
}

// saveManifest writes the manifest to the tier's dest. The TOML is base64'd and decoded on
// the remote, sidestepping every quoting hazard a heredoc or inline write would carry.
// NOTE: the encoded body rides the command line, so a tier with a very large manifest
// (many thousands of files) could approach ARG_MAX — switch to a staged transfer if that
// ceiling is ever hit; the common few-files push stays well under it.
func saveManifest(target, dest string, m syncManifest) error {
	body, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(manifestHeader + string(body)))
	path := dest + "/" + syncManifestName
	cmd := fmt.Sprintf("printf %%s %s | base64 -d > %s", shell.Quote(b64), shell.Quote(path))
	_, err = hpc.RemoteExec(target, cmd)
	return err
}
