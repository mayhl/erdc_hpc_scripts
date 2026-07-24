package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// showStorageCmd is the site command mu runs to report per-filesystem disk/file usage vs
// quota. mu parses its default KB output and formats human units itself (not -h): the
// parser only ever sees integers, and Use% stays exact instead of re-deriving from
// rounded "1.2T" strings.
const showStorageCmd = "show_storage"

// hpcStorageCmd is `mu hpc storage`: per-filesystem disk and file quota usage as a house
// table, with derived Use%/File% columns. Sibling of `mu hpc queues`, same target set:
// --node fetches one cluster over remote-exec, --local runs it on the current cluster,
// else stdin is parsed. No System column — single-cluster views carry the cluster in the
// title (a future collate view would restore it).
func hpcStorageCmd() *cobra.Command {
	var node, fleet string
	var local, all, raw, jsonOut bool
	c := &cobra.Command{
		Use:   "storage",
		Short: "Show disk and file quota usage (show_storage) as a house table.",
		Long: "List each filesystem's disk and file usage against its quota — with derived\n" +
			"Use% columns — as one house table. Sizes are formatted from show_storage's raw\n" +
			"KB output; --raw prints the site command's own output verbatim. A quota column\n" +
			"that no filesystem reports is dropped.\n\n" +
			"Target, like `mu hpc queues`: --node fetches one cluster over remote-exec,\n" +
			"--local runs it on the current cluster (no ssh), -f/--fleet collates the fleet\n" +
			"and -e/--all-systems every configured cluster (adding a System column), and\n" +
			"with none of those a listing piped on stdin is parsed:\n" +
			"    mu hpc storage --node hpc1\n" +
			"    mu hpc storage -f\n" +
			"    hpc1 show_storage | mu hpc storage",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if fleet != "" || all {
				targets, scope, err := scopeTargets(fleet, all)
				if err != nil {
					return err
				}
				label, infos, down, err := collateStorage(targets, scope)
				if err != nil {
					return err
				}
				for _, d := range down {
					render.Warn(d)
				}
				if jsonOut {
					return writeJSON(infos)
				}
				if len(infos) == 0 {
					return nil
				}
				render.StorageTable(label, toStorageRows(infos, true))
				return nil
			}
			var (
				label, out string
				err        error
			)
			switch {
			case node != "":
				label, out, err = fetchSite(node, showStorageCmd)
			case local:
				label, out, err = fetchSiteLocal(showStorageCmd)
			case !term.IsTerminal(os.Stdin.Fd()):
				var data []byte
				data, err = io.ReadAll(os.Stdin)
				label, out = "storage", string(data)
			default:
				label, out, err = fetchSiteLocal(showStorageCmd)
			}
			if err != nil {
				return err
			}
			if raw {
				fmt.Print(out)
				return nil
			}
			rows := queue.ParseShowStorage(out)
			if len(rows) == 0 {
				if out != "" {
					render.Warn("no storage rows parsed — is this `show_storage` output?")
				}
				return nil
			}
			if jsonOut {
				return writeJSON(rows)
			}
			render.StorageTable(label, toStorageRows(rows, false))
			return nil
		},
	}
	addSiteScopeFlags(c, &node, &local, &fleet, &all, siteScopeHelp{
		node:  "fetch storage from this node (else read stdin)",
		local: "run show_storage on the current cluster, locally (no ssh)",
		fleet: "collate the fleet's storage (adds a System column)",
		all:   "collate every configured cluster, incl. inactive",
	})
	c.Flags().BoolVar(&raw, "raw", false, "print show_storage's own output verbatim")
	c.Flags().BoolVar(&jsonOut, "json", false, "emit the parsed rows as JSON (raw KB, untruncated) instead of a table")
	c.MarkFlagsMutuallyExclusive("json", "raw")
	c.MarkFlagsMutuallyExclusive("raw", "fleet")
	c.MarkFlagsMutuallyExclusive("raw", "all-systems")
	return c
}

// collateStorage is the -f/--fleet / -e/--all-systems storage view: the shared site-command
// fan-out with each row's System tagged by cluster label.
func collateStorage(targets []queueTarget, scope string) (string, []queue.StorageInfo, []string, error) {
	return collateSite(targets, scope, showStorageCmd, queue.ParseShowStorage,
		func(r *queue.StorageInfo, label string) { r.System = label })
}

// toStorageRows maps parsed StorageInfo to render's plain StorageRow: KB → human sizes,
// counts thinned to k/M where big, and Use% derived from the raw integers. collate keeps
// the System tag (switching the column on); a single-cluster view blanks it — the
// cluster is the title there.
func toStorageRows(infos []queue.StorageInfo, collate bool) []render.StorageRow {
	rows := make([]render.StorageRow, len(infos))
	for i, s := range infos {
		system := ""
		if collate {
			system = s.System
		}
		rows[i] = render.StorageRow{
			System:     system,
			Location:   s.Location,
			DiskUsed:   kbHuman(s.DiskUsedKB),
			DiskQuota:  kbHuman(s.DiskQuotaKB),
			DiskPct:    usedPct(s.DiskUsedKB, s.DiskQuotaKB),
			FilesUsed:  countHuman(s.FilesUsed),
			FilesQuota: countHuman(s.FilesQuota),
			FilesPct:   usedPct(s.FilesUsed, s.FilesQuota),
		}
	}
	return rows
}

// kbHuman formats a raw show_storage KB figure as a human size ("" for a blank or
// non-numeric field — the table renders it as --).
func kbHuman(kb string) string {
	n, err := strconv.ParseInt(strings.TrimSpace(kb), 10, 64)
	if err != nil {
		return ""
	}
	return render.HumanBytes(n * 1024)
}

// countHuman thins a big count — files here, allocation hours in usage — (1234567 →
// "1.2M", 41250 → "41.2k"); small counts stay exact. "" for blank/non-numeric.
func countHuman(s string) string {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return ""
	}
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 10_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// usedPct derives an integer percent from raw used/quota strings, "" when either side is
// non-numeric or the quota is zero/absent (unlimited — no meaningful percent).
func usedPct(used, quota string) string {
	u, err1 := strconv.ParseInt(strings.TrimSpace(used), 10, 64)
	q, err2 := strconv.ParseInt(strings.TrimSpace(quota), 10, 64)
	if err1 != nil || err2 != nil || q <= 0 {
		return ""
	}
	return strconv.FormatInt((u*100+q/2)/q, 10) // rounded, integer math
}
