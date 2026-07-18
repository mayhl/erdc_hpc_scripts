// Package archive wraps the site PST/TUSC `archive` command (the HPSS
// front-end) with the mirror projection: the archive-side dir is computed from
// $PWD via mirror.Archive and injected as -C, ARCHIVE_PROBE=yes turns on the
// native size verify, and a flagless `put` packs case material into tar tiers
// before it puts (tape wants few large files). An explicit -C from the caller
// passes through untouched — the wrapper's whole job is inferring it.
package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hooks"
	"github.com/mayhl/mayhl_utils/internal/mirror"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/tar"
)

// provenanceFile is the run record `mu job prep` plants in every leaf; the pack hook
// splitter injects it into every chunk so any chunk extracts with its provenance.
const provenanceFile = "run.toml"

// Run dispatches one wrapped invocation: sub is the archive subcommand, args
// the rest verbatim. Returns a process exit code (failures already rendered).
func Run(sub string, args []string) int {
	bin, err := exec.LookPath("archive")
	if err != nil {
		render.Err("no `archive` command here — PST/TUSC lives on the HPC side")
		return 1
	}
	if hasArg(args, "-C") {
		return run(bin, "", "", sub, args)
	}
	wd, err := os.Getwd()
	if err != nil {
		render.Err(err.Error())
		return 1
	}
	if sub == "put" && len(args) > 0 && !hasFlags(args) {
		return put(bin, wd, args)
	}
	if sub == "get" && len(args) > 0 && !hasFlags(args) {
		return get(bin, wd, args)
	}
	proj, err := mirror.Archive(wd)
	if err != nil {
		render.Err(err.Error())
		return 2
	}
	return run(bin, "", proj, sub, args)
}

// run execs one real archive invocation: `archive <sub> [-C cdir] <args…>` from
// dir ("" = inherit). An injected -C also sets ARCHIVE_PROBE=yes (the native
// before/after size verify); the explicit--C passthrough stays untouched.
func run(bin, dir, cdir, sub string, args []string) int {
	argv := []string{sub}
	if cdir != "" {
		argv = append(argv, "-C", cdir)
	}
	argv = append(argv, args...)
	cmd := exec.Command(bin, argv...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if cdir != "" {
		cmd.Env = append(os.Environ(), "ARCHIVE_PROBE=yes")
	}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		render.Err("archive: " + err.Error())
		return 1
	}
	return 0
}

// pack is one dir → staged tar → put: the tar stages next to dir and lands at
// dst/name on the archive side. members (nil = the whole dir) is the leaf-relative
// subset a pack-hook chunk carries.
type pack struct {
	dir     string   // local dir to tar
	dst     string   // archive-side -C dir
	name    string   // tar basename, e.g. "250.tar"
	members []string // leaf-relative member subset; nil = whole dir
}

// put plans and runs the tar tiers for a flagless `archive put`: a case/run
// leaf → one tar at its projection; a parent whose EVERY case leaf is under
// tar_parent_threshold → ONE parent-level tar (the batch is the retrieval
// unit — all-or-nothing, so an oversize run can't hide inside it); otherwise
// tar per leaf. Plain files and non-case dirs pass through to a single native
// put under $PWD's projection.
func put(bin, wd string, args []string) int {
	var packs []pack
	var rest []string
	for _, a := range args {
		abs := a
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(wd, a)
		}
		abs = filepath.Clean(abs)
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			rest = append(rest, a)
			continue
		}
		ps, rc := planDir(abs)
		if rc != 0 {
			return rc
		}
		if ps == nil {
			rest = append(rest, a)
			continue
		}
		packs = append(packs, ps...)
	}
	for _, p := range packs {
		if rc := runPack(bin, p); rc != 0 {
			return rc
		}
	}
	if len(rest) > 0 {
		proj, err := mirror.Archive(wd)
		if err != nil {
			render.Err(err.Error())
			return 2
		}
		return run(bin, "", proj, "put", rest)
	}
	return 0
}

// get reassembles case/run leaves from the archive — the retrieval counterpart of
// put's packing. For each leaf arg it lists the leaf's chunk tars at its projection (a
// pack-hook split leaf is <base>_<suffix>.tar plus <base>_rest.tar; an unsplit one is
// just <base>.tar) and fetches them with the site tool's own -x: get extracts each tar
// and, without -S, drops it — and every chunk is rooted at the leaf basename, so they
// union back into the exact dir. Anything that isn't a case/run leaf falls to a single
// passthrough get, the same split as put's rest.
func get(bin, wd string, args []string) int {
	var rest []string
	for _, a := range args {
		if _, _, ok := mirror.ClassifyCase(filepath.Base(a)); !ok {
			rest = append(rest, a)
			continue
		}
		if rc := getLeaf(bin, wd, a); rc != 0 {
			return rc
		}
	}
	if len(rest) > 0 {
		proj, err := mirror.Archive(wd)
		if err != nil {
			render.Err(err.Error())
			return 2
		}
		return run(bin, "", proj, "get", rest)
	}
	return 0
}

// getLeaf fetches and reassembles one leaf. It projects the (possibly not-yet-present)
// local path to its archive dir — the same provenance guard as put, so a run pulls to
// the scratch tier and inputs to permanent — lists the chunk set there, then runs one
// `get -C <dst> -x <chunks…>` from the local parent so the extractions land as the leaf.
func getLeaf(bin, wd, leaf string) int {
	abs := leaf
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(wd, leaf)
	}
	abs = filepath.Clean(abs)
	proj, err := mirror.Archive(abs)
	if err != nil {
		render.Err(err.Error())
		return 2
	}
	base, dst := filepath.Base(proj), filepath.Dir(proj)
	out, err := lsOutput(bin, filepath.Join(dst, base+"*.tar"))
	if err != nil {
		render.Err(fmt.Sprintf("archive ls %s: %s", dst, err))
		return 1
	}
	names := chunkNames(base, out)
	if len(names) == 0 {
		render.Err(fmt.Sprintf("no archived chunks for %s at %s", filepath.Base(abs), dst))
		return 1
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		render.Err(err.Error())
		return 1
	}
	return run(bin, parent, dst, "get", append([]string{"-x"}, names...))
}

// lsOutput runs `archive ls <pattern>` capturing stdout (a wildcard reaches archive's
// own remote glob — we exec without a shell, so nothing expands it locally first).
func lsOutput(bin, pattern string) (string, error) {
	out, err := exec.Command(bin, "ls", pattern).Output()
	return string(out), err
}

// chunkNames picks a leaf's chunk tars out of `archive ls` output: the basenames
// matching <base>.tar or <base>_<suffix>.tar — the exact split set, so a sibling run in
// the same case container (e.g. 2500 next to 250) can't be swept in by the ls wildcard.
// It reduces to basenames, tolerating ls listing bare names or full paths.
func chunkNames(base, lsOut string) []string {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(base) + `(_.*)?\.tar$`)
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.Fields(lsOut) {
		if n := filepath.Base(f); re.MatchString(n) && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// planDir maps one dir arg to its packs: a case/run leaf packs itself (a guard
// violation — wrong tier — aborts); a parent with case-leaf children packs per
// the batch tier, skipping guarded leaves; anything else returns nil (passthrough).
func planDir(dir string) ([]pack, int) {
	if _, _, ok := mirror.ClassifyCase(filepath.Base(dir)); ok {
		ps, err := leafPack(dir)
		if err != nil {
			render.Err(err.Error())
			return nil, 2
		}
		return ps, 0
	}
	leaves, others := caseLeaves(dir)
	if len(leaves) == 0 {
		return nil, 0
	}
	small := true
	for _, l := range leaves {
		if duBytes(l) >= config.TarParentThreshold() {
			small = false
			break
		}
	}
	if small {
		proj, err := mirror.Archive(dir)
		if err != nil {
			render.Err(err.Error())
			return nil, 2
		}
		return []pack{{dir, filepath.Dir(proj), filepath.Base(proj) + ".tar", nil}}, 0
	}
	var out []pack
	for _, l := range leaves {
		ps, err := leafPack(l)
		if err != nil {
			// a staged bare case beside its runs is normal on scratch — the
			// authored input archives from $HOME, so skip it, don't abort the batch
			render.Warn("skipping " + filepath.Base(l) + " — " + err.Error())
			others++
			continue
		}
		out = append(out, ps...)
	}
	if len(out) == 0 {
		render.Err("nothing packable in " + dir + " — every case leaf skipped")
		return nil, 2
	}
	if others > 0 {
		render.Warn(fmt.Sprintf("%d non-case entries in %s skipped — put them explicitly", others, dir))
	}
	return out, 0
}

// leafPack builds the packs for one case/run leaf. Below tar_hook_threshold, or with
// no model pack hook, it's ONE tar at the leaf's projection (…/case_a/case_a.tar) with
// the flat local name as the member root, so get+extract on scratch recreates the dir
// exactly. An oversize leaf that ships a pack hook is SPLIT: the hook names member
// globs per chunk, mu expands them, sweeps anything unmatched into a <base>_rest.tar,
// and injects run.toml into every chunk. A broken, empty, or absent hook degrades to
// the single tar — a hook never blocks an archive. The projection call is also the
// provenance guard (inputs from permanent, runs from scratch).
func leafPack(dir string) ([]pack, error) {
	proj, err := mirror.Archive(dir)
	if err != nil {
		return nil, err
	}
	base, dst := filepath.Base(proj), filepath.Dir(proj)
	whole := []pack{{dir: dir, dst: dst, name: base + ".tar"}}
	if duBytes(dir) < config.TarHookThreshold() {
		return whole, nil
	}
	hook, ok := hooks.Find(dir, "pack")
	if !ok {
		render.Warn(fmt.Sprintf("%s is oversize — packing one tar; add a model pack hook to split it", base))
		return whole, nil
	}
	m, err := hooks.ExecPack(hook, dir)
	if err != nil {
		render.Warn(fmt.Sprintf("%s pack hook: %s — packing one tar", base, err))
		return whole, nil
	}
	packs, restCount, err := splitPacks(dir, base, dst, m)
	if err != nil {
		render.Warn(fmt.Sprintf("%s pack hook: %s — packing one tar", base, err))
		return whole, nil
	}
	if restCount > 0 {
		render.Warn(fmt.Sprintf("%s: %d file(s) not named by the pack hook → %s_rest.tar", base, restCount, base))
	}
	return packs, nil
}

// splitPacks expands a pack manifest against the leaf's real files into per-chunk
// packs. Each group's masks (filepath.Match globs, leaf-relative) select regular
// files; matching intersects the actual walk, so a mask can only ever name a file that
// exists — no path escape. A file claimed by two groups, or a bad/duplicate suffix, is
// a broken hook → error (leafPack then degrades to one tar). Files matched by no group
// become a <base>_rest.tar; run.toml is provenance — excluded from matching and
// injected into EVERY chunk. restCount is the rest-tar's file count (0 = full coverage,
// no rest tar).
func splitPacks(dir, base, dst string, m hooks.PackManifest) (packs []pack, restCount int, err error) {
	files, err := leafFiles(dir)
	if err != nil {
		return nil, 0, err
	}
	claimed := map[string]string{} // file → the suffix that owns it
	seen := map[string]bool{}      // suffixes used, to catch a tar-name collision
	for _, g := range m.Tars {
		if !validSuffix(g.Suffix) {
			return nil, 0, fmt.Errorf("invalid tar suffix %q", g.Suffix)
		}
		if seen[g.Suffix] {
			return nil, 0, fmt.Errorf("duplicate tar suffix %q", g.Suffix)
		}
		seen[g.Suffix] = true
		var members []string
		for _, f := range files {
			if !matchesAny(g.Members, f) {
				continue
			}
			if owner, dup := claimed[f]; dup {
				return nil, 0, fmt.Errorf("%s claimed by both %q and %q", f, owner, g.Suffix)
			}
			claimed[f] = g.Suffix
			members = append(members, f)
		}
		if len(members) == 0 {
			continue // a group that names nothing present — skip the empty chunk
		}
		packs = append(packs, pack{dir, dst, base + "_" + g.Suffix + ".tar", injectProvenance(dir, members)})
	}
	if len(packs) == 0 {
		return nil, 0, errors.New("matched no files")
	}
	var rest []string
	for _, f := range files {
		if _, ok := claimed[f]; !ok {
			rest = append(rest, f)
		}
	}
	if len(rest) > 0 {
		packs = append(packs, pack{dir, dst, base + "_rest.tar", injectProvenance(dir, rest)})
	}
	return packs, len(rest), nil
}

// leafFiles lists dir's regular files as leaf-relative slash paths, skipping the
// provenance record (injected into every chunk separately) — the set the manifest
// masks match against.
func leafFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); rel != provenanceFile {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// matchesAny reports whether rel matches any of the leaf-relative masks (filepath.Match
// per segment — a glob does not cross "/").
func matchesAny(masks []string, rel string) bool {
	for _, pat := range masks {
		if ok, _ := filepath.Match(pat, rel); ok {
			return true
		}
	}
	return false
}

// injectProvenance appends run.toml to a chunk's members when the leaf has one, so every
// chunk carries the run's record; absent → members unchanged.
func injectProvenance(dir string, members []string) []string {
	if _, err := os.Stat(filepath.Join(dir, provenanceFile)); err == nil {
		return append(members, provenanceFile)
	}
	return members
}

// validSuffix guards a hook-supplied tar suffix: it becomes a filename component
// (<base>_<suffix>.tar), so no separators, no parent refs, just the safe name chars.
func validSuffix(s string) bool {
	if s == "" || strings.ContainsAny(s, `/\`) || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			continue
		}
		return false
	}
	return true
}

// runPack stages the tar next to the dir, puts it with -D (native
// delete-local-after-verify), and removes any leftover staging either way —
// the belt for a failed put and for stubs that don't honor -D.
func runPack(bin string, p pack) int {
	staging := filepath.Join(filepath.Dir(p.dir), p.name)
	if _, err := os.Stat(staging); err == nil {
		render.Err(staging + " already exists — remove it or put it explicitly")
		return 1
	}
	if rc := tar.CreateRootedSubset(p.dir, staging, p.members); rc != 0 {
		return rc
	}
	rc := run(bin, filepath.Dir(p.dir), p.dst, "put", []string{"-D", p.name})
	_ = os.Remove(staging)
	return rc
}

// caseLeaves splits dir's child dirs into case/run leaves and the count of
// everything else (files included — a batch tar takes the whole dir, so the
// count only matters when the parent falls to per-leaf packing).
func caseLeaves(dir string) (leaves []string, others int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	for _, e := range entries {
		if _, _, ok := mirror.ClassifyCase(e.Name()); ok && e.IsDir() {
			leaves = append(leaves, filepath.Join(dir, e.Name()))
			continue
		}
		others++
	}
	return leaves, others
}

// duBytes is the best-effort recursive size of dir for the tier thresholds.
func duBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// hasFlags reports any dash-leading arg: value-taking site flags (-retry N)
// make path detection unsafe, so a flagged put skips packing and passes through.
func hasFlags(args []string) bool {
	for _, a := range args {
		if len(a) > 0 && a[0] == '-' {
			return true
		}
	}
	return false
}
