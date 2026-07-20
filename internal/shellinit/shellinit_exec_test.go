package shellinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayhl/mayhl_utils/internal/config"
)

// TestDispatchExec sources the generated dispatcher in a real shell and exercises
// the grammar end-to-end, so a parse/dispatch regression is caught (the string
// assertions in TestGenerate only pin the codegen text). It runs under both bash
// and zsh — the two shells the dispatcher must stay portable across — skipping a
// shell that isn't installed. The seam helpers are stubbed to echo what they would
// do (mu_auth ok, mu_ssh_login/mu/$MU_SSH print their target/args).
func TestDispatchExec(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	body := `
hpc_user = "alice"
[[cluster]]
name = "alpha"
domain = "alpha.example.mil"
nodes = ["hpc1"]
`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MU_CONFIG_FILE", cfg)
	t.Setenv("MU_NODE", "none") // this shell isn't hpc1 → its dispatcher is emitted
	config.ResetForTest()       // config memoizes per-process; reload from this file

	// Stub the framework seams, source the generated dispatchers, then run every
	// grammar arm. mu_ssh_login prints CONNECT <target>; $MU_SSH (fakessh) prints
	// SSH <target> :: <remote-cmd> so numbered remote-exec is observable too, and
	// writes two stderr lines — a benign dbus line (must be dropped by the
	// dispatcher's stderr filter) and a real error (must survive it).
	driver := `
mu_auth() { return 0; }
mu_ssh_login() { echo "CONNECT $1"; }
mu() { echo "MU $*"; }
fakessh() {
  echo "SSH $2 :: $3"
  echo "dbus-update-activation-environment: noise" >&2
  echo "real-error-boom" >&2
}
export MU_SSH=fakessh
` + Generate() + `
hpc1
hpc1 3
hpc1 12
hpc1 uptime
hpc1 3 uptime
hpc1 push a b
hpc1 shell --debug
hpc1 sub run.pbs -n 4
hpc1 queues
hpc1 usage
hpc1 stat
hpc1 hold 5
hpc1 tunnel
hpc1 exec usage
hpc1 -- queues
hpc1 -h
`
	script := filepath.Join(dir, "driver.sh")
	if err := os.WriteFile(script, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}

	wants := []string{
		"CONNECT alice@hpc1.alpha.example.mil",   // bare connect
		"CONNECT alice@hpc103.alpha.example.mil", // numbered connect (N=3, zero-padded)
		"CONNECT alice@hpc112.alpha.example.mil", // two-digit number passes through
		"SSH alice@hpc1.alpha.example.mil :: ",   // remote-exec, default login
		"SSH alice@hpc103.alpha.example.mil :: ", // remote-exec on numbered login node
		"MU cp push hpc1 a b",                    // push stays node-level
		// The reserved verbs route to the mu command that takes a --node anyway.
		"MU job shell --node hpc1 --debug",
		"MU job sub --node hpc1 run.pbs -n 4",
		"MU hpc queues --node hpc1",
		"MU hpc usage --node hpc1",
		// The systematic Class-A arms: the idiom queue verb (bare `stat`), a queue sub-verb, and
		// the job plane — all generated from classAVerbs, all injecting --node.
		"MU hpc queue --node hpc1",        // stat (pbs bare verb → mu hpc queue)
		"MU hpc queue hold --node hpc1 5", // hold sub-verb with a selector
		"MU job tunnel --node hpc1",       // job-plane node-first
		// ...and `exec` / `--` force remote-exec of a word that is otherwise reserved.
		"SSH alice@hpc1.alpha.example.mil :: bash -lc \"usage\"",
		"SSH alice@hpc1.alpha.example.mil :: bash -lc \"queues\"",
		"MU setup node-help hpc1", // -h calls back into mu for the house panel
		"real-error-boom",         // a real stderr line survives the filter
	}
	// The benign dbus login-profile noise must be dropped by the stderr filter.
	notWants := []string{"dbus-update-activation-environment"}

	for _, sh := range []string{"bash", "zsh"} {
		t.Run(sh, func(t *testing.T) {
			bin, err := exec.LookPath(sh)
			if err != nil {
				t.Skipf("%s not on PATH", sh)
			}
			out, err := exec.Command(bin, script).CombinedOutput()
			if err != nil {
				t.Fatalf("%s exited with error: %v\n%s", sh, err, out)
			}
			got := string(out)
			for _, w := range wants {
				if !strings.Contains(got, w) {
					t.Errorf("%s: missing %q in output:\n%s", sh, w, got)
				}
			}
			for _, nw := range notWants {
				if strings.Contains(got, nw) {
					t.Errorf("%s: %q should have been filtered from stderr but appeared:\n%s", sh, nw, got)
				}
			}
		})
	}
}

// TestSeamSelfSufficient verifies the generated shell-init DEFINES the connectivity seam
// (mu_auth/mu_ssh_login/$MU_SSH) and its support libs (mu_log/mu_indirect/…) on its own —
// so a box with only the mu binary + config (no mayhl_utils checkout, no init.sh) has a
// working shell layer. This is the inverse of TestDispatchExec, which pre-stubs the seam
// and relies on the guards making the emitted blocks a no-op.
func TestSeamSelfSufficient(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	body := `
hpc_user = "alice"
[[cluster]]
name = "alpha"
domain = "alpha.example.mil"
nodes = ["hpc1"]
`
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MU_CONFIG_FILE", cfg)
	t.Setenv("MU_SYSTEM", "hpc") // emit the HPC seam (plain ssh, no-op auth)
	t.Setenv("MU_NODE", "none")
	config.ResetForTest()

	// No stubs — a bare shell. Eval the generated layer, then report each helper: the
	// seam + support libs, the shared tooling (tar/status/utils), and the front-doors.
	driver := Generate() + `
for f in mu_log mu_indirect mu_have mu_auth mu_ssh_login qtar mu_status gkill mu_run mps mlog; do
  command -v "$f" >/dev/null 2>&1 && echo "HAVE $f" || echo "MISS $f"
done
[ -n "$MU_SSH" ] && echo "MU_SSH_SET"
`
	script := filepath.Join(dir, "driver.sh")
	if err := os.WriteFile(script, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	wants := []string{
		"HAVE mu_log", "HAVE mu_indirect", "HAVE mu_have", "HAVE mu_auth", "HAVE mu_ssh_login",
		"HAVE qtar", "HAVE mu_status", "HAVE gkill", "HAVE mu_run", "HAVE mps", "HAVE mlog", "MU_SSH_SET",
	}
	for _, sh := range []string{"bash", "zsh"} {
		t.Run(sh, func(t *testing.T) {
			bin, err := exec.LookPath(sh)
			if err != nil {
				t.Skipf("%s not on PATH", sh)
			}
			out, err := exec.Command(bin, script).CombinedOutput()
			if err != nil {
				t.Fatalf("%s exited with error: %v\n%s", sh, err, out)
			}
			got := string(out)
			if strings.Contains(got, "MISS ") {
				t.Errorf("%s: a seam helper was undefined (not self-sufficient):\n%s", sh, got)
			}
			for _, w := range wants {
				if !strings.Contains(got, w) {
					t.Errorf("%s: missing %q:\n%s", sh, w, got)
				}
			}
		})
	}
}

// TestDoctorCheckupExec evaluates the throttled-checkup snippet in real bash AND zsh:
// in an INTERACTIVE shell a missing/stale stamp backgrounds `mu doctor --checkup`
// (observed via a stub mu that drops a marker file — the run is a disowned grandchild,
// so the driver polls for it), a fresh stamp doesn't fire, and a doctor.notice is
// printed at startup; a NON-interactive shell fires nothing and prints nothing (its
// stdout is often captured — `ssh host bash -lc …`, $(…) — the notice would corrupt it).
func TestDoctorCheckupExec(t *testing.T) {
	for _, sh := range []string{"bash", "zsh"} {
		bin, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s not installed — skipped", sh)
			continue
		}
		t.Run(sh, func(t *testing.T) {
			run := func(prep string, interactive bool) string {
				t.Helper()
				cache := t.TempDir()
				driver := `mu() { : > "$XDG_CACHE_HOME/mu-called"; }
` + prep + doctorCheckup() + `
# the notice now fires from a precmd/PROMPT_COMMAND hook — this driver never reaches a
# prompt, so invoke the registered one-shot directly to observe it.
type _mu_doctor_notice >/dev/null 2>&1 && _mu_doctor_notice
i=0
while [ ! -f "$XDG_CACHE_HOME/mu-called" ] && [ "$i" -lt 20 ]; do sleep 0.05; i=$((i+1)); done
[ -f "$XDG_CACHE_HOME/mu-called" ] && echo "FIRED" || echo "SKIPPED"
`
				// -i without a tty is fine under -c; rc files are suppressed
				// (--norc / -f) so the user's shell config can't pollute the run.
				args := []string{"-c", driver}
				if interactive {
					if sh == "bash" {
						args = []string{"--norc", "-i", "-c", driver}
					} else {
						args = []string{"-f", "-i", "-c", driver}
					}
				}
				cmd := exec.Command(bin, args...)
				cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+cache)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("driver: %v\n%s", err, out)
				}
				return string(out)
			}

			if got := run("", true); !strings.Contains(got, "FIRED") {
				t.Errorf("no stamp: want a checkup fired, got:\n%s", got)
			}
			fresh := `mkdir -p "$XDG_CACHE_HOME/mayhl_utils"
date +%s > "$XDG_CACHE_HOME/mayhl_utils/doctor.stamp"
echo "NAG-LINE" > "$XDG_CACHE_HOME/mayhl_utils/doctor.notice"
`
			got := run(fresh, true)
			if !strings.Contains(got, "SKIPPED") {
				t.Errorf("fresh stamp: want no checkup, got:\n%s", got)
			}
			if !strings.Contains(got, "NAG-LINE") {
				t.Errorf("notice not printed at startup:\n%s", got)
			}

			if got := run(fresh, false); strings.Contains(got, "NAG-LINE") {
				t.Errorf("non-interactive shell printed the notice (captured stdout!):\n%s", got)
			}
			if got := run("", false); !strings.Contains(got, "SKIPPED") {
				t.Errorf("non-interactive shell fired a checkup:\n%s", got)
			}
		})
	}
}

// TestClipToolsExec runs the emitted mu_clip in real bash AND zsh and pins the exact
// OSC 52 bytes — args and stdin forms, plus the tmux DCS passthrough wrap. The test
// has no controlling terminal, so MU_CLIP_TTY routes the write to stdout.
func TestClipToolsExec(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		cmd  string
		want string
	}{
		{"args", nil, `mu_clip x`, "\x1b]52;c;eA==\a"},
		{"stdin", nil, `printf '%s' hello | mu_clip`, "\x1b]52;c;aGVsbG8=\a"},
		{"tmux wrap", []string{"TMUX=1"}, `mu_clip x`, "\x1bPtmux;\x1b\x1b]52;c;eA==\a\x1b\\"},
	}
	for _, sh := range []string{"bash", "zsh"} {
		p, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s not installed — skipped", sh)
			continue
		}
		for _, c := range cases {
			cmd := exec.Command(p, "-c", clipTools()+c.cmd)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "MU_CLIP_TTY=/dev/stdout"}, c.env...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s/%s: %v\n%q", sh, c.name, err, out)
			}
			if got := string(out); got != c.want {
				t.Errorf("%s/%s: emitted %q, want %q", sh, c.name, got, c.want)
			}
		}
	}
}

// TestMiseEnvExec evaluates the emitted MISE_ENV composition in real bash AND zsh
// across the tier combos: hpc composes on an HPC box, is skipped when the mu-toolchain
// module marker (MU_TOOLCHAIN) is set, fmt rides MU_MODULES, and a pre-set MISE_ENV
// survives. Bash parity is the point — this logic used to live zsh-only in .config.
func TestMiseEnvExec(t *testing.T) {
	script := miseEnv() + `printf '%s' "${MISE_ENV:-none}"`
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"local", nil, "none"},
		{"hpc via BC_HOST", []string{"BC_HOST=x"}, "hpc"},
		{"hpc via MU_SYSTEM", []string{"MU_SYSTEM=hpc"}, "hpc"},
		{"module provides toolchain", []string{"BC_HOST=x", "MU_TOOLCHAIN=/opt/tc"}, "none"},
		{"fmt only under module", []string{"BC_HOST=x", "MU_TOOLCHAIN=/opt/tc", "MU_MODULES=git,fmt"}, "fmt"},
		{"fmt in a space list", []string{"BC_HOST=x", "MU_TOOLCHAIN=/opt/tc", "MU_MODULES=fmt git"}, "fmt"},
		{"fmt not a substring", []string{"BC_HOST=x", "MU_TOOLCHAIN=/opt/tc", "MU_MODULES=fmtx gitfmt"}, "none"},
		{"hpc+fmt compose", []string{"BC_HOST=x", "MU_MODULES=fmt"}, "hpc,fmt"},
		{"pre-set survives", []string{"MISE_ENV=pre", "BC_HOST=x"}, "pre,hpc"},
	}
	for _, sh := range []string{"bash", "zsh"} {
		p, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("%s not installed — skipped", sh)
			continue
		}
		for _, c := range cases {
			cmd := exec.Command(p, "-c", script)
			cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, c.env...) // clean env: no inherited MU_*/BC_HOST
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s/%s: %v\n%s", sh, c.name, err, out)
			}
			if got := string(out); got != c.want {
				t.Errorf("%s/%s: MISE_ENV = %q, want %q", sh, c.name, got, c.want)
			}
		}
	}
}
