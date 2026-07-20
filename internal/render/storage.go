package render

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// StorageRow is one filesystem for StorageTable — plain preformatted fields, so render
// stays domain-free (like QueueRow). Sizes arrive human-formatted; the Pct fields are
// bare integer strings ("87") or "" when a percent can't be derived (no/unlimited quota).
// System is the owning cluster in a collate view, "" in a single-cluster view (where the
// cluster is the title and the column would be uniform noise).
type StorageRow struct {
	System, Location, DiskUsed, DiskQuota, DiskPct, FilesUsed, FilesQuota, FilesPct string
}

// StorageTable renders per-filesystem quota usage (show_storage) as the house table:
// [System] / Location / Used / [Quota / Use%] / Files / [FileQuota / File%]. A quota pair
// is dropped when no filesystem reports one, and System appears only in a collate view
// (rows tagged by cluster); single-cluster views carry the cluster in the title instead
// (see planStorageCols).
func StorageTable(cluster string, rows []StorageRow) {
	cols := planStorageCols(rows)
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	applyStyle(t)
	t.SetTitle(fmt.Sprintf("%s — storage", cluster))
	appendCols(t, cols, rows)
	t.SetColumnConfigs([]table.ColumnConfig{
		{Name: "System", Colors: tc(HueUser)}, // magenta — stands out from the blue Location
		{Name: "Location", Colors: append(tc(HueGroup), text.Bold), WidthMax: 32, WidthMaxEnforcer: truncRight},
		{Name: "Quota", Colors: tc(HueDim)},
		{Name: "FileQuota", Colors: tc(HueDim)},
	})
	t.Render()
}

// planStorageCols builds the ordered columns. System leads only when some row carries it
// (a collate view); a quota PAIR (Quota+Use%, FileQuota+File%) is dropped when no row
// reports a real quota — some systems emit 0/blank where no quota is set (file quotas
// especially), and a column of 0s with a -- percent is noise. Usage columns always stay:
// zero USAGE is information.
func planStorageCols(rows []StorageRow) []col[StorageRow] {
	var cols []col[StorageRow]
	if anyReported(rows, func(r StorageRow) string { return r.System }) {
		cols = append(cols, col[StorageRow]{"System", func(r StorageRow) string { return dash(r.System) }})
	}
	cols = append(
		cols,
		col[StorageRow]{"Location", func(r StorageRow) string { return r.Location }},
		col[StorageRow]{"Used", func(r StorageRow) string { return dash(r.DiskUsed) }},
	)
	if anyReported(rows, func(r StorageRow) string { return r.DiskQuota }) {
		cols = append(
			cols,
			col[StorageRow]{"Quota", func(r StorageRow) string { return dash(r.DiskQuota) }},
			col[StorageRow]{"Use%", func(r StorageRow) string { return pctCell(r.DiskPct) }},
		)
	}
	cols = append(cols, col[StorageRow]{"Files", func(r StorageRow) string { return dash(r.FilesUsed) }})
	if anyReported(rows, func(r StorageRow) string { return r.FilesQuota }) {
		cols = append(
			cols,
			col[StorageRow]{"FileQuota", func(r StorageRow) string { return dash(r.FilesQuota) }},
			col[StorageRow]{"File%", func(r StorageRow) string { return pctCell(r.FilesPct) }},
		)
	}
	return cols
}

// StoragePct grades a fullness percent into a display label + house hue — quota headroom
// is status, so the warm ramp applies: ≥90 HueErr, ≥75 HueWarn, else HueOK; a blank or
// non-numeric percent renders "--" (HueDim). The number carries the meaning; color is
// the accent. Exposed like QueueLoad so any future picker row reads it the same way.
func StoragePct(pct string) (label, hue string) {
	n, err := strconv.Atoi(strings.TrimSpace(pct))
	if err != nil {
		return "--", HueDim
	}
	switch {
	case n >= 90:
		return fmt.Sprintf("%d%%", n), HueErr
	case n >= 75:
		return fmt.Sprintf("%d%%", n), HueWarn
	default:
		return fmt.Sprintf("%d%%", n), HueOK
	}
}

// pctCell renders the StoragePct label colored for the static table.
func pctCell(pct string) string {
	label, hue := StoragePct(pct)
	return tc(hue).Sprint(label)
}
