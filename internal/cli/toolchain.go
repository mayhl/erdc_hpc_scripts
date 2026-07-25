package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/modules"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/setup"
)

// toolchainCmd is `mu setup toolchain`: install the dev toolchain with mise. Phase 2 of
// machine onboarding (runs ON the box, decoupled from the phase-1 push in `mu setup
// onboard`). Bare = self-install; --prefix <path> --module installs the shared HPC runtime
// (embedded manifest + config-resolved base/hpc tiers) and writes a Tcl modulefile of
// static prepend-paths for the users who follow. The base tool list is embedded in mu
// (self-contained). On macOS mise can't cover the Intel-mac-only tools, so it prints the
// Homebrew bootstrap for you to run instead of driving brew silently.
func toolchainCmd() *cobra.Command {
	var t toolchain
	var dump bool
	c := &cobra.Command{
		Use:   "toolchain",
		Short: "Install the mu dev toolchain (via mise).",
		Long: "Install the base CLI toolchain with mise. Bare installs it for you; --prefix\n" +
			"<path> --module installs the shared HPC runtime (embedded manifest + the\n" +
			"config-resolved base/hpc tiers) and writes a Tcl modulefile so other users can\n" +
			"`module load` it. The tool list is embedded in mu — print it with\n" +
			"--dump-manifest. On macOS this prints the Homebrew + mise bootstrap to run.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if dump {
				s, err := setup.DumpManifest()
				if err != nil {
					return fmt.Errorf("read toolchain manifest: %w", err)
				}
				fmt.Print(s)
				return nil
			}
			if t.module && t.prefix == "" {
				return usageErr("--module needs --prefix <path> (the shared install root)")
			}
			if runtime.GOOS == "darwin" {
				return t.darwin()
			}
			return t.linux()
		},
	}
	f := c.Flags()
	f.StringVar(&t.prefix, "prefix", "", "shared install root (MISE_DATA_DIR); default is mise's per-user dir")
	f.BoolVar(&t.module, "module", false, "write a Tcl modulefile pointing at --prefix (HPC deployer)")
	f.BoolVar(&t.withBrew, "with-brew", false, "on macOS, run the Homebrew bootstrap instead of just printing it")
	f.BoolVar(&t.dryRun, "dry-run", false, "print the plan without installing")
	f.BoolVar(&dump, "dump-manifest", false, "print the embedded toolchain manifest and exit")
	return c
}

type toolchain struct {
	prefix   string
	module   bool
	withBrew bool
	dryRun   bool
}

// linux installs the toolchain with mise: bootstrap mise if absent, then either the
// per-user emit+install (bare) or the shared-module deploy (--module). The bare path
// writes the embedded tiers to MISE_CONFIG_DIR (so the running shell resolves them) and
// runs a config-resolved `mise install` under the MU_MODULES-composed MISE_ENV.
func (t *toolchain) linux() error {
	mise, present := misePath()
	cfgDir := miseConfigDir()
	env := miseEnv()
	render.Info("toolchain plan (linux):")
	if present {
		render.Detail("  mise: " + mise)
	} else {
		render.Detail("  mise: not found → bootstrap (curl https://mise.run | sh)")
	}
	if t.module {
		render.Detail("  install root: " + t.prefix)
		render.Detail("  tiers: base + hpc (config-resolved, MISE_ENV=hpc)")
		render.Detail("  modulefile: " + t.modulefilePath() + " — one prepend-path per tool bin dir")
	} else {
		render.Detail("  config: " + cfgDir + " (emitted from the embedded tiers)")
		render.Detail("  tiers: MISE_ENV=" + env + " (base always; hpc; fmt/cast via MU_MODULES)")
	}
	if t.dryRun {
		render.OK("dry-run — nothing installed")
		return nil
	}

	if !present {
		if err := bootstrapMise(); err != nil {
			return err
		}
		mise, _ = misePath()
	}
	if t.module {
		return t.deployModule(mise)
	}
	if err := stageMiseConfigs(cfgDir); err != nil {
		return fmt.Errorf("emit mise config: %w", err)
	}
	installEnv := overrideEnv(os.Environ(), "MISE_ENV="+env)
	if err := runEnv(installEnv, mise, "install"); err != nil {
		return fmt.Errorf("mise install: %w", err)
	}
	_ = runEnv(installEnv, mise, "reshim")
	render.OK("installed toolchain (MISE_ENV=" + env + ") + wrote " + cfgDir)
	return nil
}

// dryNote annotates a plan line when nothing is actually written.
func dryNote(dry bool) string {
	if dry {
		return " — dry-run, skipped"
	}
	return ""
}

// miseConfigDir is where the running shell reads mise config — $MISE_CONFIG_DIR, else the
// XDG default ~/.config/mise (per the mise-dir split: config stays at XDG, only DATA/
// CACHE/STATE redirect).
func miseConfigDir() string {
	if d := os.Getenv("MISE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "mise")
}

// stageMiseConfigs writes the embedded tier tree into dir (creating conf.d/), the emit
// step: mu is the source of truth, these are the generated copies the shell + install read.
func stageMiseConfigs(dir string) error {
	files, err := setup.MiseFiles()
	if err != nil {
		return err
	}
	for rel, content := range files {
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// tierModules are MU_MODULES entries that each map to a same-named mise tier
// (config.<m>.toml): personal, opt-in tool sets layered on base + hpc. fmt = formatters/lint;
// cast = the recording toolchain. Keep in sync with the shellinit runtime MISE_ENV composition.
var tierModules = []string{"fmt", "cast"}

// miseEnv composes MISE_ENV for the install: hpc always (the nvim/CLI stack this command
// installs; base + conf.d are unconditional), plus each opted-in tier module from MU_MODULES.
func miseEnv() string {
	env := "hpc"
	for _, m := range tierModules {
		if modules.Enabled(m) {
			env += "," + m
		}
	}
	return env
}

// deployModule installs the shared HPC runtime into --prefix and writes the Tcl
// modulefile. The embedded tiers are staged into a temp dir as local mise config so one
// config-resolved `mise install` covers base + hpc in a single resolution; MISE_ENV is
// pinned to hpc (fmt is personal, never shared) and the cache rides under the prefix so a
// small $HOME quota never blocks the deploy.
func (t *toolchain) deployModule(mise string) error {
	dir, err := os.MkdirTemp("", "mu-toolchain-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := stageMiseConfigs(dir); err != nil {
		return fmt.Errorf("stage mise config: %w", err)
	}
	env := overrideEnv(os.Environ(),
		"MISE_DATA_DIR="+t.prefix,
		"MISE_CACHE_DIR="+filepath.Join(t.prefix, "cache"),
		"MISE_ENV=hpc")
	if err := runEnvDir(dir, env, mise, "trust", dir); err != nil {
		return fmt.Errorf("mise trust: %w", err)
	}
	if err := runEnvDir(dir, env, mise, "install"); err != nil {
		return fmt.Errorf("mise install: %w", err)
	}
	out, err := outputEnvDir(dir, env, mise, "bin-paths")
	if err != nil {
		return fmt.Errorf("mise bin-paths: %w", err)
	}
	var bins []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !isBackendRuntime(l) {
			bins = append(bins, l)
		}
	}
	if len(bins) == 0 {
		return fmt.Errorf("mise bin-paths reported no tool dirs under %s", t.prefix)
	}
	render.OK(fmt.Sprintf("installed shared toolchain: %d tool dirs under %s", len(bins), t.prefix))
	return t.writeModulefile(bins)
}

// darwin emits the mise tiers (so the shell resolves them) and prints the Homebrew + mise
// bootstrap (some tools are brew-only on Intel macs, so mise can't cover everything) —
// running the brew step only with --with-brew. Never drives brew silently.
func (t *toolchain) darwin() error {
	const brew = "brew install mise git-delta difftastic"
	const activate = `eval "$(mise activate zsh)"`
	cfgDir := miseConfigDir()
	if !t.withBrew || t.dryRun {
		render.Info("macOS toolchain bootstrap:")
		render.Detail("  config: " + cfgDir + " (emitted from the embedded tiers" + dryNote(t.dryRun) + ")")
		render.Detail("  " + brew + "   # run yourself")
		render.Detail("  " + activate + "   # add to ~/.zshrc")
		if !t.dryRun {
			if err := stageMiseConfigs(cfgDir); err != nil {
				return fmt.Errorf("emit mise config: %w", err)
			}
		}
		if !t.withBrew {
			render.Info("re-run with --with-brew to execute the brew step")
		}
		return nil
	}
	if err := stageMiseConfigs(cfgDir); err != nil {
		return fmt.Errorf("emit mise config: %w", err)
	}
	if err := runEnv(os.Environ(), "brew", "install", "mise", "git-delta", "difftastic"); err != nil {
		return fmt.Errorf("brew install: %w", err)
	}
	render.OK("brew tools installed — add to ~/.zshrc: " + activate)
	return nil
}

// isBackendRuntime filters install dirs that only back other tools (python → pipx
// venvs) out of the modulefile PATH: venv shebangs reference them by absolute prefix
// path, and HPC sites provide user-facing pythons via their own modules — ours must
// not shadow those on a consumer's PATH.
func isBackendRuntime(binDir string) bool {
	return strings.Contains(binDir, "/installs/python/")
}

// modulefilePath is where --module writes the Tcl modulefile under the shared prefix.
func (t *toolchain) modulefilePath() string {
	return filepath.Join(t.prefix, "modulefiles", "mu-toolchain")
}

// writeModulefile emits the Tcl modulefile: one static prepend-path per tool bin dir —
// consumers never invoke mise (shims need a per-user config to resolve, and would point
// at the deployer's home-dir mise binary), so a config-less user and a read-only prefix
// both work. `module load mu-toolchain` after `module use <prefix>/modulefiles`.
func (t *toolchain) writeModulefile(binDirs []string) error {
	path := t.modulefilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("#%Module1.0\n")
	b.WriteString("## mu dev toolchain — generated by `mu setup toolchain --module`\n")
	for _, d := range binDirs {
		b.WriteString("prepend-path PATH " + d + "\n")
	}
	// The module-provided marker: the zsh MISE_ENV composition skips the hpc tier
	// when set, so a per-user `fmt` opt-in never re-installs module-owned base+hpc.
	// Doubles as the doctor's provider signal (module vs personal mise).
	b.WriteString("setenv MU_TOOLCHAIN " + t.prefix + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return err
	}
	render.OK("wrote modulefile: " + path + "  (module use " + filepath.Dir(path) + ")")
	render.Info("consumers need read access — check the prefix: chmod -R a+rX " + t.prefix)
	return nil
}

// misePath finds mise on PATH, or at the ~/.local/bin/mise the bootstrap installs to.
// Returns the fallback name "mise" and false when neither exists.
func misePath() (string, bool) {
	if p, err := exec.LookPath("mise"); err == nil {
		return p, true
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "bin", "mise")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "mise", false
}

// bootstrapMise runs the official `curl https://mise.run | sh` installer, its chatter
// routed to stderr so mu's stdout stays clean.
func bootstrapMise() error {
	render.Info("Bootstrapping mise (curl https://mise.run | sh)…")
	curl := exec.Command("curl", "-fsSL", "https://mise.run")
	sh := exec.Command("sh")
	pipe, err := curl.StdoutPipe()
	if err != nil {
		return err
	}
	sh.Stdin = pipe
	sh.Stdout, sh.Stderr = os.Stderr, os.Stderr
	if err := sh.Start(); err != nil {
		return err
	}
	if err := curl.Run(); err != nil {
		return fmt.Errorf("curl mise.run: %w", err)
	}
	if err := sh.Wait(); err != nil {
		return fmt.Errorf("mise install script: %w", err)
	}
	return nil
}

// runEnv runs a command with the given env, streaming its output to stderr.
func runEnv(env []string, name string, args ...string) error {
	return runEnvDir("", env, name, args...)
}

// runEnvDir is runEnv with a working directory (mise resolves a staged local config
// by cwd).
func runEnvDir(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

// outputEnvDir runs a command and captures stdout (chatter still streams to stderr).
func outputEnvDir(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return string(out), err
}

// overrideEnv replaces (not appends) the given KEY=value entries in env — a duplicate
// key appended after the original is undefined territory for getenv.
func overrideEnv(env []string, kv ...string) []string {
	out := env[:0:0]
	for _, e := range env {
		keep := true
		for _, o := range kv {
			if k, _, ok := strings.Cut(o, "="); ok && strings.HasPrefix(e, k+"=") {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, e)
		}
	}
	return append(out, kv...)
}
