package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// The .config repo is the git-transport payload of `mu setup sync --dotfiles`, dispatched
// because it IS a git repo (config.toml, which isn't, takes the text merge). push reuses
// onboard's leak-safe bundle → box reset; pull is a fetch + backup ref + auto fast-forward
// / merge. config.toml, the sshfs registry, and secrets live outside git and are untouched.

// pushDotfiles pushes this machine's tracked .config to the box for `sync --dotfiles`.
func pushDotfiles(target, configDir string, force bool) error {
	return pushConfigBundle(target, configDir, force, false)
}

// pushConfigBundle seeds configDir's tracked HEAD onto target as a real git checkout at
// ~/.config, via a leak-safe bundle (committed files only — untracked secrets never leave)
// overlaid with reset --hard. Shared primitive of `mu setup onboard` (birth) and `mu setup
// sync --dotfiles` (maintain); neither owns it. dryRun prints the steps without running them.
func pushConfigBundle(target, configDir string, force, dryRun bool) error {
	branch := gitField(configDir, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		branch = "main"
	}
	origin := repoHTTPS(gitField(configDir, "config", "--get", "remote.origin.url"))
	render.Info(fmt.Sprintf("Seeding tracked .config → %s:~/.config (git checkout from %s)…", target, branch))
	if dryRun {
		render.Detail(fmt.Sprintf("[dry] git bundle HEAD → scp → git init+fetch+reset on %s (origin %s)", target, origin))
		return nil
	}
	if out, err := exec.Command("ssh", "-q", target, "command -v git >/dev/null").CombinedOutput(); err != nil {
		return fmt.Errorf("target has no git to seed .config as a repo (or re-run with --config=false): %w\n%s", err, out)
	}
	// Bundle local HEAD (committed history only) and ship it — a single leak-safe file.
	f, err := os.CreateTemp("", "mu-config-*.bundle")
	if err != nil {
		return err
	}
	_ = f.Close()
	defer func() { _ = os.Remove(f.Name()) }()
	if out, err := exec.Command("git", "-C", configDir, "bundle", "create", f.Name(), "HEAD").CombinedOutput(); err != nil {
		return fmt.Errorf("git bundle: %w\n%s", err, out)
	}
	const remoteBundle = "~/.mu-config.bundle"
	if err := exec.Command("scp", "-q", f.Name(), target+":"+remoteBundle).Run(); err != nil {
		return fmt.Errorf("scp bundle: %w", err)
	}
	return seedConfigRepo(target, branch, origin, remoteBundle, force)
}

// seedConfigRepo runs the box-side reconciliation as one atomic script: init, fetch the
// bundle, and reset --hard to the laptop's HEAD — but ONLY when the existing checkout is
// clean and fast-forwardable (or absent). A box with uncommitted tracked changes or diverged
// commits aborts (exit 3) instead of clobbering, unless force first saves that work to branch
// mu-onboard-backup + a git stash. Then it points origin at the public remote for `git pull`.
func seedConfigRepo(target, branch, origin, bundle string, force bool) error {
	forceFlag := "0"
	if force {
		forceFlag = "1"
	}
	// set -e is safe here: every failing command is the tested side of an && / || .
	script := fmt.Sprintf(`set -e
mkdir -p ~/.config && cd ~/.config
fresh=0; git rev-parse -q --verify HEAD >/dev/null 2>&1 || fresh=1
git init -q
[ "$fresh" = 1 ] && git symbolic-ref HEAD refs/heads/%[1]s
git fetch -q %[2]s HEAD
if [ "$fresh" = 0 ]; then
  dirty=$(git status --porcelain --untracked-files=no)
  git merge-base --is-ancestor HEAD FETCH_HEAD 2>/dev/null && ff=1 || ff=0
  if [ -n "$dirty" ] || [ "$ff" = 0 ]; then
    if [ "%[3]s" != 1 ]; then rm -f %[2]s; echo MU_DIRTY_ABORT; exit 3; fi
    git branch -f mu-onboard-backup HEAD
    git stash push -q -m "mu-onboard backup" || true
  fi
fi
git reset --hard -q FETCH_HEAD
git remote get-url origin >/dev/null 2>&1 || git remote add origin %[4]s
rm -f %[2]s
`, shell.Quote(branch), bundle, forceFlag, shell.Quote(origin))
	out, err := exec.Command("ssh", "-q", target, script).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "MU_DIRTY_ABORT") {
			render.Warn(".config on the target has local changes or diverged commits — not overwriting.")
			render.Info("skipped .config sync; reconcile on the box, or re-run with --force (saves the work to branch mu-onboard-backup + a git stash first)")
			return nil
		}
		return fmt.Errorf("seed .config: %w\n%s", err, out)
	}
	render.OK(".config is a live git repo on the target (git pull to update)")
	return nil
}

// pullDotfiles reconciles the box's .config INTO this machine's .config over git: fetch the
// box's HEAD, snapshot the current state to branch mu-sync-backup (+ stash any dirty work),
// then auto fast-forward / merge. On conflict it aborts the merge and restores, leaving the
// backup ref for a hand-merge. The fetch runs laptop-side (ssh out), matching the egress
// asymmetry — the box never needs to reach the network.
func pullDotfiles(target, configDir string, yes bool) error {
	git := func(args ...string) (string, error) {
		c := exec.Command("git", append([]string{"-C", configDir}, args...)...)
		c.Env = append(os.Environ(), "GIT_SSH_COMMAND=ssh -q") // quiet the box's login banner on fetch
		out, err := c.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := git("rev-parse", "--git-dir"); err != nil {
		return runErr("%s is not a git repo — .config reconcile needs a checkout", configDir)
	}
	fetchURL := target + ":.config" // git scp-syntax; path is relative to the box's home
	render.Info("Fetching .config ← " + target + " …")
	if out, err := git("fetch", "-q", fetchURL, "HEAD"); err != nil {
		return fmt.Errorf("fetch .config from %s: %w\n%s", target, err, out)
	}
	// Already have the box's HEAD (it's an ancestor of ours)? Nothing to do.
	if _, err := git("merge-base", "--is-ancestor", "FETCH_HEAD", "HEAD"); err == nil {
		render.OK(".config already up to date with " + target)
		return nil
	}
	if in, _ := git("log", "--oneline", "HEAD..FETCH_HEAD"); in != "" {
		render.Detail("incoming .config commits from " + target + ":")
		fmt.Fprintln(os.Stderr, in)
	}
	if !yes {
		if !confirm("merge %s's .config into %s?", target, configDir) {
			render.Info("aborted .config")
			return nil
		}
	}
	// Backup ref before touching the tree — the git analog of config.toml.bak.
	if out, err := git("branch", "-f", "mu-sync-backup", "HEAD"); err != nil {
		return fmt.Errorf("create backup branch: %w\n%s", err, out)
	}
	stashed := false
	if st, _ := git("status", "--porcelain"); st != "" {
		if _, err := git("stash", "push", "-u", "-q", "-m", "mu sync pull backup"); err == nil {
			stashed = true
		}
	}
	// Pure fast-forward when our HEAD is an ancestor of the box's; else a real merge.
	ff := false
	if _, err := git("merge-base", "--is-ancestor", "HEAD", "FETCH_HEAD"); err == nil {
		ff = true
	}
	if out, err := git("merge", "--no-edit", "-q", "FETCH_HEAD"); err != nil {
		_, _ = git("merge", "--abort")
		if stashed {
			_, _ = git("stash", "pop", "-q") // tree is back at HEAD, so this restores cleanly
		}
		// Exit 4 is a distinct "reconcile hit conflicts" signal (vs the 1/2 tiers in exit.go);
		// the by-hand steps + conflict output tail dim below the headline via logLine's indent.
		return &exitErr{code: 4, msg: "auto-merge hit conflicts — .config left unchanged (pre-merge state at branch mu-sync-backup)\n" +
			"reconcile by hand: cd " + configDir + " && git merge FETCH_HEAD\n" + strings.TrimSpace(out)}
	}
	kind := "merged"
	if ff {
		kind = "fast-forwarded"
	}
	msg := ".config " + kind + " ← " + target + " (pre-merge state at branch mu-sync-backup)"
	render.OK(msg)
	render.EventOK("setup", msg)
	if stashed {
		render.Warn("your local .config changes were stashed — `git -C " + configDir + " stash pop` to restore")
	}
	return nil
}
