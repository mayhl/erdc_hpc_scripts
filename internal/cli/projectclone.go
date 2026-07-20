package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/project"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/shell"
)

// projectCloneCmd is `mu project clone`: bootstrap a per-node clone of the
// project. Git stays local-by-default — the laptop repo is the origin of truth
// and each HPC clone is a plain ssh git remote at the same $HOME-relative path;
// a hosted origin is just one more remote, pushed by hand.
func projectCloneCmd() *cobra.Command {
	var yes, dryRun bool
	c := &cobra.Command{
		Use:   "clone <node> [path]",
		Short: "Bootstrap a project clone on a cluster as a per-node git remote.",
		Long: "Initialize ~/<project-rel> on the node as a git repo with\n" +
			"receive.denyCurrentBranch=updateInstead (a push updates the checked-out tree\n" +
			"iff clean), add it locally as remote <node>, and make the first push. After\n" +
			"this, clean-mode submits and hook discovery find the checkout on the cluster.\n" +
			"Idempotent: an existing remote repo is left as-is (config re-asserted), an\n" +
			"existing local remote gets its URL verified.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			path := "."
			if len(args) == 2 {
				path = args[1]
			}
			return projectClone(args[0], path, yes, dryRun)
		},
	}
	setHelpArgs(c,
		[2]string{"<node>", "cluster to host the clone (becomes the git remote name)"},
		[2]string{"[path]", "a path inside the project (default: the current directory)"})
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	c.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "show the plan without touching anything")
	completeNodeFlag(c)
	return c
}

// projectClone is the bootstrap pipeline: resolve → preview+confirm → remote
// init (leg 1) → local remote add (leg 2) → first push (leg 3) → verify (leg 4).
func projectClone(node, path string, yes, dryRun bool) error {
	root, err := project.FindRoot(path)
	if err != nil {
		return usageErr("%s", err)
	}
	rel, err := project.HomeRel(root)
	if err != nil {
		return usageErr("%s", err)
	}
	branch := gitLine(root, "branch", "--show-current")
	if branch == "" {
		return usageErr("%s is on a detached HEAD — check out a branch to clone from", root)
	}
	head := gitLine(root, "rev-parse", "HEAD")
	if head == "" {
		return usageErr("%s has no commits yet — nothing to push", root)
	}
	target, err := hpc.Resolve(node)
	if err != nil {
		return usageErr("%s", err)
	}
	url := target + ":" + rel

	render.Info(fmt.Sprintf("Clone project → %s", node))
	render.Detail("project: " + rel)
	render.Detail("branch:  " + branch + " @ " + head[:12])
	render.Detail("remote:  " + node + " → " + url)
	if dryRun {
		render.Info("dry run — nothing touched")
		return nil
	}
	if !yes {
		fmt.Fprintf(os.Stderr, "init + push to %s? [y/N] ", node)
		var r string
		_, _ = fmt.Scanln(&r)
		if strings.ToLower(strings.TrimSpace(r)) != "y" {
			render.Info("aborted")
			return nil
		}
	}
	if err := hpc.EnsureTicket(); err != nil {
		return runErr("%s", err)
	}

	// Leg 1: init the remote repo (idempotent — an existing repo keeps its
	// history and branch; only the receive config is re-asserted). A fresh init
	// pins HEAD to the local branch so updateInstead targets what we push.
	qrel, qref := shell.Quote(rel), shell.Quote("refs/heads/"+branch)
	script := fmt.Sprintf(`mkdir -p "$HOME"/%[1]s && cd "$HOME"/%[1]s && `+
		`if [ ! -e .git ]; then git init -q && git symbolic-ref HEAD %[2]s; fi && `+
		`git config receive.denyCurrentBranch updateInstead && echo BOOTSTRAP_OK`, qrel, qref)
	out, err := hpc.RemoteExec(target, script)
	if err != nil {
		return runErr("%s: remote init: %s", node, err)
	}
	if !strings.Contains(out, "BOOTSTRAP_OK") {
		return runErr("%s: remote init did not complete:\n%s", node, strings.TrimSpace(out))
	}

	// Leg 2: register the local remote (or verify/repoint an existing one).
	switch existing := gitLine(root, "remote", "get-url", node); existing {
	case "":
		if msg, ok := gitRun(root, nil, "remote", "add", node, url); !ok {
			return runErr("git remote add: %s", msg)
		}
	case url:
	default:
		render.Warn("remote " + node + " pointed at " + existing + " — repointing")
		if msg, ok := gitRun(root, nil, "remote", "set-url", node, url); !ok {
			return runErr("git remote set-url: %s", msg)
		}
	}

	// Leg 3: first push, over the same ssh seam the rest of mu uses (Kerberos
	// wrapper and all) — git's own ssh choice must not diverge from mu's.
	env := append(os.Environ(), "GIT_SSH_COMMAND="+config.SSHCommand())
	if msg, ok := gitRun(root, env, "push", "-q", node, branch); !ok {
		return runErr("push to %s failed: %s", node, msg)
	}

	// Leg 4: updateInstead only refreshes a CLEAN checked-out tree — trust it,
	// then verify the tree really landed on our commit.
	out, err = hpc.RemoteExec(target, fmt.Sprintf(`cd "$HOME"/%s && git rev-parse HEAD`, qrel))
	if err != nil {
		return runErr("%s: verify: %s", node, err)
	}
	if got := strings.TrimSpace(out); got != head {
		return runErr("%s: checkout is at %.12s, expected %.12s — reconcile the remote tree", node, got, head)
	}
	msg := "clone ready: " + node + ":" + rel + " @ " + head[:12]
	render.OK(msg)
	render.EventOK("project", msg)
	return nil
}

// gitLine runs git in dir and returns its first stdout line ("" on error) — for
// read-only queries where absence is a normal answer.
func gitLine(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

// gitRun runs a mutating git command, returning its combined output and success.
func gitRun(dir string, env []string, args ...string) (string, bool) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}
