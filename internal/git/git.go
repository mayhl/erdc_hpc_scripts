// Package git backs the `mu git` module: read-only PREVIEWS of the .config
// signwip / pushsigned workflow — what WOULD sign or push — plus a doctor check.
// It never mutates history; the shell `gsw`/`gps` stay the source of truth and do
// the actual signing/pushing (bootstrap-safe: signing mu's own repo never depends
// on a working mu). All queries shell out to `git` in the current directory.
package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const unreviewed = "[unreviewed] "

// out runs `git args...` in the CWD and returns trimmed stdout; non-zero exit → err.
func out(args ...string) (string, error) {
	b, err := exec.Command("git", args...).Output()
	return strings.TrimSpace(string(b)), err
}

// WipRow is one line of the signwip preview. The base (newest signed) row carries
// Act "base"; the WIP stacked on it carries "sign" (untagged) or "skip" ([unreviewed]).
type WipRow struct {
	Act     string // "base" | "sign" | "skip"
	Hash    string // short
	Subject string
}

// Signwip is the read-only signwip preview: the newest signed commit (base) and the
// unsigned WIP stacked on it, each marked sign (untagged) or skip ([unreviewed]).
type Signwip struct {
	HasBase bool
	Rows    []WipRow
	ToSign  int
	Tagged  int
	Total   int
}

// signedHash returns the commit hash if line ("hash %G?") is Good-signed (2nd field "G"),
// else "", false. The per-line predicate of prefixBase — pure, so it's unit-tested.
func signedHash(line string) (string, bool) {
	if f := strings.Fields(line); len(f) >= 2 && f[1] == "G" {
		return f[0], true
	}
	return "", false
}

// wipBase resolves the boundary the signwip workflow stacks on: the top of the
// CONTIGUOUS signed prefix above the pushed floor (the upstream merge-base; no upstream
// → "" = the root, so a fresh repo bootstraps). Not simply the newest signed commit — a
// signed commit can sit above unsigned work (a [unreviewed] skipped mid-stack and
// reviewed later, or a pinentry-failed sign), and those buried commits must stay in
// range. Pushed history is never treated as WIP, signed or not. gpg verification is
// floor-bounded (unpushed commits only). Mirrors the shell scripts' base block.
func wipBase() (string, error) {
	floor, ferr := out("merge-base", "HEAD", "@{u}")
	if ferr != nil {
		floor = "" // no upstream — the prefix walk starts at the root
	}
	raw, err := out("log", "--reverse", "--format=%H %G?", wipRange(floor))
	if err != nil {
		return "", err
	}
	return prefixBase(splitNonEmpty(raw), floor), nil
}

// prefixBase walks the "hash %G?" lines (oldest→newest) and returns the last hash of
// the leading all-signed run, or floor when the very first commit is unsigned. The pure
// core of wipBase — the sandwich case (unsigned under signed) is unit-tested here.
func prefixBase(lines []string, floor string) string {
	base := floor
	for _, ln := range lines {
		h, ok := signedHash(ln)
		if !ok {
			break
		}
		base = h
	}
	return base
}

// wipRange is the log range covering the WIP above base — all of history when there is
// no base (never-signed, no-upstream repo).
func wipRange(base string) string {
	if base == "" {
		return "HEAD"
	}
	return base + "..HEAD"
}

// SignwipPreview mirrors git-signwip.sh's preview without signing anything.
func SignwipPreview() (Signwip, error) {
	var s Signwip
	base, err := wipBase()
	if err != nil {
		return s, err
	}
	s.HasBase = true
	if base != "" {
		if bh, err := out("log", "-1", "--format=%h%x09%s", base); err == nil {
			if h, sub, ok := strings.Cut(bh, "\t"); ok {
				s.Rows = append(s.Rows, WipRow{Act: "base", Hash: h, Subject: sub})
			}
		}
	}
	wip, err := out("log", "--reverse", "--format=%h%x09%s", wipRange(base))
	if err != nil {
		return s, err
	}
	var rows []WipRow
	rows, s.Total, s.Tagged = classifySign(splitNonEmpty(wip))
	s.Rows = append(s.Rows, rows...)
	s.ToSign = s.Total - s.Tagged
	return s, nil
}

// classifySign marks the signwip WIP lines ("hash\tsubject", oldest→newest): an [unreviewed]
// commit as skip (signwip signs only untagged), the rest as sign. Returns the rows plus the
// total and tagged counts. Pure (no git) so the sign/skip split is unit-tested.
func classifySign(lines []string) (rows []WipRow, total, tagged int) {
	for _, line := range lines {
		h, sub, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		total++
		act := "sign"
		if strings.HasPrefix(sub, unreviewed) {
			act = "skip"
			tagged++
		}
		rows = append(rows, WipRow{Act: act, Hash: h, Subject: sub})
	}
	return rows, total, tagged
}

// ReviewRow is one line of the reviewed preview: a commit in the unsigned WIP stack
// above the signed base. Act is "untag" (an [unreviewed] commit among the oldest-N that
// `git reviewed` would clear), "keep" (an [unreviewed] commit beyond N, left tagged),
// "clean" (already untagged), or "base" (the newest signed commit).
type ReviewRow struct {
	Act     string
	Hash    string
	Subject string
}

// Reviewed is the read-only `git reviewed [N]` preview: the WIP above the signed base,
// with the oldest-N [unreviewed] commits marked untag.
type Reviewed struct {
	HasBase bool
	Rows    []ReviewRow
	Tagged  int // total [unreviewed] above base
	Untag   int // how many the oldest-N selection clears
}

// ReviewedPreview mirrors git-reviewed.sh's preview without un-tagging anything: it marks
// the oldest n [unreviewed] commits above the signed base as untag (n<=0 → all of them),
// the rest keep, oldest-first.
func ReviewedPreview(n int) (Reviewed, error) {
	var r Reviewed
	base, err := wipBase()
	if err != nil {
		return r, err
	}
	r.HasBase = true
	if base != "" {
		if bh, err := out("log", "-1", "--format=%h%x09%s", base); err == nil {
			if h, sub, ok := strings.Cut(bh, "\t"); ok {
				r.Rows = append(r.Rows, ReviewRow{Act: "base", Hash: h, Subject: sub})
			}
		}
	}
	wip, err := out("log", "--reverse", "--format=%h%x09%s", wipRange(base))
	if err != nil {
		return r, err
	}
	rows, tagged, untag := classifyWip(splitNonEmpty(wip), n)
	r.Rows = append(r.Rows, rows...)
	r.Tagged, r.Untag = tagged, untag
	return r, nil
}

// classifyWip marks the WIP log lines ("hash\tsubject", oldest→newest): the oldest n
// [unreviewed] commits as untag (n<=0 or n>tagged → all of them), any later [unreviewed]
// as keep, and already-untagged commits as clean. Returns the rows plus the total tagged
// count and how many were selected to untag. Pure (no git) so the selection is unit-tested.
func classifyWip(lines []string, n int) (rows []ReviewRow, tagged, untag int) {
	for _, line := range lines {
		if _, sub, ok := strings.Cut(line, "\t"); ok && strings.HasPrefix(sub, unreviewed) {
			tagged++
		}
	}
	lim := n
	if lim <= 0 || lim > tagged {
		lim = tagged
	}
	for _, line := range lines {
		h, sub, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		act := "clean"
		if strings.HasPrefix(sub, unreviewed) {
			if untag < lim {
				act, untag = "untag", untag+1
			} else {
				act = "keep"
			}
		}
		rows = append(rows, ReviewRow{Act: act, Hash: h, Subject: sub})
	}
	return rows, tagged, untag
}

// Message returns a commit's full message (subject + body) — the `i` inspect
// overlay of the reviewed cut-point picker.
func Message(hash string) (string, error) {
	return out("show", "-s", "--format=%B", hash)
}

// WipMessages returns short-hash → full message for every commit above the signed
// base, in ONE git call — the -i picker preloads its preview pane from this map, so
// cursor moves never wait on a git spawn.
func WipMessages() (map[string]string, error) {
	base, err := wipBase()
	if err != nil {
		return nil, err
	}
	raw, err := out("log", "--format=%h%x1f%B%x1e", wipRange(base))
	if err != nil {
		return nil, err
	}
	msgs := make(map[string]string)
	for _, rec := range strings.Split(raw, "\x1e") {
		if h, body, ok := strings.Cut(rec, "\x1f"); ok {
			msgs[strings.TrimSpace(h)] = strings.TrimSpace(body)
		}
	}
	return msgs, nil
}

// StripTag drops the [unreviewed] marker from a subject (no-op when untagged).
func StripTag(subject string) string {
	return strings.TrimPrefix(subject, unreviewed)
}

// PushRow is one line of the pushsigned preview.
type PushRow struct {
	Push    bool // in the contiguous signed prefix → would push
	Signed  bool // %G? == G
	Hash    string
	Subject string
}

// Pushsigned is the read-only pushsigned preview: the commits ahead of @{u}, with
// the contiguous signed prefix marked push and the rest held local.
type Pushsigned struct {
	HasUpstream bool
	Upstream    string
	Rows        []PushRow
	PushN       int
	Held        int
}

// PushsignedPreview mirrors git-pushsigned.sh's preview without pushing anything.
func PushsignedPreview() (Pushsigned, error) {
	var p Pushsigned
	up, err := out("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil || up == "" {
		return p, nil // no upstream — HasUpstream stays false
	}
	p.HasUpstream = true
	p.Upstream = up
	raw, err := out("log", "--reverse", "--format=%h%x09%G?%x09%s", "@{u}..HEAD")
	if err != nil {
		return p, err
	}
	p.Rows, p.PushN, p.Held = classifyPush(splitNonEmpty(raw))
	return p, nil
}

// classifyPush marks the pushsigned log lines ("hash\t%G?\tsubject", oldest→newest): the
// contiguous signed (%G?=="G") PREFIX as push, and everything from the first unsigned commit
// onward as held — even a later-signed commit, since pushing it would carry the unsigned one
// beneath it. Returns the rows plus the push and held counts. Pure (no git) so the
// prefix rule is unit-tested.
func classifyPush(lines []string) (rows []PushRow, pushN, held int) {
	inPrefix := true
	for _, line := range lines {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		signed := parts[1] == "G"
		if inPrefix && signed {
			rows = append(rows, PushRow{Push: true, Signed: true, Hash: parts[0], Subject: parts[2]})
			pushN++
			continue
		}
		inPrefix = false
		rows = append(rows, PushRow{Push: false, Signed: signed, Hash: parts[0], Subject: parts[2]})
		held++
	}
	return rows, pushN, held
}

// FileCheck is one existence check for the doctor report.
type FileCheck struct {
	Name   string
	Path   string
	Exists bool
}

// Doctor is the read-only `mu git doctor` report: git on PATH and the .config git
// workflow files present. Existence only — the files are not validated.
type Doctor struct {
	GitPath string // resolved `git` path, or "" if not found
	Files   []FileCheck
}

// DoctorReport checks git-on-PATH plus the ~/.config git config and workflow scripts.
func DoctorReport() Doctor {
	var d Doctor
	d.GitPath, _ = exec.LookPath("git")
	cfg := configHome()
	for _, c := range []struct{ name, path string }{
		{"git config", filepath.Join(cfg, "git", "config")},
		{"git-commitinc.sh", filepath.Join(cfg, "scripts", "git-commitinc.sh")},
		{"git-reviewed.sh", filepath.Join(cfg, "scripts", "git-reviewed.sh")},
		{"git-signwip.sh", filepath.Join(cfg, "scripts", "git-signwip.sh")},
		{"git-pushsigned.sh", filepath.Join(cfg, "scripts", "git-pushsigned.sh")},
	} {
		_, err := os.Stat(c.path)
		d.Files = append(d.Files, FileCheck{Name: c.name, Path: c.path, Exists: err == nil})
	}
	return d
}

func configHome() string {
	if p := os.Getenv("XDG_CONFIG_HOME"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

// splitNonEmpty splits on newlines, dropping the empty tail from an empty string.
func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
