package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/render"
)

// sliceMax bounds the inline failure slice so render.Err never re-spills it (its own
// tail cap is 25) — the FULL output already lands in run-<id>.log.
const sliceMax = 25

// wrapCmd is `mu wrap`: run an external command (a Python script, a Fortran build, a
// test suite) behind the house UI. Output is captured; success is one OK line, and a
// failure shows the house error with the RELEVANT slice of the output — which is
// tool-specific, not a universal tail (a Python failure ends with its traceback, a
// compiler buries "error:" lines mid-spew, ctest puts the verdict in its end
// summary) — plus a pointer to the full log beside the event log.
func wrapCmd() *cobra.Command {
	var tool string
	var tee bool
	c := &cobra.Command{
		Use:   "wrap [-t <tool>] [--tee] -- <cmd> [args...]",
		Short: "Run a command behind the house UI: quiet OK, or the relevant failure slice.",
		Long: "Run an external command with its output captured. Success prints one OK line;\n" +
			"a failure prints the house error with the RELEVANT slice of the output — the\n" +
			"Python traceback, the compiler's error lines, or a test runner's end summary,\n" +
			"detected from the output (override with -t) — and writes the full output to a\n" +
			"run-<id>.log beside the event log. The command's exit code is passed through:\n" +
			"    mu wrap -- make check\n" +
			"    mu wrap -t python -- python prep.py --stage 2\n" +
			"    mu wrap --tee -- ctest --test-dir build",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if !validTool(tool) {
				return usageErr("unknown -t %q (auto|python|compiler|ctest|tail)", tool)
			}
			return runWrap(tool, tee, args)
		},
	}
	setHelpArgs(c, [2]string{"<cmd> [args...]", "the command to run, verbatim (put it after -- so its flags stay its own)"})
	c.Flags().StringVarP(&tool, "tool", "t", "auto", "failure-slice extractor: auto|python|compiler|ctest|tail")
	c.Flags().BoolVar(&tee, "tee", false, "stream output live while capturing (builds you want to watch)")
	return c
}

func validTool(t string) bool {
	switch t {
	case "auto", "python", "compiler", "ctest", "tail":
		return true
	}
	return false
}

func runWrap(tool string, tee bool, args []string) error {
	start := time.Now()
	cmd := exec.Command(args[0], args[1:]...)
	var buf bytes.Buffer
	if tee {
		cmd.Stdout = io.MultiWriter(os.Stdout, &buf)
		cmd.Stderr = io.MultiWriter(os.Stderr, &buf)
	} else {
		cmd.Stdout = &buf
		cmd.Stderr = &buf
	}
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	dur := time.Since(start).Round(100 * time.Millisecond)
	name := strings.Join(args, " ")
	if err == nil {
		render.OK(fmt.Sprintf("%s (%s)", name, dur))
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return runErr("%s: %v", args[0], err) // spawn failure — nothing ran, nothing to slice
	}
	rc := ee.ExitCode()
	out := buf.String()
	id, path := render.Spill("run", fmt.Sprintf("mu wrap %s\nrc %d after %s\n\n%s", name, rc, dur, out))
	msg := fmt.Sprintf("%s failed (rc %d, %s)", args[0], rc, dur)
	lines := append([]string{msg}, sliceFor(tool, out)...)
	if path != "" {
		lines = append(lines, "full output: "+path)
	}
	if !tee { // under --tee the output already scrolled by — just the verdict + pointer
		render.Err(strings.Join(lines, "\n"))
	} else {
		render.Err(msg + "\nfull output: " + path)
	}
	render.Emit("run", "error", msg, map[string]any{"id": id, "file": path, "cmd": name, "rc": rc})
	return codeErr(rc)
}

// Slice markers. compilerErrLine matches the gfortran/ifx/gcc/clang families
// ("error:", "Error:", "error #5082", make's "Error 2") without swallowing prose
// that merely contains the word.
var compilerErrLine = regexp.MustCompile(`(?i)(^|[ \t])error([ :#]|$)`)

// sliceFor extracts the relevant failure lines from a command's combined output.
// auto sniffs markers in priority order — traceback, then test-runner summary, then
// compiler error lines — and falls back to a plain tail.
func sliceFor(tool, out string) []string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if tool == "auto" {
		tool = sniffTool(out)
	}
	switch tool {
	case "python":
		// The LAST traceback block to the end — chained/re-raised tracebacks end
		// with the one that killed the process.
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.HasPrefix(lines[i], "Traceback (most recent call last") {
				return capSlice(lines[i:])
			}
		}
	case "ctest":
		// The end summary: from the failed-tests list when present, else the
		// "% tests passed" verdict line onward.
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.HasPrefix(lines[i], "The following tests FAILED") || strings.Contains(lines[i], "% tests passed") {
				return capSlice(lines[i:])
			}
		}
	case "compiler":
		var hits []string
		for _, l := range lines {
			if compilerErrLine.MatchString(l) {
				hits = append(hits, l)
			}
		}
		if len(hits) > 0 {
			return capSlice(hits)
		}
	}
	return capSlice(tailLines(lines, sliceMax))
}

// sniffTool detects the extractor from output markers; "" of them → tail.
func sniffTool(out string) string {
	switch {
	case strings.Contains(out, "Traceback (most recent call last"):
		return "python"
	case strings.Contains(out, "% tests passed"), strings.Contains(out, "The following tests FAILED"):
		return "ctest"
	case compilerErrLine.MatchString(out):
		return "compiler"
	}
	return "tail"
}

// capSlice bounds a slice to sliceMax lines, keeping the END (verdicts live there)
// behind an omission marker.
func capSlice(lines []string) []string {
	if len(lines) <= sliceMax {
		return lines
	}
	out := []string{fmt.Sprintf("… %d line(s) omitted", len(lines)-sliceMax+1)}
	return append(out, lines[len(lines)-sliceMax+1:]...)
}

func tailLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}
