package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/render"
)

func logCmd() *cobra.Command {
	var tier, scope, since string
	var lines int
	var all, interactive, jsonOut bool
	c := &cobra.Command{
		Use:   "log",
		Short: "View the event log (transfers, jobs, big ops).",
		Long: "Show mu's event log, newest last, grouped by day. Defaults to the last 50\n" +
			"events (-n to change, --all for the whole log). -i opens a live, scrollable,\n" +
			"filterable viewer; --json emits the events as NDJSON. Subcommands: `write`, `clear`.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if interactive {
				return interactiveLog(tier, scope, since)
			}
			return viewLog(tier, scope, since, lines, all, jsonOut)
		},
	}
	f := c.Flags()
	f.StringVarP(&tier, "tier", "t", "", "only this tier (info|ok|warn|error)")
	f.StringVarP(&scope, "scope", "s", "", "only this scope (cp|hpc|job|…)")
	f.StringVar(&since, "since", "", "only newer than a duration (2h, 3d) or date (2026-07-01)")
	f.IntVarP(&lines, "lines", "n", 50, "show only the last N events (0 = all)")
	f.BoolVar(&all, "all", false, "show the entire log (overrides -n)")
	f.BoolVarP(&interactive, "interactive", "i", false, "browse in a live, scrollable, filterable viewer")
	f.BoolVar(&jsonOut, "json", false, "emit events as NDJSON (one record per line, payloads inline)")
	c.AddCommand(logWriteCmd(), logClearCmd())
	setHelpShortcuts(c, [2]string{"mlog", "view the event log"})
	return c
}

func logWriteCmd() *cobra.Command {
	var scope, payloadJSON string
	c := &cobra.Command{
		Use:   "write <level> <msg>",
		Short: "Append an event to the log (for external scripts).",
		Long: "Append an event. --payload attaches a JSON object as a structured payload\n" +
			"(stored inline on the line); the assigned event id is printed to stdout.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			var payload map[string]any
			if strings.TrimSpace(payloadJSON) != "" {
				if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
					return fmt.Errorf("--payload must be a JSON object: %w", err)
				}
			}
			id := render.WriteEvent(scope, args[0], strings.Join(args[1:], " "), payload)
			if id != "" {
				fmt.Println(id)
			}
			return nil
		},
	}
	setHelpArgs(c,
		[2]string{"<level>", "event level: info, ok, warn, or error"},
		[2]string{"<msg>", "event text (remaining args are joined)"})
	c.Flags().StringVarP(&scope, "scope", "s", "ext", "event scope tag")
	c.Flags().StringVar(&payloadJSON, "payload", "", "attach a JSON object as a structured payload")
	return c
}

func logClearCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "clear",
		Short: "Truncate the event log.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := render.EventLogPath()
			if !yes {
				if !confirm("clear %s?", path) {
					render.Info("aborted")
					return nil
				}
			}
			if err := os.Truncate(path, 0); err != nil && !os.IsNotExist(err) {
				return err
			}
			render.OK("event log cleared")
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation")
	return c
}

// --- reader ---

type logEntry struct {
	t          time.Time
	rawTS      string
	level      string
	scope, msg string
	payload    string // raw JSON suffix, "" when none
}

var (
	leadBracketRE = regexp.MustCompile(`^\[([^\]]*)\]\s*`)
	scopeTokenRE  = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// parseLogLine tolerantly parses `[ts] [level] [scope] msg` (Go) and the older
// 2-field `[ts] [level] msg`: grabs up to three leading [..] groups; the third is
// scope only if it looks like a scope token, else it's part of the message.
func parseLogLine(line string) (logEntry, bool) {
	rest := line
	var fields []string
	for len(fields) < 3 {
		m := leadBracketRE.FindStringSubmatch(rest)
		if m == nil {
			break
		}
		fields = append(fields, strings.TrimSpace(m[1]))
		rest = rest[len(m[0]):]
	}
	if len(fields) < 2 {
		return logEntry{}, false
	}
	e := logEntry{rawTS: fields[0], level: strings.ToUpper(fields[1])}
	switch {
	case len(fields) == 3 && scopeTokenRE.MatchString(fields[2]):
		e.scope, e.msg = fields[2], rest
	case len(fields) == 3:
		e.msg = "[" + fields[2] + "] " + rest // third bracket was part of the message
	default:
		e.msg = rest
	}
	// Optional structured payload: a trailing tab-delimited JSON suffix (…msg\t{json}).
	if i := strings.IndexByte(e.msg, '\t'); i >= 0 {
		e.payload = strings.TrimSpace(e.msg[i+1:])
		e.msg = e.msg[:i]
	}
	if ts, err := time.ParseInLocation("2006-01-02T15:04:05", e.rawTS, time.Local); err == nil {
		e.t = ts
	}
	return e, true
}

func viewLog(tier, scope, since string, limit int, all, jsonOut bool) error {
	tierU := strings.ToUpper(strings.TrimSpace(tier))
	cutoff, err := sinceCutoff(since)
	if err != nil {
		return err
	}
	rows, err := readLog(tierU, scope, cutoff)
	if err != nil {
		if os.IsNotExist(err) {
			if jsonOut { // machine-readable: no log yet ⇒ empty output, not a notice
				return nil
			}
			render.Info("no events yet (" + render.EventLogPath() + ")")
			return nil
		}
		return err
	}

	total := len(rows)
	if total == 0 {
		if !jsonOut {
			render.Info("no matching events")
		}
		return nil
	}
	title := fmt.Sprintf("Event log · %d event%s", total, plural(total))
	if !all && limit > 0 && total > limit {
		rows = rows[total-limit:]
		title = fmt.Sprintf("Event log · last %d of %d", limit, total)
	}
	if jsonOut {
		return emitLogJSON(rows)
	}
	fmt.Println(render.LogBlock(title, rows))
	return nil
}

// logJSONRecord is one `mu log --json` NDJSON record — a machine-readable mirror of a
// LogRow. Payload embeds the stored JSON object verbatim (omitted when absent or, as a
// guard, unparseable); the event id lives inside it under "id".
type logJSONRecord struct {
	TS      string          `json:"ts"`
	Level   string          `json:"level"`
	Scope   string          `json:"scope,omitempty"`
	Msg     string          `json:"msg"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// emitLogJSON writes rows as NDJSON (one object per line) to stdout — the append-only
// log's natural machine form, jq/stream-friendly. Encoder.Encode adds each line's newline.
func emitLogJSON(rows []render.LogRow) error {
	w := bufio.NewWriter(os.Stdout)
	defer func() { _ = w.Flush() }()
	enc := json.NewEncoder(w)
	for _, r := range rows {
		rec := logJSONRecord{TS: r.RawTS, Level: r.Level, Scope: r.Scope, Msg: r.Msg}
		if r.Payload != "" && json.Valid([]byte(r.Payload)) {
			rec.Payload = json.RawMessage(r.Payload)
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}

// readLog reads and filters the event log into rows (oldest first). tierU must be
// already upper-cased; cutoff zero means no since-bound. A missing log surfaces
// os.ErrNotExist so callers can distinguish "no log yet" from "no matches".
func readLog(tierU, scope string, cutoff time.Time) ([]render.LogRow, error) {
	render.EnsureEventLog() // follow a legacy ~/.cache log to the state dir
	f, err := os.Open(render.EventLogPath())
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var rows []render.LogRow
	for sc.Scan() {
		e, ok := parseLogLine(sc.Text())
		if !ok {
			continue
		}
		if tierU != "" && !tierMatch(tierU, e.level) {
			continue
		}
		if scope != "" && !strings.EqualFold(scope, e.scope) {
			continue
		}
		if !cutoff.IsZero() && !e.t.IsZero() && e.t.Before(cutoff) {
			continue
		}
		rows = append(rows, render.LogRow{Time: e.t, RawTS: e.rawTS, Level: e.level, Scope: e.scope, Msg: e.msg, Payload: e.payload})
	}
	return rows, sc.Err()
}

// sinceCutoff resolves a --since string to a cutoff time (zero when empty).
func sinceCutoff(since string) (time.Time, error) {
	if since == "" {
		return time.Time{}, nil
	}
	return parseSince(since)
}

// interactiveLog opens the read-only viewer (`mu log -i`): a live, scrollable,
// filterable flat table over the same tier/scope/since filters. Re-reads on the
// widget's refresh tick so new events tail in.
func interactiveLog(tier, scope, since string) error {
	if !render.Interactive() {
		return fmt.Errorf("mu log -i needs a terminal (stdin is not a tty)")
	}
	tierU := strings.ToUpper(strings.TrimSpace(tier))
	cutoff, err := sinceCutoff(since)
	if err != nil {
		return err
	}
	initial, err := readLog(tierU, scope, cutoff)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(initial) == 0 {
		render.Info("no matching events")
		return nil
	}
	// rowByID lets the `i` inspect overlay recover a row's full detail (untruncated
	// message + payload) from the SelectRow id. Fetch (refresh tick) and Detail (inspect)
	// run on separate bubbletea command goroutines, so the map is swapped whole under mu.
	var mu sync.Mutex
	rowByID := map[string]render.LogRow{}
	return render.Viewer(render.SelectSpec{
		Title:    "Event log",
		Columns:  []string{"TIME", "LVL", "SCOPE", "MESSAGE"},
		Interval: 2 * time.Second,
		Fetch: func() []render.SelectRow {
			rows, _ := readLog(tierU, scope, cutoff) // tolerate a blip; keep the last frame
			sel := logSelectRows(rows)
			next := make(map[string]render.LogRow, len(rows))
			for i, r := range rows {
				next[sel[i].ID] = r
			}
			mu.Lock()
			rowByID = next
			mu.Unlock()
			return sel
		},
		Detail: func(id string) string {
			mu.Lock()
			r, ok := rowByID[id]
			mu.Unlock()
			if !ok {
				return ""
			}
			return logDetailCard(r)
		},
	})
}

// logDetailCard renders one event as the house KV card for the `i` inspect overlay: the
// row's own fields untruncated, plus the pretty-printed JSON payload when the event
// carries one. Level shows its tier glyph + name in the tier hue; the payload is dropped
// when empty (an over-long message alone still justifies the card).
func logDetailCard(r render.LogRow) string {
	ts := r.RawTS
	if !r.Time.IsZero() {
		ts = r.Time.Format("2006-01-02 15:04:05")
	}
	g, hue := logLevelGlyphHue(r.Level)
	lvl := r.Level
	if lvl == "" {
		lvl = "INFO"
	}
	fields := []render.KVField{
		{Label: "Time", Value: ts, Hue: render.HueLoc},
		{Label: "Level", Value: g + " " + lvl, Hue: hue},
		{Label: "Scope", Value: r.Scope, Hue: render.HueUser},
		{Label: "Message", Value: r.Msg},
	}
	if r.Payload != "" {
		if pf := payloadFields(r.Payload); len(pf) > 0 {
			fields = append(fields, pf...) // unwrap {k:v} into first-class rows
		} else {
			fields = append(fields, render.KVField{Label: "Payload", Value: prettyJSON(r.Payload)}) // non-object: raw
		}
	}
	return render.KVCard("Event", fields)
}

// payloadFields unwraps a JSON-object payload into one card row per key (id hoisted to
// the top as the correlation key), preserving the stored key order. Values keep their
// exact form (json.Number → no float noise; nested object/array → compact JSON). Returns
// nil when the payload isn't a JSON object, so the caller shows the raw blob instead.
func payloadFields(raw string) []render.KVField {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil {
		return nil
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil
	}
	var idField *render.KVField
	var rest []render.KVField
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, _ := keyTok.(string)
		var val any
		if err := dec.Decode(&val); err != nil {
			return nil
		}
		f := render.KVField{Label: payloadLabel(key), Value: formatPayloadValue(val)}
		if key == "id" {
			idField = &f
			continue
		}
		rest = append(rest, f)
	}
	if idField != nil {
		return append([]render.KVField{*idField}, rest...)
	}
	return rest
}

// payloadLabel titles a payload key for its card row — the well-known "id" acronym is
// upper-cased (matching the other cards); any other key shows verbatim.
func payloadLabel(key string) string {
	if key == "id" {
		return "ID"
	}
	return key
}

// formatPayloadValue renders a decoded payload value as a card cell: strings/numbers/
// bools verbatim (json.Number keeps ints exact — no 1.04e+07), a nested object/array as
// compact JSON.
func formatPayloadValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", t)
	}
}

// prettyJSON indents a raw JSON payload for the inspect card's fallback row (used when
// the payload isn't a JSON object); a malformed value passes through verbatim.
func prettyJSON(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return raw
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(b)
}

// logSelectRows adapts log rows into viewer rows: TIME (date+time — the flat view
// has no day headers) · LVL glyph · SCOPE · MESSAGE. The row ID is the stamp+message
// so the cursor holds its place across a live refresh. Hues: time blue, glyph
// tier-colored, scope magenta, message default (matches the static view). A payloaded
// event leads its message with a ⊕ marker (a fixed 2-col slot so the column stays
// aligned) — it flags which rows reward `i`, and leading survives the right-side crop.
func logSelectRows(rows []render.LogRow) []render.SelectRow {
	out := make([]render.SelectRow, len(rows))
	for i, r := range rows {
		tm := r.RawTS
		if !r.Time.IsZero() {
			tm = r.Time.Format("01-02 15:04:05")
		}
		g, hue := logLevelGlyphHue(r.Level)
		mark := "  "
		if r.Payload != "" {
			mark = "⊕ "
		}
		out[i] = render.SelectRow{
			ID:    tm + " " + r.Msg,
			Cells: []string{tm, g, r.Scope, mark + r.Msg},
			Hues:  []string{render.HueLoc, hue, render.HueUser, ""},
		}
	}
	return out
}

// logLevelGlyphHue maps a level to its glyph and house hue key for the viewer's LVL
// cell (the glyph carries the level; hue is status-reserved green/yellow/red/cyan).
func logLevelGlyphHue(level string) (glyph, hue string) {
	switch strings.ToUpper(level) {
	case "OK":
		return "✓", render.HueOK
	case "WARN", "WARNING":
		return "!", render.HueWarn
	case "ERROR", "ERR":
		return "✗", render.HueErr
	default:
		return "→", render.HueID
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// tierMatch compares tiers, accepting warn/warning and err/error.
func tierMatch(want, have string) bool {
	norm := func(s string) string {
		switch s {
		case "WARNING":
			return "WARN"
		case "ERR":
			return "ERROR"
		}
		return s
	}
	return norm(want) == norm(have)
}

func parseSince(s string) (time.Time, error) {
	if d, ok := parseDur(s); ok {
		return time.Now().Add(-d), nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad --since %q (use 2h/3d or 2026-07-01)", s)
}

// parseDur extends time.ParseDuration with a 'd' (days) suffix.
func parseDur(s string) (time.Duration, bool) {
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Duration(n) * 24 * time.Hour, true
		}
		return 0, false
	}
	d, err := time.ParseDuration(s)
	return d, err == nil
}
