package render

import (
	"fmt"
	"os"
	"strings"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// applyStyle sets the house rounded style, or a borderless tab-aligned style when
// plainMode() (MU_RENDER=plain / --plain / piped). Plain implies no color — the
// status glyph (or MU_ASCII label) still carries meaning. Under asciiMode() (MU_ASCII
// or a non-UTF-8 locale) it keeps the framed layout but swaps the rounded box for the
// ASCII +/-/| box, so a PuTTY/latin1 session gets a clean frame instead of mojibake —
// the table-side analog of the picker's asciiBorder.
func applyStyle(t table.Writer) {
	if plainMode() {
		s := table.StyleDefault
		s.Options.DrawBorder = false
		s.Options.SeparateColumns = false
		s.Options.SeparateHeader = false
		t.SetStyle(s)
		text.DisableColors()
		return
	}
	s := table.StyleRounded
	if asciiMode() {
		s.Box = table.StyleDefault.Box // ASCII +/-/| frame on non-UTF-8 terminals
	}
	t.SetStyle(s)
	if colorOff() {
		text.DisableColors()
	}
}

// col is one renderable table column: its header and a row→cell formatter. The
// column-planned house tables (queues/storage/usage/tunnels) each build a []col
// over their own row type; plain plans assemble through appendCols, the variant
// layouts (grouped usage, width-shed tunnels) compose headerRow/bodyRow directly.
type col[T any] struct {
	header string
	cell   func(T) string
}

// headerRow formats the column headers as one table row.
func headerRow[T any](cols []col[T]) table.Row {
	row := make(table.Row, len(cols))
	for i, c := range cols {
		row[i] = c.header
	}
	return row
}

// bodyRow formats one item through the column plan.
func bodyRow[T any](cols []col[T], r T) table.Row {
	row := make(table.Row, len(cols))
	for i, c := range cols {
		row[i] = c.cell(r)
	}
	return row
}

// appendCols writes the header and one body row per item — the whole table body
// for a plan with no per-row layout tricks.
func appendCols[T any](t table.Writer, cols []col[T], rows []T) {
	t.AppendHeader(headerRow(cols))
	for _, r := range rows {
		t.AppendRow(bodyRow(cols, r))
	}
}

// anyReported reports whether any row carries a real (non-blank, non-zero) value in the
// given field. Values arrive preformatted, so zero is "0" (counts) or "0B" (sizes).
func anyReported[T any](rows []T, get func(T) string) bool {
	for _, r := range rows {
		switch strings.TrimSpace(get(r)) {
		case "", "0", "0B":
		default:
			return true
		}
	}
	return false
}

// NodeGroup is one cluster's nodes for NodesTable — a render-local view that keeps render
// domain-free of config. Host is the fully-qualified ssh host for each node.
type NodeGroup struct {
	Cluster string
	Nodes   []NodeRow
}

// NodeRow is a single node's display fields: its short name and full ssh host.
type NodeRow struct {
	Name, Host string
}

// NodesTable renders `mu hpc nodes`: a framed username line plus a
// Cluster/Node/Host table (magenta cluster, bold-green node, cyan host, one
// cluster label per group, dividers between clusters). When status is non-empty
// (from `-s`), a reachability column is added — ● up (green) / ○ down (red),
// keyed by node name.
func NodesTable(groups []NodeGroup, user string, status map[string]string) {
	withStatus := len(status) > 0

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	applyStyle(t)
	// Username lives inside the title (like the sshfs list's LOCAL PATH line),
	// rather than in a separate box above the table.
	t.SetTitle("HPC Nodes\nUsername: " + user)
	header := table.Row{"Cluster", "Node", "Host"}
	if withStatus {
		header = append(header, "Status")
	}
	t.AppendHeader(header)
	for i, g := range groups {
		for j, n := range g.Nodes {
			label := ""
			if j == 0 {
				label = g.Cluster
			}
			row := table.Row{label, n.Name, n.Host}
			if withStatus {
				row = append(row, nodeStatusBadge(status[n.Name]))
			}
			t.AppendRow(row)
		}
		if i < len(groups)-1 {
			t.AppendSeparator()
		}
	}
	cols := []table.ColumnConfig{
		{Name: "Cluster", Colors: text.Colors{text.FgMagenta}},
		{Name: "Node", Colors: text.Colors{text.FgGreen, text.Bold}},
		{Name: "Host", Colors: text.Colors{text.FgCyan}},
	}
	if withStatus {
		cols = append(cols, table.ColumnConfig{Name: "Status", Transformer: nodeStatusTransformer})
	}
	t.SetColumnConfigs(cols)
	t.Render()
}

// StatusRow is one row of a StatusTable: a level ("ok"|"warn"|"error"|"info"), a
// name, and a detail string. Domain-free — any grouped check output can use it.
type StatusRow struct {
	Level, Name, Detail string
}

// StatusTable renders a titled rounded table of check rows, the status column a
// glyph colored by level. Used by `mu doctor` (one table per section).
func StatusTable(title string, rows []StatusRow) {
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	applyStyle(t)
	// House accents: cyan-bold title, cyan headers, dim frame, bold-magenta check
	// name, white detail — the colored status glyph still carries the verdict.
	t.Style().Title.Colors = text.Colors{text.FgCyan, text.Bold}
	t.Style().Color.Header = text.Colors{text.FgCyan}    // headers: cyan accent
	t.Style().Color.Border = text.Colors{text.FgHiBlack} // frame: quiet dim chrome
	t.Style().Color.Separator = text.Colors{text.FgHiBlack}
	t.SetTitle(title)
	t.AppendHeader(table.Row{"", "Check", "Detail"})
	for _, r := range rows {
		t.AppendRow(table.Row{statusCell(r.Level), r.Name, r.Detail})
	}
	// Detail left uncolored → terminal default foreground (white on dark), theme-aware.
	t.SetColumnConfigs([]table.ColumnConfig{
		{Name: "Check", Colors: text.Colors{text.FgMagenta, text.Bold}},
	})
	t.Render()
}

func statusCell(level string) string {
	switch strings.ToUpper(level) {
	case "OK":
		return text.Colors{text.FgGreen, text.Bold}.Sprint(glyph("✓", "OK"))
	case "WARN", "WARNING":
		return text.Colors{text.FgYellow, text.Bold}.Sprint(glyph("!", "WARN"))
	case "ERROR", "ERR", "FAIL":
		return text.Colors{text.FgRed, text.Bold}.Sprint(glyph("✗", "ERR"))
	default:
		return text.Colors{text.FgCyan}.Sprint(glyph("→", "INFO"))
	}
}

func nodeStatusBadge(status string) string {
	switch status {
	case "up":
		return glyph("●", "*") + " up"
	case "down":
		return glyph("○", "o") + " down"
	default:
		return glyph("?", "?") + " ?"
	}
}

func nodeStatusTransformer(v interface{}) string {
	s := fmt.Sprint(v)
	switch {
	case strings.Contains(s, "down"):
		return text.Colors{text.FgRed}.Sprint(s)
	case strings.Contains(s, "up"):
		return text.Colors{text.FgGreen}.Sprint(s)
	default:
		return text.Colors{text.FgHiBlack}.Sprint(s)
	}
}
