package render

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

// MountRow is one row of the `mu sshfs list` table. Status is one of
// "mounted" | "hung" | "unmounted" | "parked (av)" | "parked (idle)" (the guard's). Groups is the comma-joined group list (may be
// empty); the Groups column is shown only when at least one row has groups.
type MountRow struct {
	Name, Node, Path, Status, Groups string
	RO                               bool
}

// MountsTable renders the sshfs mount list: name/node/remote-path/access/status.
// Colors are applied go-pretty-natively (column Colors + color-only Transformers)
// so cell values stay plain text and columns line up; the remote path is
// truncated with a leading … to fit the terminal width. A "LOCAL PATH:
// <mountsRoot>" line under the title gives the shared local root once (each
// mount's local dir is that root joined with its Name), instead of repeating it
// per row.
func MountsTable(rows []MountRow, mountsRoot string) {
	if colorOff() {
		text.DisableColors()
	}
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetStyle(table.StyleRounded)
	t.SetTitle("SSHFS Mounts\nLOCAL PATH: " + mountsRoot)

	// The Groups column appears only when some mount has groups, so the common
	// group-free listing keeps its original shape.
	showGroups := false
	for _, r := range rows {
		if r.Groups != "" {
			showGroups = true
			break
		}
	}

	header := table.Row{"Name", "Node", "Remote path", "Access", "Status"}
	if showGroups {
		header = table.Row{"Name", "Node", "Remote path", "Access", "Groups", "Status"}
	}
	t.AppendHeader(header)
	for _, r := range rows {
		access := "rw"
		if r.RO {
			access = "ro"
		}
		if showGroups {
			t.AppendRow(table.Row{r.Name, r.Node, r.Path, access, r.Groups, statusBadge(r.Status)})
		} else {
			t.AppendRow(table.Row{r.Name, r.Node, r.Path, access, statusBadge(r.Status)})
		}
	}

	cols := []table.ColumnConfig{
		{Name: "Name", Colors: text.Colors{text.FgGreen, text.Bold}},
		{Name: "Node", Colors: text.Colors{text.FgMagenta}},
		{Name: "Remote path", Colors: text.Colors{text.FgCyan}},
		{Name: "Access", Transformer: accessTransformer},
	}
	if showGroups {
		cols = append(cols, table.ColumnConfig{Name: "Groups", Colors: text.Colors{text.FgBlue}})
	}
	cols = append(cols, table.ColumnConfig{Name: "Status", Transformer: statusTransformer})
	fitPathColumn(cols, rows)
	t.SetColumnConfigs(cols)
	t.Render()
}

// statusBadge is the plain (uncolored) badge text; statusTransformer colors it
// without changing width. Kept split so go-pretty measures the real display width.
func statusBadge(status string) string {
	switch status {
	case "mounted":
		return glyph("●", "*") + " mounted"
	case "hung":
		return glyph("!", "!") + " hung"
	case "parked (av)", "parked (idle)":
		return glyph("◌", "o") + " " + status
	default:
		return glyph("○", "o") + " not mounted"
	}
}

func statusTransformer(v interface{}) string {
	s := fmt.Sprint(v)
	switch {
	case strings.Contains(s, "not mounted"):
		return text.Colors{text.FgHiBlack}.Sprint(s)
	case strings.Contains(s, "hung"), strings.Contains(s, "parked"):
		return text.Colors{text.FgYellow}.Sprint(s)
	default:
		return text.Colors{text.FgGreen}.Sprint(s)
	}
}

func accessTransformer(v interface{}) string {
	s := fmt.Sprint(v)
	if s == "ro" {
		return text.Colors{text.FgYellow}.Sprint(s)
	}
	return text.Colors{text.FgHiBlack}.Sprint(s)
}

// fitPathColumn caps the remote-path column to whatever terminal width is left
// after the fixed columns, so the table never wraps. A no-op when the width is
// unknown (e.g. piped), leaving full paths intact.
func fitPathColumn(cols []table.ColumnConfig, rows []MountRow) {
	tw := termWidth()
	if tw <= 0 {
		return
	}
	nameW, nodeW := len("Name"), len("Node")
	accessW := len("Access")
	statusW := text.StringWidth(statusBadge("unmounted")) // widest badge
	groupsW := 0
	for _, r := range rows {
		nameW = max(nameW, text.StringWidth(r.Name))
		nodeW = max(nodeW, text.StringWidth(r.Node))
		groupsW = max(groupsW, text.StringWidth(r.Groups))
	}
	if groupsW > 0 {
		groupsW = max(groupsW, len("Groups"))
	}
	// StyleRounded overhead per column: 2 padding + a border glyph → 3*ncols + 1.
	// len(cols) already reflects the Groups column when present, so overhead scales;
	// groupsW is 0 when that column is absent.
	overhead := 3*len(cols) + 1
	budget := tw - (nameW + nodeW + accessW + statusW + groupsW + overhead)
	if budget < 8 {
		budget = 8 // floor: a narrow terminal still shows a stub, not nothing
	}
	for i := range cols {
		if cols[i].Name == "Remote path" {
			cols[i].WidthMax = budget
			cols[i].WidthMaxEnforcer = truncLeft
		}
	}
}

// truncLeft trims a string to maxLen display columns, keeping the right-hand end
// (the distinguishing tail of a path) behind a leading ….
func truncLeft(s string, maxLen int) string {
	if text.StringWidth(s) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return "…"
	}
	r := []rune(s)
	i, w := len(r), 0
	for i > 0 && w+text.StringWidth(string(r[i-1])) <= maxLen-1 {
		i--
		w += text.StringWidth(string(r[i]))
	}
	return "…" + string(r[i:])
}

// termWidth is the terminal column count: $COLUMNS if set, else the stdout tty
// size, else 0 (unknown — caller should skip width-fitting).
func termWidth() int {
	if c := os.Getenv("COLUMNS"); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 0 {
			return n
		}
	}
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		return w
	}
	return 0
}
