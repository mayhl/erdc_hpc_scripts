package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/sshfs"
)

func sshfsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sshfs",
		Short: "Mount HPC dirs locally over sshfs (macOS/fuse-t).",
		Long: "Mount HPC dirs onto the local workstation over sshfs (macOS/fuse-t) so remote\n" +
			"paths behave like local files. Local-only. The h* shell shortcuts below are the\n" +
			"day-to-day front-doors (not 1:1 with the subcommands).",
	}
	c.AddCommand(sshfsListCmd(), sshfsMountCmd(), sshfsUmountCmd(), sshfsPathCmd(), sshfsAddCmd(), sshfsSetCmd(), sshfsRmCmd(), sshfsGroupCmd(false), sshfsGroupCmd(true), sshfsDoctorCmd())
	setHelpShortcuts(
		c,
		[2]string{"hcd <name>", "mount if needed + cd into it (mu sshfs mount)"},
		[2]string{"hmt <name>…", "mount, no cd; @group / --all / --stale (mu sshfs mount)"},
		[2]string{"hls", "list mounts with live status (mu sshfs list)"},
		[2]string{"hadd", "register a new mount (mu sshfs add)"},
		[2]string{"hset", "change/repoint a mount (mu sshfs set)"},
		[2]string{"hum", "unmount; hum --all = all live (mu sshfs umount)"},
		[2]string{"hgroup <g> <n>…", "add mounts to a group (mu sshfs group)"},
	)
	return c
}

// mountCompletion completes the first arg (mount name) from the registry.
func mountCompletion(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return sshfs.CompleteMount(toComplete), cobra.ShellCompDirectiveNoFileComp
}

func sshfsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured mounts with live status.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			reg := sshfs.ReadRegistry()
			if len(reg) == 0 {
				render.Info("no mounts — add one with `mu sshfs add <name> <node> <path>`")
				return nil
			}
			names := make([]string, 0, len(reg))
			for n := range reg {
				names = append(names, n)
			}
			sort.Strings(names)
			rows := make([]render.MountRow, 0, len(reg))
			for _, n := range names {
				m := reg[n]
				rows = append(rows, render.MountRow{
					Name: n, Node: m.Node, Path: m.Path, RO: m.RO, Status: sshfs.Status(n),
					Groups: strings.Join(m.Groups, ","),
				})
			}
			render.MountsTable(rows, sshfs.MountsRoot())
			return nil
		},
	}
}

func sshfsMountCmd() *cobra.Command {
	var all, stale, dryRun bool
	c := &cobra.Command{
		Use:   "mount <name>...",
		Short: "Mount configured names (--all mounts every registered, --stale remounts hung ones).",
		Args: func(_ *cobra.Command, args []string) error {
			if all || stale {
				if len(args) != 0 {
					return errors.New("cannot name a mount together with --all/--stale")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(nil, args)
		},
		ValidArgsFunction: mountCompletion,
		RunE: func(_ *cobra.Command, args []string) error {
			if all && stale {
				return usageErr("--all and --stale are mutually exclusive")
			}
			if dryRun && !stale {
				return usageErr("--dry-run only applies with --stale")
			}
			if stale {
				return codeErr(runMountStale(dryRun, render.IsVerbose()))
			}
			names := args
			if all {
				names = registeredMountNames()
				if len(names) == 0 {
					render.Info("no mounts — add one with `mu sshfs add <name> <node> <path>`")
					return nil
				}
			} else {
				expanded, err := expandMountArgs(args)
				if err != nil {
					return usageErr("%s", err)
				}
				names = expanded
			}
			if len(names) == 1 && !all {
				return codeErr(runMount(names[0], render.IsVerbose(), "", false)) // classic single-mount path (hcd uses this)
			}
			return codeErr(runMountBatch(names, render.IsVerbose()))
		},
	}
	setHelpArgs(c, [2]string{"<name>", "registered mount name, or @group for every mount in a group"})
	// -v (global) shows the remote target + verbose ssh output
	c.Flags().BoolVarP(&all, "all", "a", false, "mount every registered mount")
	c.Flags().BoolVar(&stale, "stale", false, "remount only mounts that have gone stale (hung)")
	c.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "with --stale: report stale mounts without remounting (exit non-zero if any)")
	return c
}

// staleNames returns the names whose status func reports "hung" — the stale mounts
// (in the mount table but not responding). Factored out so the filter is testable
// without shelling out to `mount`/`ls`.
func staleNames(names []string, status func(string) string) []string {
	var out []string
	for _, n := range names {
		if status(n) == "hung" {
			out = append(out, n)
		}
	}
	return out
}

// runMountStale scans every registered mount and re-establishes only the ones that
// have gone stale. With dryRun it reports them and exits non-zero if any (a scriptable
// health check); otherwise each is torn down and remounted via runMount's hung path.
// Healthy and unmounted mounts are left alone — this revives what died, it never mounts
// something that was never up.
func runMountStale(dryRun, verbose bool) int {
	stale := staleNames(registeredMountNames(), sshfs.Status)
	if len(stale) == 0 {
		render.Info("no stale mounts")
		return 0
	}
	if dryRun {
		for _, n := range stale {
			render.Warn(n + ": stale (hung)")
		}
		render.Info(fmt.Sprintf("%d stale mount(s) — remount with `mu sshfs mount --stale`", len(stale)))
		return 1
	}
	total := len(stale)
	failed := 0
	for i, n := range stale {
		var rc int
		if verbose {
			rc = runMount(n, true, "", false)
		} else {
			rc = runMount(n, false, fmt.Sprintf("Remounting %s  %d/%d", n, i+1, total), true)
		}
		if rc != 0 {
			failed++
		}
	}
	if failed > 0 {
		render.Warn(fmt.Sprintf("remounted %d/%d stale (%d failed)", total-failed, total, failed))
		return 1
	}
	render.OK(fmt.Sprintf("remounted %d/%d stale", total, total))
	return 0
}

// registeredMountNames returns all registered mount names, sorted.
func registeredMountNames() []string {
	reg := sshfs.ReadRegistry()
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// expandMountArgs turns a mount-arg list into concrete names, expanding any "@group"
// entry to that group's members (deduped, order-stable); bare names pass through.
// Errors if an @group has no members.
func expandMountArgs(args []string) ([]string, error) {
	reg := sshfs.ReadRegistry()
	var out []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "@") {
			members := groupMembers(reg, a[1:])
			if len(members) == 0 {
				return nil, fmt.Errorf("no mounts in group %q", a[1:])
			}
			for _, m := range members {
				add(m)
			}
			continue
		}
		add(a)
	}
	return out, nil
}

// groupMembers returns the names of mounts belonging to group g, sorted.
func groupMembers(reg map[string]sshfs.Mount, g string) []string {
	var out []string
	for name, m := range reg {
		for _, mg := range m.Groups {
			if mg == g {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// validGroupName rejects names that would break the registry encoding (tab/comma),
// the @-selector, or the #-comment marker.
func validGroupName(g string) error {
	if g == "" {
		return errors.New("empty group name")
	}
	if strings.ContainsAny(g, " \t,@#") {
		return errors.New("group name cannot contain a space, tab, comma, @ or #")
	}
	return nil
}

// sshfsGroupCmd builds `group`/`ungroup <group> <name>...`: add or remove a group on
// each named mount, then rewrite the registry. Unknown names warn and are skipped.
func sshfsGroupCmd(remove bool) *cobra.Command {
	use, short := "group <group> <name>...", "Add mounts to a group (mount together with `hmt @group`)."
	if remove {
		use, short = "ungroup <group> <name>...", "Remove mounts from a group."
	}
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			group, names := args[0], args[1:]
			if err := validGroupName(group); err != nil {
				return usageErr("%s", err)
			}
			reg := sshfs.ReadRegistry()
			changed := 0
			for _, name := range names {
				m, ok := reg[name]
				if !ok {
					render.Err(fmt.Sprintf("unknown mount: %s (see `mu sshfs list`)", name))
					continue
				}
				var did bool
				if remove {
					did = dropGroup(&m, group)
				} else {
					did = addGroup(&m, group)
				}
				if did {
					reg[name] = m
					changed++
				}
			}
			if changed > 0 {
				if err := sshfs.WriteRegistry(reg); err != nil {
					return runErr("%s", err)
				}
			}
			action := "added to"
			if remove {
				action = "removed from"
			}
			render.OK(fmt.Sprintf("%d mount(s) %s group %q", changed, action, group))
			return nil
		},
	}
	argVerb := "add"
	if remove {
		argVerb = "remove"
	}
	setHelpArgs(c,
		[2]string{"<group>", "free-form group name (mount together with `hmt @group`)"},
		[2]string{"<name>", "registered mount name(s) to " + argVerb})
	return c
}

// addGroup adds g to m.Groups (sorted, deduped); returns false if already present.
func addGroup(m *sshfs.Mount, g string) bool {
	for _, x := range m.Groups {
		if x == g {
			return false
		}
	}
	m.Groups = append(m.Groups, g)
	sort.Strings(m.Groups)
	return true
}

// dropGroup removes g from m.Groups; returns false if it wasn't a member.
func dropGroup(m *sshfs.Mount, g string) bool {
	var out []string
	found := false
	for _, x := range m.Groups {
		if x == g {
			found = true
			continue
		}
		out = append(out, x)
	}
	if found {
		m.Groups = out
	}
	return found
}

func sshfsUmountCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "umount <name>",
		Short: "Unmount a mount, or every live mount with --all.",
		Args: func(_ *cobra.Command, args []string) error {
			if all {
				if len(args) != 0 {
					return errors.New("cannot name a mount together with --all")
				}
				return nil
			}
			return cobra.ExactArgs(1)(nil, args)
		},
		ValidArgsFunction: mountCompletion,
		RunE: func(_ *cobra.Command, args []string) error {
			if all {
				return umountAll()
			}
			if !umountOne(args[0]) {
				return codeErr(1)
			}
			return nil
		},
	}
	setHelpArgs(c, [2]string{"<name>", "registered mount to unmount (see mu sshfs list)"})
	c.Flags().BoolVarP(&all, "all", "a", false, "unmount every live mount")
	return c
}

// umountOne unmounts a single mount by name, reporting the outcome. Returns
// false only when a live mount fails to unmount (hung).
func umountOne(name string) bool {
	mdir := sshfs.MountDir(name)
	if !sshfs.IsMounted(mdir) {
		render.Info(name + ": not mounted")
		return true
	}
	if sshfs.Umount(mdir) {
		render.OK("unmounted " + name)
		return true
	}
	render.Err(fmt.Sprintf("%s: couldn't unmount (hung?); try `diskutil unmount force %s` or a restart", name, mdir))
	return false
}

// umountAll unmounts every currently-live registered mount, continuing past a
// hung one and exiting non-zero at the end if any failed.
func umountAll() error {
	reg := sshfs.ReadRegistry()
	names := make([]string, 0, len(reg))
	for n := range reg {
		if sshfs.IsMounted(sshfs.MountDir(n)) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		render.Info("no live mounts")
		return nil
	}
	sort.Strings(names)
	failed := 0
	for _, n := range names {
		if !umountOne(n) {
			failed++
		}
	}
	if failed > 0 {
		return codeErr(1)
	}
	return nil
}

func sshfsPathCmd() *cobra.Command {
	var remote bool
	c := &cobra.Command{
		Use:               "path <name>",
		Short:             "Print the local mount dir (used by hcd to cd), or the remote node:path with -r. stdout = just the path.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: mountCompletion,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			m, ok := sshfs.ReadRegistry()[name]
			if !ok {
				return usageErr("unknown mount: %s", name)
			}
			if remote {
				fmt.Println(m.Node + ":" + m.Path)
				return nil
			}
			fmt.Println(sshfs.MountDir(name))
			return nil
		},
	}
	setHelpArgs(c, [2]string{"<name>", "registered mount name (see mu sshfs list)"})
	c.Flags().BoolVarP(&remote, "remote", "r", false, "print the remote node:path instead of the local dir")
	return c
}

func sshfsAddCmd() *cobra.Command {
	var readOnly bool
	c := &cobra.Command{
		Use:   "add <name> <node> <path>",
		Short: "Register a new mount (name → node:path). Does not mount.",
		Args:  cobra.ExactArgs(3),
		ValidArgsFunction: func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			switch len(args) {
			case 1: // node
				return hpc.CompleteNode(toComplete), cobra.ShellCompDirectiveNoFileComp
			case 2: // remote path on the chosen node (dirs, from the cached completer)
				return completeRemotePath(args[1], toComplete, true)
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(_ *cobra.Command, args []string) error {
			name, node, path := args[0], args[1], args[2]
			reg := sshfs.ReadRegistry()
			if ex, ok := reg[name]; ok {
				// warn (yellow) that it already exists, but still exit non-zero
				render.Warn(fmt.Sprintf("mount '%s' already exists → %s:%s", name, ex.Node, ex.Path))
				return codeErr(1)
			}
			if _, err := hpc.Resolve(node); err != nil { // validate the node resolves
				return usageErr("%s", err)
			}
			reg[name] = sshfs.Mount{Node: node, Path: path, RO: readOnly}
			if err := sshfs.WriteRegistry(reg); err != nil {
				return runErr("%s", err)
			}
			roTag := ""
			if readOnly {
				roTag = " (ro)"
			}
			render.OK(fmt.Sprintf("added %s → %s:%s%s", name, node, path, roTag))
			return nil
		},
	}
	setHelpArgs(c,
		[2]string{"<name>", "short name to register the mount under"},
		[2]string{"<node>", "cluster/node alias from the configured inventory"},
		[2]string{"<path>", "remote directory to mount"})
	c.Flags().BoolVar(&readOnly, "ro", false, "mount read-only (data to browse, no writes)")
	c.Flags().BoolVar(&readOnly, "read-only", false, "alias for --ro")
	return c
}

func sshfsSetCmd() *cobra.Command {
	var node, path string
	var ro, rw bool
	c := &cobra.Command{
		Use:   "set <name>",
		Short: "Change an existing mount's node/path or swap ro↔rw. Remounts if live.",
		Long: `Change an existing mount's target or access mode in the registry.

Only the flags you pass change:
  --node   repoint to a different HPC node (validated against your clusters)
  --path   repoint to a different remote path
  --ro/--rw   swap read-only ↔ read-write

sshfs bakes node/path/ro at mount time, so if the mount is live, set umounts and
remounts it to apply. Flipping an /archive (HSM) path to rw warns first — those
writes land on tape.

Examples:
  mu sshfs set scratch --path /archive/project/run   # fix a wrong path
  mu sshfs set scratch --rw                           # temporarily allow writes
  mu sshfs set scratch --node hpc1                    # move to another node`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: mountCompletion,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			reg := sshfs.ReadRegistry()
			m, ok := reg[name]
			if !ok {
				return usageErr("unknown mount: %s", name)
			}
			if ro && rw {
				return usageErr("--ro and --rw are mutually exclusive")
			}
			changed := false
			if node != "" && node != m.Node {
				if _, err := hpc.Resolve(node); err != nil { // validate the node resolves
					return usageErr("%s", err)
				}
				m.Node, changed = node, true
			}
			if path != "" && path != m.Path {
				m.Path, changed = path, true
			}
			if ro && !m.RO {
				m.RO, changed = true, true
			}
			if rw && m.RO {
				m.RO, changed = false, true
			}
			if !changed {
				render.Info(name + ": no change (pass --node/--path/--ro/--rw)")
				return nil
			}
			if !m.RO && strings.HasPrefix(m.Path, "/archive") {
				render.Warn(name + ": rw on an /archive (HSM) path — writes land on tape; make sure that's intended")
			}
			reg[name] = m
			if err := sshfs.WriteRegistry(reg); err != nil {
				return runErr("%s", err)
			}
			roTag := ""
			if m.RO {
				roTag = " (ro)"
			}
			render.OK(fmt.Sprintf("set %s → %s:%s%s", name, m.Node, m.Path, roTag))
			// Apply to a live mount: sshfs bakes node/path/ro at mount time, so a
			// change only takes effect on remount.
			if sshfs.IsMounted(sshfs.MountDir(name)) {
				render.Info(name + ": remounting to apply")
				if !sshfs.Umount(sshfs.MountDir(name)) {
					return runErr("%s: couldn't unmount to remount — unmount manually, then `hcd %s`", name, name)
				}
				return codeErr(runMount(name, false, "", false))
			}
			return nil
		},
	}
	setHelpArgs(c, [2]string{"<name>", "registered mount name (see mu sshfs list)"})
	c.Flags().StringVar(&node, "node", "", "repoint to a different node")
	c.Flags().StringVar(&path, "path", "", "repoint to a different remote path")
	c.Flags().BoolVar(&ro, "ro", false, "make read-only (remounts if live)")
	c.Flags().BoolVar(&rw, "rw", false, "make read-write (remounts if live)")
	// --path completes remote dirs on the mount's node (or --node, if being repointed).
	_ = c.RegisterFlagCompletionFunc("path", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		n := ""
		if m, ok := sshfs.ReadRegistry()[args[0]]; ok {
			n = m.Node
		}
		if nf, _ := cmd.Flags().GetString("node"); nf != "" {
			n = nf
		}
		if n == "" {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeRemotePath(n, toComplete, true)
	})
	completeNodeFlag(c) // --node completes cluster names
	return c
}

func sshfsRmCmd() *cobra.Command {
	c := &cobra.Command{
		Use:               "rm <name>",
		Short:             "Remove a mount from the registry (does not unmount).",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: mountCompletion,
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			reg := sshfs.ReadRegistry()
			if _, ok := reg[name]; !ok {
				return usageErr("unknown mount: %s", name)
			}
			if sshfs.IsMounted(sshfs.MountDir(name)) {
				render.Warn(name + " still mounted — `mu sshfs umount " + name + "` first")
			}
			delete(reg, name)
			if err := sshfs.WriteRegistry(reg); err != nil {
				return runErr("%s", err)
			}
			render.OK("removed " + name)
			return nil
		},
	}
	setHelpArgs(c, [2]string{"<name>", "registered mount to remove (see mu sshfs list)"})
	return c
}

func sshfsDoctorCmd() *cobra.Command {
	var fix, yes bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Scan every fuse mount for hung endpoints and orphans (not just the registry).",
		Long: "Inspect ALL fuse/sshfs mounts on this machine — the registry's and any others —\n" +
			"and flag the ones needing attention: a registered mount whose endpoint died\n" +
			"(hung), and a stale mu mount still mounted after its registry entry was removed.\n" +
			"Foreign fuse mounts (mounted outside mu) are reported but never touched. --fix\n" +
			"unmounts the hung + stale ones. The remount side is `hmt --stale`.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return codeErr(runSshfsDoctor(fix, yes))
		},
	}
	c.Flags().BoolVar(&fix, "fix", false, "unmount the hung + stale (unregistered mu) mounts it finds")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "with --fix, skip the confirmation prompt")
	return c
}

// mountKind classifies an active fuse mount by its dir: "known"+name for a registered
// mount dir, "stale" for a dir under mu's mounts root with no registry entry (a mu mount
// left after its registry entry was removed), else "foreign" (mounted outside mu). Pure —
// the doctor's testable core; the liveness probe is layered on in runSshfsDoctor.
func mountKind(dir string, regByDir map[string]string, mountsRoot string) (kind, name string) {
	if n, ok := regByDir[dir]; ok {
		return "known", n
	}
	if dir == mountsRoot || strings.HasPrefix(dir, mountsRoot+string(filepath.Separator)) {
		return "stale", filepath.Base(dir)
	}
	return "foreign", ""
}

// runSshfsDoctor scans every fuse-like mount, probes each for liveness, and reports the
// healthy/hung/stale/foreign split. --fix unmounts the hung + stale ones (mu's own dead
// mounts), never a foreign mount. Returns non-zero when a problem is found and left
// unfixed, so it doubles as a scriptable health check.
func runSshfsDoctor(fix, yes bool) int {
	reg := sshfs.ReadRegistry()
	regByDir := make(map[string]string, len(reg))
	for name := range reg {
		regByDir[sshfs.MountDir(name)] = name
	}
	mountsRoot := sshfs.MountsRoot()

	type problem struct{ dir, label string }
	var scanned, healthy int
	var problems []problem // hung + stale — the --fix targets

	for _, a := range sshfs.ActiveMounts() {
		if !a.FuseLike() {
			continue
		}
		scanned++
		kind, name := mountKind(a.Dir, regByDir, mountsRoot)
		live := sshfs.Responds(a.Dir)
		switch {
		case kind == "known" && live:
			healthy++
			render.Detail(fmt.Sprintf("● %-16s %s", name, a.Dir))
		case kind == "known":
			problems = append(problems, problem{a.Dir, name})
			render.Err(fmt.Sprintf("%s: hung — endpoint gone (%s)", name, a.Dir))
		case kind == "stale":
			problems = append(problems, problem{a.Dir, name})
			render.Warn(fmt.Sprintf("%s: stale — mounted but no longer in the registry (%s)", name, a.Dir))
		default: // foreign
			state := "ok"
			if !live {
				state = "hung"
			}
			render.Info(fmt.Sprintf("○ foreign %s mount (%s) — %s, left alone", a.Type, a.Dir, state))
		}
	}

	if scanned == 0 {
		render.Info("no fuse mounts active")
		return 0
	}
	render.Info(fmt.Sprintf("scanned %d fuse mount(s): %d healthy, %d need attention", scanned, healthy, len(problems)))
	if len(problems) == 0 {
		return 0
	}
	if !fix {
		render.Info("`mu sshfs doctor --fix` unmounts the hung/stale ones")
		return 1
	}

	// --fix: unmount the hung + stale mounts (never a foreign one).
	if !yes {
		for _, p := range problems {
			render.Detail("  will unmount " + p.dir)
		}
		if !confirm("unmount %d hung/stale mount(s)?", len(problems)) {
			render.Info("aborted")
			return 1
		}
	}
	failed := 0
	for _, p := range problems {
		if sshfs.Umount(p.dir) {
			render.OK("unmounted " + p.label)
		} else {
			render.Err(fmt.Sprintf("%s: couldn't unmount (hung?); try `diskutil unmount force %s`", p.label, p.dir))
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}
