package cli

import (
	"strings"
	"testing"
)

// sliceFor must pick the tool-specific slice: the LAST Python traceback, ctest's end
// summary, a compiler's error lines — and fall back to a tail when nothing matches.
func TestSliceFor(t *testing.T) {
	py := "loading\nTraceback (most recent call last):\n  old\nHandled\n" +
		"Traceback (most recent call last):\n  File \"prep.py\", line 3\nValueError: bad\n"
	got := sliceFor("auto", py)
	if got[0] != "Traceback (most recent call last):" || got[1] != "  File \"prep.py\", line 3" {
		t.Errorf("python slice must start at the LAST traceback: %q", got)
	}

	ctest := "Start 1: t_a\n1/2 Test #1: t_a ... Passed\n2/2 Test #2: t_b ... Failed\n" +
		"50% tests passed, 1 tests failed out of 2\n\nThe following tests FAILED:\n\t  2 - t_b (Failed)\n"
	got = sliceFor("auto", ctest)
	if got[0] != "The following tests FAILED:" {
		t.Errorf("ctest slice must start at the failed list: %q", got)
	}

	fort := "compiling a.f90\na.f90(12): error #5082: Syntax error\nnote: blah\n" +
		"b.f90:4:10: Error: Unexpected end of file\nmake: *** [all] Error 2\n"
	got = sliceFor("auto", fort)
	want := []string{
		"a.f90(12): error #5082: Syntax error",
		"b.f90:4:10: Error: Unexpected end of file",
		"make: *** [all] Error 2",
	}
	if len(got) != len(want) {
		t.Fatalf("compiler slice = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("compiler slice[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// No markers → tail; and "terror"/"errors" prose must not trip the compiler sniff.
	plain := "no such terror here\nmany errors of judgement\nlast line\n"
	got = sliceFor("auto", plain)
	if len(got) != 3 || got[2] != "last line" {
		t.Errorf("fallback tail wrong: %q", got)
	}

	// An explicit -t wins over the sniff.
	got = sliceFor("tail", py)
	if got[0] == "Traceback (most recent call last):" && len(got) < 5 {
		t.Errorf("explicit tail must not run the python extractor: %q", got)
	}
}

// capSlice keeps the END of an over-long slice behind an omission marker.
func TestCapSlice(t *testing.T) {
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, "l")
	}
	lines = append(lines, "verdict")
	got := capSlice(lines)
	if len(got) != sliceMax {
		t.Fatalf("capped to %d lines, want %d", len(got), sliceMax)
	}
	if !strings.Contains(got[0], "omitted") || got[len(got)-1] != "verdict" {
		t.Errorf("cap must keep the end behind a marker: first=%q last=%q", got[0], got[len(got)-1])
	}
}
