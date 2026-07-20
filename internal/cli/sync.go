package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/tomledit"
)

// syncOpts carries the resolved flags for a sync run in either direction.
type syncOpts struct {
	muRoot    string
	configDir string // the .config git repo, for --dotfiles
	yes       bool
	pull      bool
	dotfiles  bool
	force     bool // push --dotfiles: overwrite the box's .config even if diverged
}

// localSeams are the tables that never travel between machines: the laptop's ossh path is
// meaningless on a cluster, and sshfs mounts are a local-only plane. Everything else in
// config.toml is shared fleet inventory (see ARCHITECTURE.md "Sync merge strategy"). The
// mount registry and secrets live in other files and are never touched at all.
var localSeams = []string{"ssh", "sshfs"}

// defaultConfigDir is the .config git repo synced by --dotfiles (default ~/.config).
func defaultConfigDir() string { return filepath.Join(os.Getenv("HOME"), ".config") }

// syncCmd is `mu setup sync <node>`: the machine-lifecycle MAINTAIN step. It propagates
// this machine's shared inventory (config.toml — hpc_user, [[cluster]] defs, fleet, prefs)
// to a target, while KEEPING the target's machine-local [ssh]/[sshfs] seams. The sshfs
// mount registry and any secrets live in other files and are never touched. Diff + confirm
// before writing. With --dotfiles it also syncs the .config git repo (git transport, since
// it's a repo). `sync pull` reconciles the other direction (box → this machine). Reuses
// onboard's ssh plumbing; skips the birth steps.
func syncCmd() *cobra.Command {
	o := syncOpts{muRoot: "~/.config/mu"}
	c := &cobra.Command{
		Use:   "sync <node|user@host>",
		Short: "Push this machine's config.toml (clusters/fleet) to another box.",
		Long: "Propagate this machine's shared inventory — hpc_user, [[cluster]] defs, fleet,\n" +
			"prefs — to a target's config.toml, keeping the target's machine-local [ssh] and\n" +
			"[sshfs] settings. The sshfs mount registry and secrets are never touched. Shows a\n" +
			"diff and confirms before writing. --dotfiles also pushes the .config git repo (via\n" +
			"git bundle). Use `sync pull` to reconcile box → this machine.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if o.configDir == "" {
				o.configDir = defaultConfigDir()
			}
			return runSync(args[0], o)
		},
	}
	setHelpArgs(c, [2]string{"<node|user@host>", "box to update — configured node alias, or a raw ssh target"})
	f := c.Flags()
	f.StringVar(&o.muRoot, "mu-root", o.muRoot, "target MU_ROOT (holds config.toml)")
	f.BoolVarP(&o.yes, "yes", "y", false, "skip the confirmation prompt")
	f.BoolVar(&o.dotfiles, "dotfiles", false, "also sync the .config git repo (git transport)")
	f.StringVar(&o.configDir, "config-dir", "", "the .config git repo for --dotfiles (default ~/.config)")
	f.BoolVar(&o.force, "force", false, "with --dotfiles, overwrite the box's .config even if diverged (backs it up first)")
	_ = c.RegisterFlagCompletionFunc("mu-root", noFileComp)
	c.AddCommand(syncPullCmd())
	return c
}

// syncPullCmd is `mu setup sync pull <node>`: the mirror of sync. It brings a target's
// shared inventory INTO this machine's config.toml, keeping THIS machine's [ssh]/[sshfs]
// seams. Because it overwrites the real local config.toml, it backs up config.toml.bak
// first, on top of the diff + confirm. --dotfiles also reconciles the .config git repo
// (fetch + backup ref + auto fast-forward/merge).
func syncPullCmd() *cobra.Command {
	o := syncOpts{muRoot: "~/.config/mu", pull: true}
	c := &cobra.Command{
		Use:   "pull <node|user@host>",
		Short: "Pull another box's config.toml (clusters/fleet) into this machine.",
		Long: "Reverse of `mu setup sync`: bring a target's shared inventory — hpc_user,\n" +
			"[[cluster]] defs, fleet, prefs — into this machine's config.toml, keeping THIS\n" +
			"machine's [ssh]/[sshfs] seams. Backs up the local config.toml to config.toml.bak,\n" +
			"shows a diff, and confirms before writing. --dotfiles also reconciles the .config\n" +
			"git repo: fetch the box, snapshot to branch mu-sync-backup, then auto-merge.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if o.configDir == "" {
				o.configDir = defaultConfigDir()
			}
			return runSync(args[0], o)
		},
	}
	setHelpArgs(c, [2]string{"<node|user@host>", "box to import from — configured node alias, or a raw ssh target"})
	f := c.Flags()
	f.StringVar(&o.muRoot, "mu-root", o.muRoot, "target MU_ROOT (holds config.toml)")
	f.BoolVarP(&o.yes, "yes", "y", false, "skip the confirmation prompt")
	f.BoolVar(&o.dotfiles, "dotfiles", false, "also reconcile the .config git repo (fetch + auto-merge)")
	f.StringVar(&o.configDir, "config-dir", "", "the .config git repo for --dotfiles (default ~/.config)")
	_ = c.RegisterFlagCompletionFunc("mu-root", noFileComp)
	return c
}

func noFileComp(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// runSync syncs a machine's shared state to/from a target. The config.toml payload always
// syncs (text merge). With --dotfiles the .config repo also syncs, dispatched by transport:
// it's a git repo, so git (bundle push / fetch+merge pull) — the git analog of the text merge.
func runSync(nodeOrTarget string, o syncOpts) error {
	target, err := hpc.Resolve(nodeOrTarget)
	if err != nil {
		return usageErr("%s", err)
	}
	if err := syncConfigTOML(target, o); err != nil {
		return err
	}
	if o.dotfiles {
		if o.pull {
			return pullDotfiles(target, o.configDir, o.yes)
		}
		return pushDotfiles(target, o.configDir, o.force)
	}
	return nil
}

// syncConfigTOML merges the config.toml payload between this machine and target. Inventory
// flows from the SOURCE; the DESTINATION's [ssh]/[sshfs] seams are preserved. push (default):
// source = local, dest = remote box. pull: source = remote box, dest = local — so it also
// backs up and atomically rewrites the real local config.toml. A user abort or an
// already-in-sync state returns nil (so a --dotfiles run still proceeds to the repo payload).
func syncConfigTOML(target string, o syncOpts) error {
	localPath := config.Path()
	if localPath == "" {
		return runErr("no local config.toml (set MU_CONFIG_FILE or MU_ROOT)")
	}
	localBytes, rerr := os.ReadFile(localPath)
	if rerr != nil && (!o.pull || !os.IsNotExist(rerr)) { // pull tolerates a missing local (fresh)
		return fmt.Errorf("read %s: %w", localPath, rerr)
	}
	localText := string(localBytes)
	remotePath := o.muRoot + "/config.toml"
	remoteText, _ := captureSSH(target, "cat "+remotePath+" 2>/dev/null") // empty if absent

	// Direction: inventory from source, seams kept from destination.
	srcText, dstText := localText, remoteText
	if o.pull {
		srcText, dstText = remoteText, localText
	}
	if strings.TrimSpace(srcText) == "" {
		src := "local config.toml"
		if o.pull {
			src = target + "'s config.toml (check --mu-root)"
		}
		return runErr("nothing to sync — %s is empty or missing", src)
	}
	rest, _ := tomledit.Split(srcText, localSeams...)
	_, seams := tomledit.Split(dstText, localSeams...)
	merged := tomledit.Assemble(rest, seams, localSeams...)

	dstDesc := target
	if o.pull {
		dstDesc = localPath
	}
	if strings.TrimSpace(merged) == strings.TrimSpace(dstText) {
		render.OK(dstDesc + ": config.toml already in sync")
		return nil
	}
	showConfigDiff(dstText, merged)
	if !o.yes {
		if !confirm("write config.toml to %s?", dstDesc) {
			render.Info("aborted config.toml")
			return nil
		}
	}

	if o.pull {
		if err := writeLocalConfig(localPath, localBytes, merged); err != nil {
			return err
		}
		msg := "pulled config.toml ← " + target + keptClause("this machine's", seams)
		render.OK(msg)
		render.EventOK("setup", msg)
		return nil
	}
	// push: write atomically on the box — stage to .tmp, then mv into place.
	script := "mkdir -p " + o.muRoot + " && cat > " + remotePath + ".tmp && mv " + remotePath + ".tmp " + remotePath
	if err := pipeSSH(target, script, merged); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}
	msg := "synced config.toml → " + target + keptClause("its", seams)
	render.OK(msg)
	render.EventOK("setup", msg)
	return nil
}

// writeLocalConfig backs up the existing local config.toml to config.toml.bak, then writes
// merged atomically (stage to .tmp, rename into place), preserving the file's mode. A pull
// overwrites the real local config.toml, so the .bak is the undo.
func writeLocalConfig(path string, prev []byte, merged string) error {
	mode := os.FileMode(0o600)
	if fi, e := os.Stat(path); e == nil {
		mode = fi.Mode().Perm()
	}
	if len(prev) > 0 {
		if e := os.WriteFile(path+".bak", prev, mode); e != nil {
			return fmt.Errorf("back up %s: %w", path, e)
		}
	}
	tmp := path + ".tmp"
	if e := os.WriteFile(tmp, []byte(merged), mode); e != nil {
		return fmt.Errorf("write %s: %w", path, e)
	}
	if e := os.Rename(tmp, path); e != nil {
		return fmt.Errorf("install %s: %w", path, e)
	}
	return nil
}

// keptClause names the destination seam tables actually preserved, e.g. " (kept its
// [ssh]/[sshfs])" — or "" when the destination had none, so the success line never claims
// to keep what wasn't there. whose is "its" (push) or "this machine's" (pull).
func keptClause(whose string, seams map[string]string) string {
	var k []string
	for _, n := range localSeams {
		if strings.TrimSpace(seams[n]) != "" {
			k = append(k, "["+n+"]")
		}
	}
	if len(k) == 0 {
		return ""
	}
	return " (kept " + whose + " " + strings.Join(k, "/") + ")"
}

// showConfigDiff prints a unified diff of the destination's current config.toml vs the
// merged one (— current, + merged) via the system `diff`; on no diff tool it prints nothing.
func showConfigDiff(oldText, newText string) {
	od, err1 := os.CreateTemp("", "cfg-old-*.toml")
	nd, err2 := os.CreateTemp("", "cfg-new-*.toml")
	if err1 != nil || err2 != nil {
		return
	}
	defer func() { _ = os.Remove(od.Name()); _ = os.Remove(nd.Name()) }()
	_, _ = od.WriteString(oldText)
	_, _ = nd.WriteString(newText)
	_ = od.Close()
	_ = nd.Close()
	render.Detail("config.toml changes (— current, + merged):")
	// Prefer git's colored diff (honors the user's diff.color config); fall back to plain
	// `diff -u` in plain/NO_COLOR mode or when git can't run. Both exit 1 on differences, so
	// output — not exit code — is the signal git produced a diff.
	if !render.Plain() {
		if out, _ := exec.Command("git", "diff", "--no-index", "--color=always", od.Name(), nd.Name()).CombinedOutput(); len(out) > 0 {
			fmt.Fprintln(os.Stderr, strings.TrimRight(string(out), "\n"))
			return
		}
	}
	out, _ := exec.Command("diff", "-u", od.Name(), nd.Name()).CombinedOutput() // diff exits 1 when they differ
	fmt.Fprintln(os.Stderr, strings.TrimRight(string(out), "\n"))
}

// captureSSH runs a read command on the target and returns its stdout. -q quiets ssh's own
// chatter (login banner/MOTD) so it never contaminates captured output.
func captureSSH(target, cmd string) (string, error) {
	out, err := exec.Command("ssh", "-q", target, cmd).Output()
	return string(out), err
}

// pipeSSH runs a command on the target with stdin fed from content (its stderr surfaced).
// -q suppresses the login banner so only the command's own stderr reaches the user.
func pipeSSH(target, cmd, content string) error {
	c := exec.Command("ssh", "-q", target, cmd)
	c.Stdin = strings.NewReader(content)
	c.Stderr = os.Stderr
	return c.Run()
}
