// Package tomledit is a TOML text surgeon: it edits a config file in place, by line,
// without ever re-emitting it. This exists because go-toml/v2 has no comment-preserving
// AST — an unmarshal → edit → marshal round-trip would erase every comment and all
// formatting from config.toml, which is precisely the file a human hand-edits. So mu
// never marshals its config struct back to disk; it patches the text it was given.
//
// The model is deliberately shallow: a document is a list of lines plus the spans of the
// tables it contains. Enough to replace a key's value, revive one that was left commented
// out, and lift a table out verbatim (what `mu setup sync` splices). Not enough to
// understand TOML — anything subtler stays a hand-edit.
package tomledit

import "strings"

// Doc is a parsed document: the original lines, plus each table's span in file order.
type Doc struct {
	lines  []string
	tables []table
}

// table is one table's header and line span. A nested header keeps its full dotted path
// ("cluster.node"); array marks the [[…]] form, whose header may repeat.
type table struct {
	header     string // "" for the root (pre-header) lines
	array      bool
	start, end int // [start,end) into Doc.lines, header line included
}

// Parse splits text into lines and locates every table. It never rejects input: a line that
// doesn't parse as a header is body, so an unreadable file degrades to one root table
// rather than an error.
func Parse(text string) *Doc {
	d := &Doc{lines: strings.Split(text, "\n")}
	cur := table{start: 0}
	for i, line := range d.lines {
		h, array, ok := header(line)
		if !ok {
			continue
		}
		cur.end = i
		d.tables = append(d.tables, cur)
		cur = table{header: h, array: array, start: i}
	}
	cur.end = len(d.lines)
	d.tables = append(d.tables, cur)
	return d
}

// String renders the document — byte-identical to the input unless an edit touched a line.
func (d *Doc) String() string { return strings.Join(d.lines, "\n") }

// Tables returns the indices of every table with this dotted header path, in file order
// ("cluster" yields each [[cluster]] in turn). The root table's path is "".
func (d *Doc) Tables(path string) []int {
	var out []int
	for i, t := range d.tables {
		if t.header == path {
			out = append(out, i)
		}
	}
	return out
}

// Find returns the index of the table whose key holds value — how an array table is
// addressed by identity rather than position (the [[cluster]] named "erdc"), or -1.
func (d *Doc) Find(path, key, value string) int {
	for _, i := range d.Tables(path) {
		if v, ok := d.Value(i, key); ok && Unquote(v) == value {
			return i
		}
	}
	return -1
}

// Owner returns the index of the table that table i nests under — the [[cluster]] owning a
// [[cluster.node]] is just the nearest table above it with the parent path. -1 at top level.
func (d *Doc) Owner(i int) int {
	want, ok := parentPath(d.tables[i].header)
	if !ok {
		return -1
	}
	for j := i - 1; j >= 0; j-- {
		if d.tables[j].header == want {
			return j
		}
	}
	return -1
}

// Value returns the raw (still-quoted, comment-stripped) right-hand side of a key in table
// i, and whether the key is actually set there. A commented-out key is NOT set — it's a
// placeholder, which is what Set revives.
func (d *Doc) Value(i int, key string) (string, bool) {
	idx, live, ok := d.keyLine(i, key)
	if !ok || !live {
		return "", false
	}
	_, rhs, _ := strings.Cut(d.lines[idx], "=")
	val, _ := splitComment(rhs)
	return strings.TrimSpace(val), true
}

// Set assigns raw (a TOML value as it should appear — quotes and all) to key in table i.
// Three cases, in order: the key is live → rewrite its line, keeping the indent, the
// alignment padding around `=`, and any trailing comment; the key is a commented-out
// placeholder → revive that line in place, so the value lands where the schema said it
// would; the key is absent → append it to the end of the table's body.
func (d *Doc) Set(i int, key, raw string) {
	idx, live, ok := d.keyLine(i, key)
	if !ok {
		d.insert(i, key, raw)
		return
	}
	indent, pad, comment := d.lineParts(idx, key, live)
	line := indent + key + pad + "= " + raw
	if comment != "" {
		line += "   " + comment
	}
	d.lines[idx] = line
}

// insert appends "key = raw" to table i's body, after its last non-blank line — so the
// key lands inside the table rather than after the blank line that separates it from the
// next one. It inherits the indent of the table's header (node blocks are indented).
func (d *Doc) insert(i int, key, raw string) {
	t := d.tables[i]
	at := t.start + 1
	for j := t.start + 1; j < t.end; j++ {
		if strings.TrimSpace(d.lines[j]) != "" {
			at = j + 1
		}
	}
	indent := leadingSpace(d.lines[t.start])
	if t.header == "" && t.start == 0 && at == 1 && strings.TrimSpace(d.lines[0]) == "" {
		at = 0 // an empty root: put the key on the first line rather than after it
	}
	line := indent + key + " = " + raw
	d.lines = append(d.lines[:at], append([]string{line}, d.lines[at:]...)...)
	d.shift(at)
}

// InsertTable adds a new [[header]] array-table nested under owner (e.g. a [[cluster.node]]
// under its [[cluster]]), seeded with the given key/value lines, and returns its index for
// further Set calls. It lands after owner's existing child tables — before the blank that
// separates the cluster from the next — indented to match its siblings, and preceded by a
// blank line. raw values are inserted verbatim (quote strings yourself). The document is
// re-parsed, so indices at or after the insertion move; owner (which precedes it) is stable.
func (d *Doc) InsertTable(owner int, header string, seed [][2]string) int {
	at := d.insertPoint(owner)
	indent := d.childIndent(owner)
	block := []string{"", indent + "[[" + header + "]]"}
	for _, kv := range seed {
		block = append(block, indent+kv[0]+" = "+kv[1])
	}
	d.lines = append(d.lines[:at:at], append(block, d.lines[at:]...)...)
	*d = *Parse(d.String())
	// the new block is owner's child with the greatest start — the one just appended
	newIdx := -1
	for j := range d.tables {
		if d.Owner(j) == owner && d.tables[j].header == header &&
			(newIdx == -1 || d.tables[j].start > d.tables[newIdx].start) {
			newIdx = j
		}
	}
	return newIdx
}

// DeleteTable removes table i and its lines entirely — the node block dropped when a
// machine is decommissioned. A single blank line immediately above it goes too, so the
// removal doesn't leave a double gap. The root table is never deleted. Re-parses.
func (d *Doc) DeleteTable(i int) {
	t := d.tables[i]
	if t.header == "" {
		return
	}
	// the span already runs to the next header, so it carries the block's trailing blank —
	// removing [start,end) leaves the PREVIOUS block's blank as the separator.
	d.lines = append(d.lines[:t.start], d.lines[t.end:]...)
	// belt: if that seam now has two adjacent blanks (block had blanks on both sides and
	// no trailing one in-span), drop one.
	if t.start > 0 && t.start < len(d.lines) &&
		strings.TrimSpace(d.lines[t.start-1]) == "" && strings.TrimSpace(d.lines[t.start]) == "" {
		d.lines = append(d.lines[:t.start], d.lines[t.start+1:]...)
	}
	*d = *Parse(d.String())
}

// insertPoint is the line a new child table for owner should go at: past owner's own body
// and all its child tables, then backed up over trailing blank lines so the block joins the
// cluster's group rather than landing after the blank that precedes the next cluster.
func (d *Doc) insertPoint(owner int) int {
	end := d.tables[owner].end
	for j := range d.tables {
		if d.Owner(j) == owner && d.tables[j].end > end {
			end = d.tables[j].end
		}
	}
	for end > 0 && strings.TrimSpace(d.lines[end-1]) == "" {
		end--
	}
	return end
}

// childIndent is the indent a new child of owner should use: matched from an existing child
// if the cluster already has one, else two spaces (the house nesting for node blocks).
func (d *Doc) childIndent(owner int) string {
	for j := range d.tables {
		if d.Owner(j) == owner {
			return leadingSpace(d.lines[d.tables[j].start])
		}
	}
	return "  "
}

// shift moves every span at or below an inserted line down one, so the table index the
// caller holds keeps pointing at the same table after an edit.
func (d *Doc) shift(at int) {
	for i := range d.tables {
		if d.tables[i].start >= at {
			d.tables[i].start++
		}
		if d.tables[i].end >= at {
			d.tables[i].end++
		}
	}
}

// keyLine locates key within table i: its line, whether that line is a live assignment
// (false = a commented-out placeholder), and whether it was found at all. A live line wins
// over a placeholder, wherever each sits in the table.
func (d *Doc) keyLine(i int, key string) (idx int, live, ok bool) {
	t := d.tables[i]
	found, foundIdx := false, 0
	for j := t.start; j < t.end; j++ {
		l, isLive := assigns(d.lines[j], key)
		if !l {
			continue
		}
		if isLive {
			return j, true, true
		}
		if !found {
			found, foundIdx = true, j
		}
	}
	return foundIdx, false, found
}

// lineParts pulls the layout of an existing key line apart so Set can rebuild it in place:
// the indent, the padding that aligns `=` with its neighbours, and any trailing comment. A
// revived placeholder keeps its indent but drops the commented-out sample value.
func (d *Doc) lineParts(idx int, key string, live bool) (indent, pad, comment string) {
	line := d.lines[idx]
	indent = leadingSpace(line)
	if !live {
		return indent, " ", ""
	}
	lhs, rhs, _ := strings.Cut(line, "=")
	if _, c := splitComment(rhs); c != "" {
		comment = c
	}
	// Everything between the key and `=` is alignment padding, and is worth keeping: a
	// table whose `=` line up stops lining up the moment one value is rewritten.
	pad = lhs[strings.Index(lhs, key)+len(key):]
	if pad == "" {
		pad = " "
	}
	return indent, pad, comment
}

// assigns reports whether a line assigns key — as a live "key = …" or a commented-out
// "# key = …" placeholder. Matching on the key + `=` (not a bare prefix) keeps `account`
// from matching an `account_id` line.
func assigns(line, key string) (match, live bool) {
	s := strings.TrimSpace(line)
	live = true
	if strings.HasPrefix(s, "#") {
		live = false
		s = strings.TrimSpace(strings.TrimLeft(s, "#"))
	}
	rest, ok := strings.CutPrefix(s, key)
	if !ok {
		return false, false
	}
	return strings.HasPrefix(strings.TrimSpace(rest), "="), live
}

// splitComment cuts a value from its trailing "# …" comment, ignoring a '#' inside quotes
// (a path or a name may carry one).
func splitComment(s string) (value, comment string) {
	var quote byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
		case quote == 0 && c == '#':
			return s[:i], strings.TrimSpace(s[i:])
		}
	}
	return s, ""
}

// Unquote strips one layer of TOML basic/literal quotes; a bare value is returned as is.
func Unquote(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []string{`"`, `'`} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// Quote renders a Go string as a TOML basic string.
func Quote(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }

// leadingSpace returns a line's indent.
func leadingSpace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// parentPath drops a dotted header's last segment ("cluster.node" → "cluster"), or ok=false
// for a top-level header.
func parentPath(path string) (string, bool) {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return "", false
	}
	return path[:i], true
}

// header parses a "[table]" / "[[array]]" / "[a.b]" line into its dotted path.
func header(line string) (path string, array, ok bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") {
		return "", false, false
	}
	array = strings.HasPrefix(s, "[[")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "[["), "[")
	end := strings.IndexByte(s, ']')
	if end <= 0 {
		return "", false, false
	}
	return strings.TrimSpace(s[:end]), array, true
}

// Split walks TOML text and returns it with the named top-level tables removed (rest),
// plus each removed table's verbatim text (sections). A table runs from its header line to
// the next top-level header or EOF, so a subtable travels with its parent; the root
// (pre-header) lines are always kept. This is the whole-table half of the surgeon: `mu
// setup sync` uses it to drop the source's machine-local seams and splice the
// destination's back in, without either file being re-emitted.
func Split(text string, names ...string) (rest string, sections map[string]string) {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	sections = map[string]string{}
	var restB strings.Builder
	cur := "" // table being captured into sections; "" → the line goes to rest
	for _, line := range strings.Split(text, "\n") {
		if h, _, ok := header(line); ok {
			if root, _ := rootPath(h); want[root] {
				cur = root
			} else {
				cur = ""
			}
		}
		if cur != "" {
			sections[cur] += line + "\n"
		} else {
			restB.WriteString(line)
			restB.WriteByte('\n')
		}
	}
	return restB.String(), sections
}

// Assemble joins a Split body back with preserved sections, in the given order, each
// separated by a blank line. An empty section is skipped, so the output never claims a
// table the destination never had.
func Assemble(rest string, sections map[string]string, order ...string) string {
	out := strings.TrimRight(rest, "\n") + "\n"
	for _, name := range order {
		if s := strings.TrimRight(sections[name], "\n"); strings.TrimSpace(s) != "" {
			out += "\n" + s + "\n"
		}
	}
	return out
}

// rootPath is a dotted header's first segment ("cluster.node" → "cluster").
func rootPath(path string) (string, bool) {
	if i := strings.Index(path, "."); i >= 0 {
		return path[:i], true
	}
	return path, false
}
