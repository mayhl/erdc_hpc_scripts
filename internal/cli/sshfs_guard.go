package cli

// `mu sshfs guard`: the unattended mount guard. A bare `guard` is ONE tick — sample,
// decide, act, save — which a user launchd agent (`guard install`) fires every 30 s;
// `status` shows what the guard is holding, `-n` previews a tick. The engine (sampling,
// state, the pure decision) is internal/sshfs/guard.go; this file is the wiring: turning
// registry + processes into observations, performing the actions with the same
// runMount/Umount the interactive verbs use, and the launchd plist.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/sshfs"
)

const (
	guardLabel    = "com.mayhl.mu.sshfs-guard"
	guardInterval = 30 * time.Second
)

func sshfsGuardCmd() *cobra.Command {
	tun := sshfs.DefaultGuardTuning()
	var dryRun bool
	c := &cobra.Command{
		Use:   "guard",
		Short: "Park mounts the AV is scanning and unmount idle ones (one tick; launchd runs it).",
		Long: "Run one tick of the mount guard. It parks (unmounts) a mount the laptop's AV is\n" +
			"traversing — scanner busy + that mount's fuse-t daemons busy + none of your own\n" +
			"processes in it — and remounts it after --park; a mount parked " + strconv.Itoa(tun.TripLimit) +
			"× in " + tun.TripWindow.String() + " stays\n" +
			"down (benched) until you `hmt` it. It also unmounts a mount nobody has touched for\n" +
			"--idle. Never runs pkinit: without a live ticket it just waits. `guard install` makes\n" +
			"launchd fire a tick every " + guardInterval.String() + "; `hls` shows parked mounts as ◌ parked.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return codeErr(runGuardTick(tun, dryRun, render.IsVerbose()))
		},
	}
	guardTuningFlags(c, &tun)
	c.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "report what this tick would do; touch nothing (with -v: the samples)")
	c.AddCommand(guardStatusCmd(), guardInstallCmd(), guardUninstallCmd())
	return c
}

// guardTuningFlags registers the two user-facing knobs (tick + install share them).
func guardTuningFlags(c *cobra.Command, tun *sshfs.GuardTuning) {
	c.Flags().DurationVar(&tun.Idle, "idle", tun.Idle, "unmount a mount unused for this long (0 = never)")
	c.Flags().DurationVar(&tun.Park, "park", tun.Park, "how long an AV-parked mount stays down before remounting")
}

// runGuardTick samples the machine, decides, and acts. Quiet when nothing happens (the
// launchd log stays small); every action is also an event (`mu log`).
func runGuardTick(tun sshfs.GuardTuning, dryRun, verbose bool) int {
	reg := sshfs.ReadRegistry()
	if len(reg) == 0 {
		return 0
	}
	st := sshfs.LoadGuardState()
	procs := sshfs.SampleProcs()
	scanner := sshfs.ScannerCPU(procs)
	daemon := sshfs.DaemonCPU(procs, sshfs.MountsRoot())

	mounted := map[string]bool{}
	for _, a := range sshfs.ActiveMounts() {
		mounted[a.Dir] = true
	}
	names := registeredMountNames()
	var mountedDirs []string
	for _, n := range names {
		if d := sshfs.MountDir(n); mounted[d] {
			mountedDirs = append(mountedDirs, d)
		}
	}
	held := sshfs.HeldByUser(mountedDirs)

	obs := make([]sshfs.Observation, 0, len(names))
	for _, n := range names {
		d := sshfs.MountDir(n)
		obs = append(obs, sshfs.Observation{Name: n, Mounted: mounted[d], DaemonCPU: daemon[d], Held: held[d]})
	}
	now := time.Now()
	ticketOK := hpc.TicketOK()
	actions := sshfs.Decide(st, scanner, obs, now, tun, ticketOK)

	if verbose {
		render.Detail(fmt.Sprintf("scanner %.1f%% (hot %d tick(s), threshold %.0f%%)  ticket %v", scanner, st.ScannerHotTicks, tun.ScannerHot, ticketOK))
		for _, o := range obs {
			if !o.Mounted {
				continue
			}
			idle := "-"
			if m := st.Mounts[o.Name]; m != nil && !m.LastActive.IsZero() {
				idle = now.Sub(m.LastActive).Round(time.Second).String()
			}
			render.Detail(fmt.Sprintf("  %-16s daemon %5.1f%%  held %-5v idle %s", o.Name, o.DaemonCPU, o.Held, idle))
		}
	}
	if dryRun {
		if len(actions) == 0 {
			render.Info("nothing to do")
		}
		for _, a := range actions {
			render.Info(fmt.Sprintf("would %s %s — %s", a.Op, a.Name, a.Why))
		}
		return 0
	}

	rc := 0
	for _, a := range actions {
		m := st.Mounts[a.Name]
		switch a.Op {
		case "park", "idle":
			if sshfs.Umount(sshfs.MountDir(a.Name)) {
				render.Warn(fmt.Sprintf("parked %s — %s", a.Name, a.Why))
				render.EventWarn("sshfs", "guard parked "+a.Name+" — "+a.Why)
			} else {
				m.Parked, m.Reason = time.Time{}, "" // still up; nothing owed
				render.Err(fmt.Sprintf("%s: couldn't unmount to park (%s)", a.Name, a.Why))
				rc = 1
			}
		case "remount":
			if runMount(a.Name, false, "", true) == 0 {
				render.EventOK("sshfs", "guard remounted "+a.Name)
			} else {
				m.Parked = now // another cooldown before the next try
				rc = 1
			}
		case "bench":
			render.Warn(fmt.Sprintf("%s: %s", a.Name, a.Why))
			render.EventWarn("sshfs", "guard benched "+a.Name+" — "+a.Why)
		}
	}
	if err := sshfs.SaveGuardState(st); err != nil {
		render.Err("guard state: " + err.Error())
		return 1
	}
	return rc
}

func guardStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the guard's agent state and what it is holding.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			plist := guardPlistPath()
			switch {
			case runtime.GOOS != "darwin":
				render.Info("agent: n/a (launchd is macOS-only)")
			case guardLoaded():
				render.OK("agent: loaded — " + plist)
			case fileExists(plist):
				render.Warn("agent: installed but not loaded — `mu sshfs guard install` reloads it")
			default:
				render.Info("agent: not installed — `mu sshfs guard install`")
			}
			st := sshfs.LoadGuardState()
			if fi, err := os.Stat(sshfs.GuardStatePath()); err == nil {
				render.Detail(fmt.Sprintf("last tick %s ago; scanner hot for %d tick(s)",
					time.Since(fi.ModTime()).Round(time.Second), st.ScannerHotTicks))
			} else {
				render.Detail("no ticks yet")
			}
			names := make([]string, 0, len(st.Mounts))
			for n := range st.Mounts {
				names = append(names, n)
			}
			sort.Strings(names)
			now := time.Now()
			for _, n := range names {
				m := st.Mounts[n]
				switch {
				case m.Benched:
					render.Warn(fmt.Sprintf("%s: benched (%d AV parks) — `hmt %s` to bring it back", n, len(m.Trips), n))
				case !m.Parked.IsZero() && m.Reason == "av":
					render.Info(fmt.Sprintf("%s: parked (av) %s ago — remounts after the cooldown", n, now.Sub(m.Parked).Round(time.Second)))
				case !m.Parked.IsZero():
					render.Info(fmt.Sprintf("%s: parked (idle) %s ago — `hmt %s` to bring it back", n, now.Sub(m.Parked).Round(time.Second), n))
				case !m.LastActive.IsZero():
					render.Detail(fmt.Sprintf("  %-16s up, idle %s%s", n, now.Sub(m.LastActive).Round(time.Second), tripsNote(m)))
				}
			}
			return nil
		},
	}
}

func tripsNote(m *sshfs.GuardMount) string {
	if len(m.Trips) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d recent AV park(s)", len(m.Trips))
}

func guardInstallCmd() *cobra.Command {
	tun := sshfs.DefaultGuardTuning()
	c := &cobra.Command{
		Use:   "install",
		Short: "Install (or reload) the launchd agent that ticks the guard every " + guardInterval.String() + ".",
		Long: "Write ~/Library/LaunchAgents/" + guardLabel + ".plist pointing at THIS mu binary\n" +
			"with the given --idle/--park and load it. Re-run after moving the binary or to\n" +
			"change the knobs. Output goes to " + filepath.Base(guardLogPath()) + " beside the state file.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runtime.GOOS != "darwin" {
				return usageErr("the guard agent is launchd-based (macOS only)")
			}
			mu, err := os.Executable()
			if err == nil {
				mu, err = filepath.EvalSymlinks(mu)
			}
			if err != nil {
				return runErr("locating mu: %s", err)
			}
			plist := guardPlistPath()
			if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
				return runErr("%s", err)
			}
			if err := os.MkdirAll(filepath.Dir(guardLogPath()), 0o755); err != nil {
				return runErr("%s", err)
			}
			if err := os.WriteFile(plist, []byte(guardPlist(mu, tun)), 0o644); err != nil {
				return runErr("%s", err)
			}
			_ = launchctl("bootout", guardDomain()+"/"+guardLabel) // reload: ignore "not loaded"
			if err := launchctl("bootstrap", guardDomain(), plist); err != nil {
				return runErr("launchctl bootstrap: %s", err)
			}
			render.OK(fmt.Sprintf("guard agent loaded — every %s, idle %s, park %s (%s)", guardInterval, tun.Idle, tun.Park, plist))
			return nil
		},
	}
	guardTuningFlags(c, &tun)
	return c
}

func guardUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Unload and remove the launchd agent (keeps the state file).",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runtime.GOOS != "darwin" {
				return usageErr("the guard agent is launchd-based (macOS only)")
			}
			plist := guardPlistPath()
			if !fileExists(plist) {
				render.Info("guard agent not installed")
				return nil
			}
			_ = launchctl("bootout", guardDomain()+"/"+guardLabel)
			if err := os.Remove(plist); err != nil {
				return runErr("%s", err)
			}
			render.OK("guard agent removed")
			return nil
		},
	}
}

func guardDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func guardPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", guardLabel+".plist")
}

// guardLogPath is the agent's stdout/stderr — beside the state file (ticks are quiet,
// so it only grows on actions).
func guardLogPath() string {
	return strings.TrimSuffix(sshfs.GuardStatePath(), ".json") + ".log"
}

// guardLoaded asks launchd whether the agent is bootstrapped.
func guardLoaded() bool {
	return launchctl("print", guardDomain()+"/"+guardLabel) == nil
}

func launchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// guardPlist renders the agent: this binary, one tick per interval, a PATH that reaches
// the tools a tick shells out to (ps/lsof/mount/umount/diskutil/klist/sshfs) since
// launchd's is bare. Background ProcessType keeps it off the interactive scheduling tier.
func guardPlist(mu string, tun sshfs.GuardTuning) string {
	home, _ := os.UserHomeDir()
	path := strings.Join([]string{
		filepath.Dir(mu), filepath.Join(home, ".local", "bin"),
		"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}, ":")
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<!-- managed by mu sshfs guard install — re-run it to change the knobs -->
	<key>Label</key><string>` + guardLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + mu + `</string>
		<string>sshfs</string>
		<string>guard</string>
		<string>--idle=` + tun.Idle.String() + `</string>
		<string>--park=` + tun.Park.String() + `</string>
	</array>
	<key>StartInterval</key><integer>` + strconv.Itoa(int(guardInterval.Seconds())) + `</integer>
	<key>RunAtLoad</key><true/>
	<key>ProcessType</key><string>Background</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key><string>` + path + `</string>
	</dict>
	<key>StandardOutPath</key><string>` + guardLogPath() + `</string>
	<key>StandardErrorPath</key><string>` + guardLogPath() + `</string>
</dict>
</plist>
`
}
