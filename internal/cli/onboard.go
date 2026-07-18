package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// onboardCmd is `mu setup onboard <node>`: seed a fresh Linux/HPC box with mu + your
// tracked .config, driven from an already-set-up machine. The Go port of the Track-A
// onboard.sh prototype — phase 1 of machine onboarding (push), decoupled from the
// slower phase-2 `mu setup toolchain` (mise install, runs on the box). Idempotent:
// safe to re-run. config.toml is NEVER copied (it's machine-specific identity) — it's
// seeded from the example and left for you to fill in.
func onboardCmd() *cobra.Command {
	o := onboard{
		goarch:    "amd64",
		bin:       "~/.local/bin/mu",
		muRoot:    "~/.config/mu",
		shellKind: "zsh",
		doConfig:  true,
	}
	c := &cobra.Command{
		Use:   "onboard <node|user@host>",
		Short: "Set up a fresh box with mu + your .config (push from this machine).",
		Long: "Seed a fresh Linux/HPC box from this one: cross-build a linux mu, push it and\n" +
			"your tracked .config, seed a config.toml, and print the shell wiring. Runs over\n" +
			"your own ssh session and only touches the node you name. config.toml is never\n" +
			"copied — it holds this box's identity, so it's seeded from the example for you\n" +
			"to fill in. Only committed .config files are sent (untracked secrets never leave).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if o.repo == "" {
				o.repo = defaultRepo()
			}
			if o.configDir == "" {
				o.configDir = filepath.Join(os.Getenv("HOME"), ".config")
			}
			return o.run(args[0])
		},
	}
	setHelpArgs(c,
		[2]string{"<node|user@host>", "configured node alias, or a raw ssh target for a not-yet-configured box"})
	f := c.Flags()
	f.StringVar(&o.repo, "repo", "", "mayhl_utils checkout to cross-build from (default $MU_ROOT or ~/repos/mayhl_utils)")
	f.StringVar(&o.configDir, "config-dir", "", "the .config git repo to push (default ~/.config)")
	f.StringVar(&o.goarch, "goarch", o.goarch, "target CPU arch for the mu cross-build")
	f.StringVar(&o.bin, "bin", o.bin, "where mu lands on the target")
	f.StringVar(&o.muRoot, "mu-root", o.muRoot, "target MU_ROOT (holds config.toml)")
	f.StringVar(&o.shellKind, "shell", o.shellKind, "target login shell for the rc snippet (bash|zsh|fish)")
	f.BoolVar(&o.doConfig, "config", o.doConfig, "push tracked .config (--config=false to skip)")
	f.BoolVar(&o.dryRun, "dry-run", false, "print each mutating step without running it")
	f.BoolVar(&o.force, "force", false, "overwrite the target's .config even if it has local changes (backs them up first)")
	_ = c.RegisterFlagCompletionFunc("shell", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"bash", "zsh", "fish"}, cobra.ShellCompDirectiveNoFileComp
	})
	return c
}

type onboard struct {
	repo, configDir     string
	goarch, bin, muRoot string
	shellKind           string
	doConfig, dryRun    bool
	force               bool
}

func (o *onboard) run(nodeOrTarget string) error {
	target, err := hpc.Resolve(nodeOrTarget)
	if err != nil {
		return usageErr("%s", err)
	}
	// Preflight (local): a real ssh target, a toolchain to cross-build with, and a
	// .config git repo whose whitelist keeps secrets out of the push.
	if strings.Contains(target, " ") {
		return usageErr("target has a space — pass a node name or a real user@host")
	}
	if _, err := exec.LookPath("go"); err != nil {
		return runErr("go not on PATH (needed to cross-build mu)")
	}
	if !isDir(filepath.Join(o.repo, "cmd", "mu")) {
		return runErr("no cmd/mu under --repo %s", o.repo)
	}
	if err := exec.Command("git", "-C", o.configDir, "rev-parse", "--git-dir").Run(); err != nil {
		return runErr("--config-dir %s is not a git repo", o.configDir)
	}

	tag := ""
	if o.dryRun {
		tag = "   [DRY RUN]"
	}
	render.Info(fmt.Sprintf("onboard → %s   (mu + .config, linux/%s)%s", target, o.goarch, tag))

	// Connectivity (real ssh; skipped under --dry-run).
	if !o.dryRun {
		if err := exec.Command("ssh", "-q", "-o", "ConnectTimeout=15", target, "true").Run(); err != nil {
			return runErr("cannot ssh to %s (auth/PKI? host? must be a real ssh target)", target)
		}
		render.OK("ssh ok")
	}

	// Cross-build mu (always, even in dry-run — it validates the tree compiles).
	tmp, err := os.MkdirTemp("", "mu-onboard")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	localMu := filepath.Join(tmp, "mu")
	render.Info(fmt.Sprintf("Cross-building mu (linux/%s) from %s…", o.goarch, o.repo))
	// Stamp the pushed binary with `git describe` so the box's `mu --version` and
	// `mu doctor setup` report what's deployed (matches the Makefile's version seam).
	args := []string{"build", "-o", localMu}
	if v := gitField(o.repo, "describe", "--tags", "--always", "--dirty"); v != "" {
		args = append(args, "-ldflags", "-s -w -X github.com/mayhl/mayhl_utils/internal/cli.version="+v)
	}
	build := exec.Command("go", append(args, "./cmd/mu")...)
	build.Dir = o.repo
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+o.goarch)
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("cross-build mu: %w\n%s", err, out)
	}
	render.OK("built linux binary")

	// Push mu + verify it runs on the target.
	binDir := posixDir(o.bin)
	if err := o.ssh(target, "mkdir -p "+binDir); err != nil {
		return err
	}
	if o.dryRun {
		render.Detail(fmt.Sprintf("[dry] scp %s %s:%s", localMu, target, o.bin))
	} else {
		render.Info(fmt.Sprintf("Pushing mu → %s:%s…", target, o.bin))
		if err := o.push(target, localMu, o.bin); err != nil {
			return err
		}
	}
	if err := o.ssh(target, "chmod +x "+o.bin); err != nil {
		return err
	}
	if !o.dryRun {
		if out, err := exec.Command("ssh", "-q", target, o.bin+" --version").CombinedOutput(); err != nil {
			render.Warn("mu --version failed on target: " + strings.TrimSpace(string(out)))
		} else {
			render.Detail("target mu: " + strings.TrimSpace(string(out)))
		}
	}

	// Push tracked .config — git archive HEAD is the whitelist: committed files only,
	// no .git, no untracked secrets. This is the default-deny push (leak-safe by design).
	if o.doConfig {
		if err := pushConfigBundle(target, o.configDir, o.force, o.dryRun); err != nil {
			return err
		}
	}

	// Seed config.toml from the example (never clobber an existing one).
	if err := o.seedConfig(target); err != nil {
		return err
	}

	o.printNextSteps(target)
	render.OK("onboard " + dryLabel(o.dryRun) + "complete")
	return nil
}

// gitField runs a git query in dir and returns its trimmed output ("" on error).
func gitField(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// repoHTTPS rewrites a GitHub ssh remote (git@github.com:owner/repo.git) to its public
// https form, so a fresh box — which won't have your ssh key — can `git pull` a public
// repo. Non-github or already-https URLs pass through unchanged.
func repoHTTPS(url string) string {
	const p = "git@github.com:"
	if strings.HasPrefix(url, p) {
		return "https://github.com/" + strings.TrimPrefix(url, p)
	}
	return url
}

// seedConfig copies config.toml.example → target:<mu-root>/config.toml, only when the
// target has none — never overwrites a box's filled-in identity.
func (o *onboard) seedConfig(target string) error {
	ex := filepath.Join(o.repo, "config.toml.example")
	if !isFile(ex) {
		render.Warn("no config.toml.example in " + o.repo + " — skipping config seed")
		return nil
	}
	render.Info(fmt.Sprintf("Seeding %s/config.toml (from config.toml.example)…", o.muRoot))
	if err := o.ssh(target, "mkdir -p "+o.muRoot); err != nil {
		return err
	}
	dst := o.muRoot + "/config.toml"
	if o.dryRun {
		render.Detail(fmt.Sprintf("[dry] scp %s %s:%s (if absent)", ex, target, dst))
		return nil
	}
	if exec.Command("ssh", target, "test -f "+dst).Run() == nil {
		render.Warn("config.toml already on target — left untouched")
		return nil
	}
	if err := o.push(target, ex, dst); err != nil {
		return err
	}
	render.OK("config.toml seeded — fill in this cluster's identity")
	return nil
}

func (o *onboard) printNextSteps(target string) {
	binHome := "$HOME/" + strings.TrimPrefix(posixDir(o.bin), "~/")
	rootHome := "$HOME/" + strings.TrimPrefix(o.muRoot, "~/")
	fmt.Fprintf(os.Stderr, `
Next steps on %s:

  1. Add to ~/.%src:

       export PATH="%s:$PATH"
       export MU_ROOT="%s"
       eval "$(mu setup --eval %s)"

  2. Edit %s/config.toml — set hpc_user and a [[cluster]] block
     (domain, nodes, scheduler) for THIS system.

  3. Open a fresh shell, then check:
       mu --version
       mu hpc queue        # once config.toml has a cluster

  4. First user on this box? Install the toolchain (phase 2, runs on the box):
       mu setup toolchain --prefix <shared-path> --module
`, target, o.shellKind, binHome, rootHome, o.shellKind, rootHome)
}

// ssh runs a mutating remote command, echoing it first; under --dry-run it only echoes.
// push copies local → target:remote over ssh, writing a temp file then `mv`-ing it over
// the target. Two reasons not to scp straight onto remote: (1) modern scp rides the SFTP
// subsystem, which DoD/HPC servers often disable (scp dies with a bare exit 1), whereas a
// plain ssh command — which we already used for mkdir — works; (2) re-onboarding a box
// whose mu is running can't truncate the binary in place ("text file busy"), but a rename
// replaces the name while the live process keeps the old inode. Captures the remote's
// stderr (scp -q swallowed it); ~ expands in the login shell.
func (o *onboard) push(target, local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	tmp := remote + ".mu-onboard.tmp"
	cmd := exec.Command("ssh", "-q", target, "cat > "+tmp+" && mv -f "+tmp+" "+remote)
	cmd.Stdin = f
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("push %s → %s: %w: %s", filepath.Base(local), remote, err, msg)
		}
		return fmt.Errorf("push %s → %s: %w", filepath.Base(local), remote, err)
	}
	return nil
}

func (o *onboard) ssh(target, remote string) error {
	if o.dryRun {
		render.Detail("[dry] ssh " + target + " " + remote)
		return nil
	}
	cmd := exec.Command("ssh", "-q", target, remote)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh %s: %q: %w", target, remote, err)
	}
	return nil
}

// defaultRepo is the mayhl_utils checkout to cross-build from: $MU_ROOT, else the
// conventional ~/repos/mayhl_utils.
func defaultRepo() string {
	if r := os.Getenv("MU_ROOT"); r != "" {
		return r
	}
	return filepath.Join(os.Getenv("HOME"), "repos", "mayhl_utils")
}

// posixDir is filepath.Dir but always with forward slashes — these paths run on the
// remote POSIX box, not the local (possibly Windows) filesystem.
func posixDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}

func dryLabel(dry bool) string {
	if dry {
		return "dry-run "
	}
	return ""
}

func isDir(p string) bool  { fi, err := os.Stat(p); return err == nil && fi.IsDir() }
func isFile(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }
