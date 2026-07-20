package cli

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/proc"
	"github.com/mayhl/mayhl_utils/internal/render"
)

func psCmd() *cobra.Command {
	var interactive, allUsers bool
	var userFlag string
	c := &cobra.Command{
		Use:   "ps [mask]",
		Short: "List your local processes, optionally filtered by a name mask.",
		Long: "List local processes as the house table. Defaults to your own processes;\n" +
			"-u <user> shows another user's, -a shows all. An optional argument filters\n" +
			"the list — a name mask (grep-style), a PID, a PID range (4501-4510), or a\n" +
			"list (4501,4507). Prefix a numeric that's really a name with ~ to force a\n" +
			"mask. With -i, open the interactive picker to filter, multi-select, and\n" +
			"kill. To signal matches headlessly, see `mu ps kill`.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			mask := ""
			if len(args) == 1 {
				mask = args[0]
			}
			if interactive {
				return psInteractive(userFlag, allUsers, mask)
			}
			ps, err := proc.List()
			if err != nil {
				return err
			}
			ps = filterUser(ps, userFlag, allUsers)
			if mask != "" {
				ps = proc.Classify(mask).Match(ps)
			}
			if len(ps) == 0 {
				render.Info("no matching processes for " + userScope(userFlag, allUsers))
				return nil
			}
			render.ProcTable(fmt.Sprintf("Processes (%d) — %s", len(ps), userScope(userFlag, allUsers)), procRows(ps))
			return nil
		},
	}
	setHelpArgs(c, [2]string{"[mask]", "filter: " + argProcSelectorDesc})
	c.Flags().BoolVarP(&interactive, "interactive", "i", false, "pick processes to kill interactively")
	c.Flags().StringVarP(&userFlag, "user", "u", "", "filter by user (default: you)")
	c.Flags().BoolVarP(&allUsers, "all-users", "a", false, "include every user's processes")
	c.AddCommand(psKillCmd())
	setHelpShortcuts(c, [2]string{"mps", "list local processes (mps -i = interactive picker)"})
	return c
}

// argProcSelectorDesc is the shared Arguments-panel line for the process verbs'
// selectors — one wording, matching their Long texts.
const argProcSelectorDesc = "name mask (grep-style), PID, PID range (4501-4510), or list (4501,4507); ~ forces a mask"

// psInteractive runs the live picker (a selector, not an actuator), then hands the
// picks to the SAME kill path as headless — so the destructive op is confirmed and
// event-logged identically however the set was built. The picker keeps its list
// fresh via the snapshot closure; selected PIDs are resolved against a final
// snapshot so a proc that died mid-session is simply dropped.
func psInteractive(userFlag string, allUsers bool, mask string) error {
	if !render.Interactive() {
		return fmt.Errorf("mu ps -i needs a terminal (stdin is not a tty)")
	}
	snapshot := func() []proc.Process {
		ps, err := proc.List()
		if err != nil {
			return nil
		}
		ps = filterUser(ps, userFlag, allUsers)
		if mask != "" {
			ps = proc.Classify(mask).Match(ps)
		}
		return ps
	}
	pids, err := render.Select(render.SelectSpec{
		Verb:    "kill",
		Columns: []string{"PID", "USER", "ST", "ELAPSED", "COMMAND"},
		Fetch:   func() []render.SelectRow { return procSelectRows(snapshot()) },
	})
	if err != nil {
		return err
	}
	if len(pids) == 0 {
		render.Info("nothing selected")
		return nil
	}
	want := make(map[string]bool, len(pids))
	for _, p := range pids {
		want[p] = true
	}
	var matched []proc.Process
	for _, p := range snapshot() {
		if want[strconv.Itoa(p.PID)] {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		render.Info("selected processes are no longer running")
		return nil
	}
	return killProcs(matched, syscall.SIGTERM, false)
}

func psKillCmd() *cobra.Command {
	var hard, pattern, yes, allUsers bool
	var userFlag string
	c := &cobra.Command{
		Use:   "kill <selector>...",
		Short: "Signal your processes by name mask, PID, PID range, or list (preview + confirm).",
		Long: "Resolve each selector against your processes, preview the matched set, and\n" +
			"signal it after confirmation. Scoped to your own processes by default (you\n" +
			"can't signal another user's anyway); -u <user> or -a widen it. Default\n" +
			"signal is SIGTERM (graceful); -9 escalates to SIGKILL. A selector is a name\n" +
			"mask, a PID, a range (4501-4510), or a list (4501,4507); -p forces every\n" +
			"argument to be a mask. Front-door: `mkill`.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ps, err := proc.List()
			if err != nil {
				return err
			}
			ps = filterUser(ps, userFlag, allUsers)
			matched := selectProcs(ps, args, pattern)
			if len(matched) == 0 {
				render.Info("no matching processes for " + userScope(userFlag, allUsers))
				return nil
			}
			sig := syscall.SIGTERM
			if hard {
				sig = syscall.SIGKILL
			}
			return killProcs(matched, sig, yes)
		},
	}
	setHelpArgs(c, [2]string{"<selector>", argProcSelectorDesc})
	c.Flags().BoolVarP(&hard, "sigkill", "9", false, "use SIGKILL (-9) instead of the default SIGTERM")
	c.Flags().BoolVarP(&pattern, "pattern", "p", false, "force every argument to be a name mask (not a PID)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	c.Flags().StringVarP(&userFlag, "user", "u", "", "target another user (default: you)")
	c.Flags().BoolVarP(&allUsers, "all-users", "a", false, "target every user's processes")
	return c
}

// killProcs is the shared actuator: preview the set, confirm (unless yes), signal,
// and record the destructive op in the event log. Both `mu ps kill` (headless) and
// `mu ps -i` (interactive pick) funnel through here so logging/confirm are uniform.
func killProcs(matched []proc.Process, sig syscall.Signal, yes bool) error {
	render.ProcTable(fmt.Sprintf("Kill %d process(es) — %s", len(matched), sigName(sig)), procRows(matched))
	if !yes {
		if !confirm("kill %d process(es) with %s?", len(matched), sigName(sig)) {
			render.Info("aborted")
			return nil
		}
	}
	if err := proc.Signal(pids(matched), sig); err != nil {
		return err
	}
	msg := fmt.Sprintf("signalled %d process(es) with %s", len(matched), sigName(sig))
	render.OK(msg)
	render.EventOK("ps", msg)
	return nil
}

// filterUser scopes the list by owner: all users when all, else the given user, or
// the current user when none is named (the default — a full listing is too long).
func filterUser(ps []proc.Process, userFlag string, all bool) []proc.Process {
	if all {
		return ps
	}
	want := userFlag
	if want == "" {
		want = currentUser()
	}
	var out []proc.Process
	for _, p := range ps {
		if p.User == want {
			out = append(out, p)
		}
	}
	return out
}

// userScope is the human label for the active owner filter, shown in the title.
func userScope(userFlag string, all bool) string {
	switch {
	case all:
		return "all users"
	case userFlag != "":
		return userFlag
	default:
		return currentUser()
	}
}

// currentUser is your login name, matching the USER column ps reports.
func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// selectProcs resolves every selector arg against ps and returns the deduped union
// (a process matched by two selectors is signalled once). forcePattern makes every
// arg a mask, for a numeric that's really a process name.
func selectProcs(ps []proc.Process, args []string, forcePattern bool) []proc.Process {
	seen := make(map[int]bool)
	var out []proc.Process
	for _, a := range args {
		sel := proc.Classify(a)
		if forcePattern {
			sel = proc.Selector{Kind: proc.Mask, Pat: strings.TrimPrefix(a, "~")}
		}
		for _, p := range sel.Match(ps) {
			if !seen[p.PID] {
				seen[p.PID] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// procSelectRows adapts processes into generic picker rows: ID = PID, cells match
// the ProcTable columns, PID cyan / User magenta via the house palette.
func procSelectRows(ps []proc.Process) []render.SelectRow {
	rows := make([]render.SelectRow, len(ps))
	for i, p := range ps {
		pid := strconv.Itoa(p.PID)
		rows[i] = render.SelectRow{
			ID:    pid,
			Cells: []string{pid, p.User, p.State, p.Elapsed, p.Command},
			Hues:  []string{render.HueID, render.HueUser, "", "", ""},
		}
	}
	return rows
}

func procRows(ps []proc.Process) []render.ProcRow {
	rows := make([]render.ProcRow, len(ps))
	for i, p := range ps {
		rows[i] = render.ProcRow{
			PID:     strconv.Itoa(p.PID),
			User:    p.User,
			State:   p.State,
			Elapsed: p.Elapsed,
			Command: p.Command,
		}
	}
	return rows
}

func pids(ps []proc.Process) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.PID
	}
	return out
}

func sigName(s syscall.Signal) string {
	switch s {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return s.String()
	}
}
