// Package cast reads and edits asciinema recordings (asciicast v3), the build half of the
// recording pipeline (see ARCHITECTURE.md "Recording"). v3 event times are INTERVALS — the
// gap since the previous event, not an absolute clock — so every timing edit is a per-interval
// transform, which is what lets this replace asciinema-editor.py in ~a page of Go. The only
// v2 touchpoint is ToV2, run last so svg-term-cli (v1/v2 only) can render the result.
//
// Format: https://docs.asciinema.org/manual/asciicast/v3/
package cast

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
)

// Cast is a header line plus newline-delimited event tuples. The header stays raw JSON so
// forward/unknown fields (theme, idle_time_limit, env, …) round-trip untouched — we only ever
// rewrite the events.
type Cast struct {
	Header json.RawMessage
	Events []Event
}

// Event is one v3 tuple [interval, code, data]. Code: "o" output, "i" input, "m" marker,
// "r" resize, "x" exit — treated uniformly for timing.
type Event struct {
	Interval float64
	Code     string
	Data     string
}

func (e *Event) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if len(raw) != 3 {
		return fmt.Errorf("event: want 3 fields, got %d", len(raw))
	}
	if err := json.Unmarshal(raw[0], &e.Interval); err != nil {
		return err
	}
	if err := json.Unmarshal(raw[1], &e.Code); err != nil {
		return err
	}
	return json.Unmarshal(raw[2], &e.Data)
}

func (e Event) MarshalJSON() ([]byte, error) {
	// round to microseconds (asciinema's own resolution) so accumulated deltas don't render
	// as 6.300000000000001; json.Marshal then emits the shortest form (6.3, or 2 for 2.0).
	iv, err := json.Marshal(math.Round(e.Interval*1e6) / 1e6)
	if err != nil {
		return nil, err
	}
	code, _ := json.Marshal(e.Code)
	data, _ := json.Marshal(e.Data)
	return []byte("[" + string(iv) + ", " + string(code) + ", " + string(data) + "]"), nil
}

// Parse reads a v3 cast: first line = header, rest = events (blank lines skipped).
func Parse(r io.Reader) (*Cast, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20) // a full-screen clear event can be long
	if !sc.Scan() {
		return nil, fmt.Errorf("cast: empty (no header)")
	}
	c := &Cast{Header: append(json.RawMessage(nil), bytes.TrimSpace(sc.Bytes())...)}
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("cast: bad event %q: %w", line, err)
		}
		c.Events = append(c.Events, e)
	}
	return c, sc.Err()
}

// Write emits the cast: header line, then one event per line.
func (c *Cast) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)
	bw.Write(c.Header)
	bw.WriteByte('\n')
	for _, e := range c.Events {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		bw.Write(b)
		bw.WriteByte('\n')
	}
	return bw.Flush()
}

// Version reports the asciicast version from the header (2 or 3; 0 if unparseable). The timing
// ops here assume v3 delta semantics, so callers should reject a non-3 cast rather than silently
// mangle an absolute-time v2 recording.
func (c *Cast) Version() int {
	var h struct {
		Version int `json:"version"`
	}
	json.Unmarshal(c.Header, &h)
	return h.Version
}

// Duration is the wall-clock length (sum of gaps).
func (c *Cast) Duration() float64 {
	var t float64
	for _, e := range c.Events {
		t += e.Interval
	}
	return t
}

// ── timing ops (delta-native) ──────────────────────────────────────────────────────────────

// IdleLimit clamps every gap to at most limit seconds. Mirrors asciinema-editor.py
// limit-idle-time / asciinema-edit quantize / native `asciinema rec -i`.
func (c *Cast) IdleLimit(limit float64) {
	for i := range c.Events {
		if c.Events[i].Interval > limit {
			c.Events[i].Interval = limit
		}
	}
}

// Offset prepends a leading pause of d seconds (v3: bump the first event's delta). The Python
// add-offset added to every absolute timestamp; in v3 that collapses to the first interval.
func (c *Cast) Offset(d float64) {
	if len(c.Events) > 0 {
		c.Events[0].Interval += d
	}
}

// Speed scales every gap (>1 slower, <1 faster). Mirrors asciinema-edit speed (whole-cast).
func (c *Cast) Speed(factor float64) {
	for i := range c.Events {
		c.Events[i].Interval *= factor
	}
}

// Concat stitches parts end-to-end, inserting gap seconds before each part after the first —
// the multi-part concat asciinema-editor.py never got, and gap control `asciinema cat` lacks.
// v3 makes it an append: the inter-part pause rides the next part's first interval. Inputs are
// not mutated; the result inherits the first part's header.
func Concat(gap float64, parts ...*Cast) *Cast {
	out := &Cast{}
	for i, p := range parts {
		if i == 0 {
			out.Header = p.Header
		}
		ev := append([]Event(nil), p.Events...)
		if i > 0 && len(ev) > 0 {
			ev[0].Interval += gap
		}
		out.Events = append(out.Events, ev...)
	}
	return out
}

// ── boundary hygiene ───────────────────────────────────────────────────────────────────────

// Crop trims the recording to its START/STOP markers (see ARCHITECTURE.md "Recording" check
// a). A marker is an echo whose output lands as an event: everything up to & INCLUDING the
// last event containing startMark is dropped (killing the shell-start / instant-prompt glitch),
// and everything from the first event containing stopMark onward is dropped (killing the exit
// tail). The first survivor's interval is re-zeroed so playback starts immediately. A missing
// marker leaves that side untrimmed — the caller then leans on TrimExit + a warning. Returns
// how many events were dropped off the head and the tail.
func (c *Cast) Crop(startMark, stopMark string) (head, tail int) {
	start := -1
	for i, e := range c.Events {
		if strings.Contains(e.Data, startMark) {
			start = i // last match wins: the typed command may echo the marker before its output
		}
	}
	stop := len(c.Events)
	for i := start + 1; i < len(c.Events); i++ {
		if strings.Contains(c.Events[i].Data, stopMark) {
			stop = i
			break
		}
	}
	head = start + 1 // start==-1 (not found) → head 0, no head trim
	tail = len(c.Events) - stop
	c.Events = c.Events[head:stop]
	if len(c.Events) > 0 {
		c.Events[0].Interval = 0
	}
	return head, tail
}

// TrimExit strips trailing exit artifacts — a v3 exit event [_, "x", _] and trailing events
// whose visible output is just an EOT (^D), a "logout", or empty control noise. It is the
// escape / bad-exit check (ARCHITECTURE.md check b): run it after Crop so a fumbled live exit
// (no STOP marker) still yields a clean svg. Returns the count removed so the caller can warn.
func (c *Cast) TrimExit() (stripped int) {
	n := len(c.Events)
	for n > 0 && isExitArtifact(c.Events[n-1]) {
		n--
	}
	stripped = len(c.Events) - n
	c.Events = c.Events[:n]
	return stripped
}

func isExitArtifact(e Event) bool {
	if e.Code == "x" { // v3 exit event
		return true
	}
	if e.Code != "o" {
		return false
	}
	s := e.Data
	if strings.ContainsRune(s, '\x04') { // ^D
		return true
	}
	t := strings.TrimSpace(s)
	return t == "" || t == "^D" || t == "logout"
}

// ── output ─────────────────────────────────────────────────────────────────────────────────

// ToV2 rewrites to asciicast v2 — absolute event times and header version 2 — the only format
// svg-term-cli reads. It returns a NEW Cast (the v3 original stays intact for re-processing):
// the header's `version` flips to 2 and v3's `term:{cols,rows}` becomes v2's top-level
// `width`/`height`; the events' deltas accumulate into absolute times. Run it last.
func (c *Cast) ToV2() (*Cast, error) {
	var h map[string]json.RawMessage
	if err := json.Unmarshal(c.Header, &h); err != nil {
		return nil, fmt.Errorf("cast: header: %w", err)
	}
	h["version"] = json.RawMessage("2")
	if term, ok := h["term"]; ok {
		var t struct {
			Cols int `json:"cols"`
			Rows int `json:"rows"`
		}
		if json.Unmarshal(term, &t) == nil {
			wb, _ := json.Marshal(t.Cols)
			hb, _ := json.Marshal(t.Rows)
			h["width"], h["height"] = wb, hb
		}
		delete(h, "term")
	}
	nh, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	out := &Cast{Header: nh, Events: make([]Event, 0, len(c.Events))}
	var t float64
	for _, e := range c.Events {
		t += e.Interval
		if e.Code == "x" { // v2 has no exit event
			continue
		}
		out.Events = append(out.Events, Event{Interval: t, Code: e.Code, Data: e.Data}) // Interval now holds absolute time
	}
	return out, nil
}
