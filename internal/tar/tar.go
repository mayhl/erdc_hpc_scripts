// Package tar wraps the system `tar` binary with a house progress bar. It shells
// out to real tar (keeping sparse-file / xattr / symlink correctness that a
// pure-Go archive/tar would miss on scientific data) and meters the pipe with a
// Go counting reader/writer driving render.ProgressBar — replacing the retired
// tqdm-based shell helpers. Verb is inferred from the path: a directory is
// archived, an archive is extracted.
package tar

import (
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mayhl/mayhl_utils/internal/render"
)

// Run archives dir → tar (gzip when useGzip) or extracts an archive, inferring
// which from the path suffix. Returns a process exit code. useGzip is ignored on
// extract (compression is auto-detected by tar).
func Run(path string, useGzip bool) int {
	if isArchive(path) {
		return extract(path)
	}
	return create(path, useGzip)
}

func isArchive(p string) bool {
	return strings.HasSuffix(p, ".tar") || strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz")
}

// archiveName is the output name for archiving dir: dir.tar, or dir.tar.gz.
func archiveName(dir string, useGzip bool) string {
	base := strings.TrimRight(dir, "/")
	if useGzip {
		return base + ".tar.gz"
	}
	return base + ".tar"
}

func create(dir string, useGzip bool) int {
	return createStream(dir, archiveName(dir, useGzip), useGzip, false)
}

// CreateRooted archives dir into the named tar with members rooted at the dir's
// basename (tar runs from the parent), so extraction recreates the dir exactly —
// the archive put wrapper's staging shape (case_a_123/… inside 123.tar).
func CreateRooted(dir, out string) int {
	return createStream(dir, out, false, true)
}

// CreateRootedSubset archives a subset of dir's members into out, each rooted at dir's
// basename (tar runs from the parent) — so the chunk tars split from one leaf all
// extract back into the same dir. members are dir-relative paths; empty members packs
// the whole dir (identical to CreateRooted).
func CreateRootedSubset(dir, out string, members []string) int {
	if len(members) == 0 {
		return CreateRooted(dir, out)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		render.Err("not a directory: " + dir)
		return 1
	}
	base := filepath.Base(dir)
	args := make([]string, len(members))
	var total int64
	for i, m := range members {
		args[i] = filepath.Join(base, m) // root each member under the leaf basename
		if fi, err := os.Stat(filepath.Join(dir, m)); err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
	}
	return tarTo(filepath.Dir(dir), args, out, false, total)
}

// createStream tars dir → archive, metering the pipe; rooted runs tar from the
// parent with the basename as the member root (else dir as given, from CWD).
func createStream(dir, archive string, useGzip, rooted bool) int {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		render.Err("not a directory: " + dir)
		return 1
	}
	total, _ := dirSize(dir) // best-effort total for the bar (tar adds header overhead)
	arg, cmdDir := dir, ""
	if rooted {
		arg, cmdDir = filepath.Base(dir), filepath.Dir(dir)
	}
	return tarTo(cmdDir, []string{arg}, archive, useGzip, total)
}

// tarTo runs `tar -cf - <tarArgs>` from cmdDir ("" = inherit CWD), streaming into
// archive (gzip when useGzip) and metering the pipe against total (best-effort, for
// the bar). The single seam every create path shares.
func tarTo(cmdDir string, tarArgs []string, archive string, useGzip bool, total int64) int {
	out, err := os.Create(archive)
	if err != nil {
		render.Err(err.Error())
		return 1
	}
	defer func() { _ = out.Close() }()

	var dest io.Writer = out
	var gz *gzip.Writer
	if useGzip {
		gz = gzip.NewWriter(out)
		dest = gz
	}

	cmd := exec.Command("tar", append([]string{"-cf", "-"}, tarArgs...)...)
	cmd.Dir = cmdDir
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		render.Err(err.Error())
		return 1
	}
	if err := cmd.Start(); err != nil {
		render.Err(err.Error())
		return 1
	}

	bar := render.NewProgressBar("tar " + filepath.Base(archive))
	m := newMeter(bar, total)
	_, copyErr := io.Copy(&meterWriter{w: dest, m: m}, stdout)
	if gz != nil {
		_ = gz.Close()
	}
	waitErr := cmd.Wait()
	bar.Finish()

	if err := firstErr(copyErr, waitErr); err != nil {
		render.Err("tar failed: " + err.Error())
		_ = os.Remove(archive) // don't leave a partial archive
		return 1
	}
	sz, _ := fileSize(archive)
	render.OK(fmt.Sprintf("created %s (%s)", archive, render.HumanBytes(sz)))
	return 0
}

func extract(archive string) int {
	f, err := os.Open(archive)
	if err != nil {
		render.Err(err.Error())
		return 1
	}
	defer func() { _ = f.Close() }()
	total, _ := fileSize(archive)

	// tar -xf - auto-detects gzip from the stream (GNU tar ≥1.15 and bsdtar).
	cmd := exec.Command("tar", "-xf", "-", "-C", ".")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	bar := render.NewProgressBar("untar " + filepath.Base(archive))
	cmd.Stdin = &meterReader{r: f, m: newMeter(bar, total)}
	err = cmd.Run()
	bar.Finish()
	if err != nil {
		render.Err("tar extract failed: " + err.Error())
		return 1
	}
	render.OK("extracted " + filepath.Base(archive))
	return 0
}

// --- byte metering -----------------------------------------------------------

// meter converts a running byte count into progress-bar updates (pct + rate +
// ETA), throttled to ~10 redraws/sec so a multi-GB transfer doesn't flood stderr.
type meter struct {
	bar         *render.ProgressBar
	total, done int64
	start, last time.Time
}

func newMeter(bar *render.ProgressBar, total int64) *meter {
	now := time.Now()
	return &meter{bar: bar, total: total, start: now, last: now}
}

func (m *meter) add(n int) {
	m.done += int64(n)
	now := time.Now()
	final := m.total > 0 && m.done >= m.total
	if now.Sub(m.last) < 100*time.Millisecond && !final {
		return
	}
	m.last = now

	pct := 0
	if m.total > 0 {
		pct = int(m.done * 100 / m.total)
	}
	rate, eta := "", "--:--:--"
	if elapsed := now.Sub(m.start).Seconds(); elapsed > 0 {
		bps := float64(m.done) / elapsed
		rate = render.HumanRate(bps)
		switch {
		case m.total > m.done && bps > 0:
			eta = render.FmtETA(float64(m.total-m.done) / bps)
		case m.total > 0:
			eta = "0:00:00"
		}
	}
	m.bar.Update(pct, rate, eta)
}

type meterWriter struct {
	w io.Writer
	m *meter
}

func (mw *meterWriter) Write(p []byte) (int, error) {
	n, err := mw.w.Write(p)
	mw.m.add(n)
	return n, err
}

type meterReader struct {
	r io.Reader
	m *meter
}

func (mr *meterReader) Read(p []byte) (int, error) {
	n, err := mr.r.Read(p)
	mr.m.add(n)
	return n, err
}

// --- helpers -----------------------------------------------------------------

func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
