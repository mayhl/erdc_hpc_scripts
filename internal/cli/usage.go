package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// showUsageCmd is the site command mu runs to report per-subproject allocation hours.
// Its banner also carries the percent of the fiscal year remaining — mu parses that too,
// for the title and the per-row pace column.
const showUsageCmd = "show_usage"

// hpcUsageCmd is `mu hpc usage`: per-subproject allocation usage as a house table, with a
// derived "vs FY" pace column — allocation-percent-remaining minus fiscal-year-percent-
// remaining — so a negative margin warns of overuse (burning hours faster than the year
// passes) and a big positive one late in the year warns of forfeiture (use-it-or-lose-it).
// Sibling of `mu hpc storage`, same targets.
func hpcUsageCmd() *cobra.Command {
	var node string
	var local, fleet, all, raw, jsonOut bool
	c := &cobra.Command{
		Use:   "usage",
		Short: "Show allocation usage (show_usage) with a fiscal-year pace column.",
		Long: "List each subproject's allocation hours — allocated, used, remaining, Remain%\n" +
			"— as one house table. The banner's percent-of-fiscal-year-remaining lands in\n" +
			"the title, and each row gets a derived `vs FY` margin (Remain% minus FY%):\n" +
			"negative means the allocation is burning faster than the year passes — overuse.\n" +
			"A large positive margin is its own warning (marked ↑): hours that can no longer\n" +
			"plausibly be spent before the year ends are forfeited — use them or lose them.\n" +
			"--raw prints the site command's own output verbatim.\n\n" +
			"Target, like `mu hpc storage`: --node fetches one cluster over remote-exec,\n" +
			"--local runs it on the current cluster (no ssh), -f/--fleet and\n" +
			"-e/--all-systems collate (adding a System column), and with none of those a\n" +
			"listing piped on stdin is parsed:\n" +
			"    mu hpc usage --node hpc1\n" +
			"    mu hpc usage -f\n" +
			"    hpc1 show_usage | mu hpc usage",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if fleet || all {
				targets, scope := fleetScope(), "fleet"
				if all {
					targets, scope = allSystemsScope(), "all"
				}
				label, infos, down, err := collateSite(targets, scope, showUsageCmd, parseUsageWithFY,
					func(r *queue.UsageInfo, lbl string) { r.System = lbl })
				if err != nil {
					return err
				}
				for _, d := range down {
					render.Warn(d)
				}
				// Group by subproject code: an allocation spans systems, so its rows
				// sit together with the System column telling them apart.
				sort.SliceStable(infos, func(i, j int) bool {
					if infos[i].Subproject != infos[j].Subproject {
						return infos[i].Subproject < infos[j].Subproject
					}
					return infos[i].System < infos[j].System
				})
				cacheCollatedUsage(infos)
				if jsonOut {
					return writeJSON(infos)
				}
				if len(infos) == 0 {
					return nil
				}
				render.UsageTable(label, "", collatedUsageRows(infos))
				return nil
			}
			var (
				label, out string
				err        error
				live       bool // a real machine answered — a piped listing has no cluster to cache under
			)
			switch {
			case node != "":
				label, out, err = fetchSite(node, showUsageCmd)
				live = true
			case local:
				label, out, err = fetchSiteLocal(showUsageCmd)
				live = true
			case !term.IsTerminal(os.Stdin.Fd()):
				var data []byte
				data, err = io.ReadAll(os.Stdin)
				label, out = "usage", string(data)
			default:
				label, out, err = fetchSiteLocal(showUsageCmd)
				live = true
			}
			if err != nil {
				return err
			}
			if raw {
				fmt.Print(out)
				return nil
			}
			infos := parseUsageWithFY(out)
			if live {
				// The view is the refresh: every live listing restocks `mu config -i`'s
				// account picker (names only — see acctcache.go) and the submit pre-flight's
				// hours cache (the whole rows, stamped — see usagecache.go), so a later
				// `mu job sub` shows its allocation share without fetching again.
				writeAcctCache(label, infos)
				writeUsageCache(label, infos)
			}
			if len(infos) == 0 {
				if out != "" {
					render.Warn("no usage rows parsed — is this `show_usage` output?")
				}
				return nil
			}
			if jsonOut {
				return writeJSON(infos)
			}
			render.UsageTable(label, infos[0].FYLeft, toUsageRows(infos, false))
			return nil
		},
	}
	c.Flags().StringVarP(&node, "node", "N", "", "fetch usage from this node (else read stdin)")
	c.Flags().BoolVarP(&local, "local", "l", false, "run show_usage on the current cluster, locally (no ssh)")
	c.Flags().BoolVarP(&fleet, "fleet", "f", false, "collate the fleet's usage (adds a System column)")
	c.Flags().BoolVarP(&all, "all-systems", "e", false, "collate every configured cluster, incl. inactive")
	c.Flags().BoolVar(&raw, "raw", false, "print show_usage's own output verbatim")
	c.Flags().BoolVar(&jsonOut, "json", false, "emit the parsed rows as JSON (verbatim fields + fy_left) instead of a table")
	c.MarkFlagsMutuallyExclusive("node", "local", "fleet", "all-systems")
	c.MarkFlagsMutuallyExclusive("json", "raw")
	c.MarkFlagsMutuallyExclusive("raw", "fleet")
	c.MarkFlagsMutuallyExclusive("raw", "all-systems")
	completeNodeFlag(c)
	return c
}

// parseUsageWithFY parses the show_usage table and stamps every row with the banner's
// fiscal-year percent — kept per row so collated systems each pace against their own
// banner (and it reaches --json).
func parseUsageWithFY(out string) []queue.UsageInfo {
	rows := queue.ParseShowUsage(out)
	fy := queue.ParseFiscalYearLeft(out)
	for i := range rows {
		rows[i].FYLeft = fy
	}
	return rows
}

// cacheCollatedUsage restocks every system a collate reached — both the account picker
// (names) and the submit pre-flight's hours cache — from one fan-out. The rows already carry
// their System tag, so grouping by it refreshes the whole fleet's caches at once, keyed by
// the same node label a later `mu job sub` looks up.
func cacheCollatedUsage(infos []queue.UsageInfo) {
	bySystem := map[string][]queue.UsageInfo{}
	for _, in := range infos {
		bySystem[in.System] = append(bySystem[in.System], in)
	}
	for label, rows := range bySystem {
		writeAcctCache(label, rows)
		writeUsageCache(label, rows)
	}
}

// toUsageRows maps parsed UsageInfo to render's plain UsageRow: hour figures thinned to
// k/M (countHuman — raw hours stay in --json), the vs-FY pace margin derived. collate
// keeps the System tag (switching the column on); a single-cluster view blanks it — the
// cluster is the title there.
func toUsageRows(infos []queue.UsageInfo, collate bool) []render.UsageRow {
	rows := make([]render.UsageRow, len(infos))
	for i, s := range infos {
		system := ""
		if collate {
			system = s.System
		}
		rows[i] = render.UsageRow{
			System:     system,
			Subproject: s.Subproject,
			Allocated:  countHuman(s.Allocated),
			Used:       countHuman(s.Used),
			Remaining:  countHuman(s.Remaining),
			RemainPct:  s.PctRemain,
			Background: countHuman(s.Background),
			VsFY:       vsFY(s.PctRemain, s.FYLeft),
			FYLeft:     s.FYLeft, // render paces the row against its own system's year percent
		}
	}
	return rows
}

// collatedUsageRows builds the grouped collate layout from the subproject-sorted rows:
// within a group only the first row carries the Subproject (continuations blank it — the
// divider cue for render), and a group spanning several systems gets a bold cross-system
// total row.
func collatedUsageRows(infos []queue.UsageInfo) []render.UsageRow {
	var out []render.UsageRow
	for i := 0; i < len(infos); {
		j := i
		for j < len(infos) && infos[j].Subproject == infos[i].Subproject {
			j++
		}
		group := infos[i:j]
		rows := toUsageRows(group, true)
		for k := 1; k < len(rows); k++ {
			rows[k].Subproject = ""
		}
		out = append(out, rows...)
		if len(group) > 1 {
			if total, ok := usageTotalRow(group); ok {
				out = append(out, total)
			}
		}
		i = j
	}
	return out
}

// usageTotalRow sums a subproject's hour figures across its systems into the bold total
// row: Remain% recomputed from the sums (not averaged), the pace margin against the
// group's fiscal-year percent when the systems agree on one. ok=false when the core hour
// fields don't all parse — a bogus total is worse than none.
func usageTotalRow(group []queue.UsageInfo) (render.UsageRow, bool) {
	var alloc, used, rem, bg int64
	for _, s := range group {
		a, err1 := strconv.ParseInt(strings.TrimSpace(s.Allocated), 10, 64)
		u, err2 := strconv.ParseInt(strings.TrimSpace(s.Used), 10, 64)
		r, err3 := strconv.ParseInt(strings.TrimSpace(s.Remaining), 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return render.UsageRow{}, false
		}
		alloc, used, rem = alloc+a, used+u, rem+r
		if b, err := strconv.ParseInt(strings.TrimSpace(s.Background), 10, 64); err == nil {
			bg += b
		}
	}
	pct := ""
	if alloc > 0 {
		pct = fmt.Sprintf("%.2f%%", float64(rem)*100/float64(alloc))
	}
	fy := group[0].FYLeft
	for _, s := range group[1:] {
		if s.FYLeft != fy {
			fy = "" // systems disagree on the year percent — no honest pace for the sum
			break
		}
	}
	return render.UsageRow{
		Total:      true,
		System:     "total",
		Allocated:  countHuman(strconv.FormatInt(alloc, 10)),
		Used:       countHuman(strconv.FormatInt(used, 10)),
		Remaining:  countHuman(strconv.FormatInt(rem, 10)),
		RemainPct:  pct,
		Background: countHuman(strconv.FormatInt(bg, 10)),
		VsFY:       vsFY(pct, fy),
		FYLeft:     fy,
	}, true
}

// vsFY derives the pace margin: allocation-percent-remaining minus fiscal-year-percent-
// remaining, in signed percentage points ("+26.3%" / "-23.7%"). "" when either side is
// missing/non-numeric — the pace column degrades away.
func vsFY(remainPct, fyLeft string) string {
	r, err1 := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(remainPct), "%"), 64)
	f, err2 := strconv.ParseFloat(strings.TrimSpace(fyLeft), 64)
	if err1 != nil || err2 != nil {
		return ""
	}
	return fmt.Sprintf("%+.1f%%", r-f)
}
