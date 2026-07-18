// Package hooks implements the model-hooks contract — per-model plugin scripts
// (the doctor checks.d pattern applied to models) living in PROJECT repos at
// scripts/<model>/hooks/<name>. mu owns only the contract and discovery; a
// missing hook is a graceful no-op everywhere. Contract: CWD = the case/run
// dir, stdout = one flat JSON object, exit 0/2/other = ok/warn/fail.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/mirror"
	"github.com/mayhl/mayhl_utils/internal/project"
)

// Timeout is the per-hook exec budget: a hook is a quick metadata probe, not a
// post-processor — anything slower must not stall a queue listing. A var, not a
// const, so tests can widen it (laptop AV scans fresh scripts on first exec —
// seconds under parallel test load, nothing to do with the contract).
var Timeout = 5 * time.Second

// Model extracts the model name from a case/run path — the component after a
// "simulations" segment (`…/simulations/<model>/…/case_X`), per the blessed
// layout. The shared-data tier (simulations/data) is not a model. Composite
// models (ww3-funwave) are just the dir name.
func Model(path string) (string, bool) {
	comps := splitPath(path)
	for i, c := range comps {
		if c == "simulations" && i+1 < len(comps) {
			m := comps[i+1]
			if filepath.Join("simulations", m) == config.ProjectDataDir() {
				return "", false
			}
			return m, true
		}
	}
	return "", false
}

// Dir resolves the hooks dir for a run/case path via the discovery chain:
// (1) the enclosing checkout of the path itself ($HOME-side runs, workdir
// checkouts); (2) the swap-mirrored counterpart's checkout — read-time run dirs
// live on scratch where iterate-mode staging is NOT a checkout, so the project
// (and its hooks) sit on the permanent tier. FUTURE tier: model-module shipped
// defaults. No checkout anywhere → ok=false, the graceful no-op.
func Dir(path string) (string, bool) {
	model, ok := Model(path)
	if !ok {
		return "", false
	}
	for _, base := range []func() (string, error){
		func() (string, error) { return path, nil },
		func() (string, error) { return mirror.Swap(path) },
	} {
		p, err := base()
		if err != nil {
			continue
		}
		root, err := project.FindRoot(p)
		if err != nil {
			continue
		}
		d := filepath.Join(root, "scripts", model, "hooks")
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return d, true
		}
	}
	return "", false
}

// Find locates one named hook for the path ("" when absent — callers no-op).
func Find(path, name string) (string, bool) {
	d, ok := Dir(path)
	if !ok {
		return "", false
	}
	h := filepath.Join(d, name)
	if info, err := os.Stat(h); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
		return h, true
	}
	return "", false
}

// List names every executable hook available for the path (for --full runs).
func List(path string) []string {
	d, ok := Dir(path)
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			out = append(out, e.Name())
		}
	}
	return out
}

// Result is one hook execution: the checks.d exit semantics (0/2/other =
// ok/warn/fail), the parsed flat JSON, and the parse/exec error if any. A
// timeout or unparsable stdout is a fail — consumers degrade to "no model data".
type Result struct {
	Hook string         `json:"hook"`
	Exit int            `json:"exit"`
	Data map[string]any `json:"data,omitempty"`
	Err  string         `json:"err,omitempty"`
}

// Exec runs one hook per the contract: CWD = the run/case dir, MU_JOBID in the
// env (read-time gets only CWD + jobid + run.toml on disk — NOT the in-job
// MU_JOB_* set), stdout parsed as one flat JSON object.
func Exec(hookPath, runDir, jobID string) Result {
	r := Result{Hook: filepath.Base(hookPath)}
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, hookPath)
	cmd.Dir = runDir
	cmd.Env = append(os.Environ(), "MU_JOBID="+jobID)
	out, err := cmd.Output()
	if err != nil {
		r.Exit = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			r.Exit = ee.ExitCode()
		}
		if ctx.Err() != nil {
			r.Err = "timeout"
			return r
		}
		if r.Exit != 2 { // warn still parses; fail doesn't
			r.Err = "exit " + strconv.Itoa(r.Exit)
			return r
		}
	}
	var data map[string]any
	if jerr := json.Unmarshal(out, &data); jerr != nil {
		r.Err = "stdout is not one flat JSON object"
		if r.Exit == 0 {
			r.Exit = 1
		}
		return r
	}
	r.Data = data
	return r
}

// PackTimeout is the pack hook's own exec budget. Unlike the read-time Timeout, the
// pack hook runs during `archive put` — a deliberate bulk op, not an inline listing —
// and may stat thousands of files on a cold Lustre cache, so it gets a far longer
// window. It stays a metadata probe (names groups by glob, never touches bytes), so
// the budget still bounds only manifest emission, not the tar+put that follows. A var
// so tests can shorten it.
var PackTimeout = 60 * time.Second

// PackGroup is one tar chunk the pack hook asks for: a filename suffix and the leaf-
// relative member masks (filepath.Match globs) mu expands into it.
type PackGroup struct {
	Suffix  string   `json:"suffix"`
	Members []string `json:"members"`
}

// PackManifest is the pack hook's whole output — the split plan for one leaf. An empty
// Tars (or no hook at all) leaves mu to pack the leaf as one tar.
type PackManifest struct {
	Tars []PackGroup `json:"tars"`
}

// ExecPack runs a leaf's pack hook (CWD = leafDir) and decodes its split manifest. It
// carries the PACK contract, not the flat-metrics one: stdout is one nested {"tars":[…]}
// object read under PackTimeout. Any failure — timeout, non-zero exit, unparsable
// stdout — is an error so the caller degrades to one whole-leaf tar; a broken hook never
// blocks an archive.
func ExecPack(hookPath, leafDir string) (PackManifest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), PackTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, hookPath)
	cmd.Dir = leafDir
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return PackManifest{}, errors.New("timed out")
	}
	if err != nil {
		return PackManifest{}, fmt.Errorf("%w", err)
	}
	var m PackManifest
	if jerr := json.Unmarshal(out, &m); jerr != nil {
		return PackManifest{}, errors.New(`stdout is not a {"tars":[…]} object`)
	}
	return m, nil
}

func splitPath(p string) []string {
	return strings.Split(filepath.ToSlash(filepath.Clean(p)), "/")
}
