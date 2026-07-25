package cast

import (
	"strings"
	"testing"
)

// a minimal v3 fixture: header + start-marker echo + two commands + a stray exit event.
const v3 = `{"version":3,"term":{"cols":80,"rows":24},"timestamp":1785001281,"env":{"SHELL":"/bin/zsh"}}
[0.10, "o", "glitch\r\n"]
[0.05, "o", "START_RECORDING\r\n"]
[5.00, "o", "first\r\n"]
[0.30, "o", "second\r\n"]
[0.20, "o", "STOP_RECORDING\r\n"]
[0.01, "x", "0"]
`

func parse(t *testing.T, s string) *Cast {
	t.Helper()
	c, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

// round-trip: parse then Write reproduces the events (numbers may re-render, so compare via
// a re-parse rather than byte-equality).
func TestRoundTrip(t *testing.T) {
	c := parse(t, v3)
	if len(c.Events) != 6 {
		t.Fatalf("events = %d, want 6", len(c.Events))
	}
	var b strings.Builder
	if err := c.Write(&b); err != nil {
		t.Fatalf("Write: %v", err)
	}
	c2 := parse(t, b.String())
	if len(c2.Events) != len(c.Events) || c2.Events[3].Data != "second\r\n" {
		t.Fatalf("round-trip mismatch: %+v", c2.Events)
	}
}

func TestIdleLimitAndOffset(t *testing.T) {
	c := parse(t, v3)
	c.IdleLimit(2.0) // the 5.00 gap clamps to 2.0; others untouched
	if c.Events[2].Interval != 2.0 || c.Events[3].Interval != 0.30 {
		t.Fatalf("idle-limit: %v, %v", c.Events[2].Interval, c.Events[3].Interval)
	}
	c.Offset(1.5) // leading pause onto the first event only
	if c.Events[0].Interval != 0.10+1.5 {
		t.Fatalf("offset: %v", c.Events[0].Interval)
	}
}

func TestCropToMarkers(t *testing.T) {
	c := parse(t, v3)
	head, tail := c.Crop("START_RECORDING", "STOP_RECORDING")
	if head != 2 || tail != 2 { // drop [glitch, START] and [STOP, exit]
		t.Fatalf("crop counts: head=%d tail=%d", head, tail)
	}
	if len(c.Events) != 2 || c.Events[0].Data != "first\r\n" || c.Events[1].Data != "second\r\n" {
		t.Fatalf("cropped events: %+v", c.Events)
	}
	if c.Events[0].Interval != 0 { // first survivor re-zeroed
		t.Fatalf("first interval not re-zeroed: %v", c.Events[0].Interval)
	}
}

// with no STOP marker, Crop leaves the tail; TrimExit removes the stray exit event.
func TestTrimExitOnDirtyTail(t *testing.T) {
	c := parse(t, v3)
	c.Crop("START_RECORDING", "NO_SUCH_STOP")
	// head dropped (2), tail kept: first, second, STOP echo, exit event
	if stripped := c.TrimExit(); stripped != 1 {
		t.Fatalf("TrimExit stripped %d, want 1 (the exit event)", stripped)
	}
	if c.Events[len(c.Events)-1].Code == "x" {
		t.Fatal("exit event survived TrimExit")
	}
}

func TestConcat(t *testing.T) {
	a := parse(t, v3)
	b := parse(t, v3)
	out := Concat(3.0, a, b)
	if len(out.Events) != 12 {
		t.Fatalf("concat events = %d, want 12", len(out.Events))
	}
	// second part's first event carries the +3.0 inter-part gap
	if got := out.Events[6].Interval; got != 0.10+3.0 {
		t.Fatalf("inter-part gap: %v, want 3.10", got)
	}
	// inputs not mutated
	if a.Events[0].Interval != 0.10 {
		t.Fatalf("Concat mutated input a: %v", a.Events[0].Interval)
	}
}

func TestToV2(t *testing.T) {
	c := parse(t, v3)
	v2, err := c.ToV2()
	if err != nil {
		t.Fatalf("ToV2: %v", err)
	}
	h := string(v2.Header)
	if !strings.Contains(h, `"version":2`) || !strings.Contains(h, `"width":80`) || !strings.Contains(h, `"height":24`) {
		t.Fatalf("v2 header: %s", h)
	}
	if strings.Contains(h, `"term"`) {
		t.Fatalf("v2 header still has term: %s", h)
	}
	// exit event dropped; times are now absolute (monotonic non-decreasing)
	if len(v2.Events) != 5 {
		t.Fatalf("v2 events = %d, want 5 (exit dropped)", len(v2.Events))
	}
	last := -1.0
	for _, e := range v2.Events {
		if e.Interval < last {
			t.Fatalf("v2 times not absolute/monotonic: %v after %v", e.Interval, last)
		}
		last = e.Interval
	}
	// 0.10 + 0.05 + 5.00 + 0.30 + 0.20 = 5.65 at the last kept event
	if got := v2.Events[4].Interval; got < 5.64 || got > 5.66 {
		t.Fatalf("last absolute time = %v, want ~5.65", got)
	}
}
