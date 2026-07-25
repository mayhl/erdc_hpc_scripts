package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/cast"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// Default START/STOP markers: an `echo` of these lands in the cast as an output event and bounds
// the real content, so cropping to them drops the shell-start glitch and the exit tail (see
// ARCHITECTURE.md "Recording"). Overridable via --start/--stop.
const (
	defStartMark = "START_RECORDING"
	defStopMark  = "STOP_RECORDING"
)

func castCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cast",
		Short: "Build terminal-demo recordings (asciicast → svg).",
		Long: "Turn asciinema recordings into shareable svgs. `build` runs the fixed post-\n" +
			"processing pipeline (crop → idle-limit → concat → svg) on one or more raw casts.\n" +
			"Capture (auto/live) lands later; for now record with asciinema, then `mu cast build`.",
	}
	c.AddCommand(castBuildCmd())
	return c
}

type castBuildOpts struct {
	out   string
	idle  float64
	gap   float64
	start string
	stop  string
	noSVG bool
}

func castBuildCmd() *cobra.Command {
	var o castBuildOpts
	cmd := &cobra.Command{
		Use:   "build <cast>...",
		Short: "Clean raw casts and render an svg (crop → idle-limit → concat → svg).",
		Long: "Post-process one or more raw asciicast v3 recordings into a single svg: crop to the\n" +
			"START/STOP markers, clamp idle gaps, stitch the parts in order, convert to asciicast\n" +
			"v2 (svg-term reads v2 only), and render. A <cast> may be named with or without the\n" +
			"'.cast' suffix. Writes <out>.v2.cast (the cleaned svg source) and <out>.svg.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runCastBuild(args, o)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.out, "out", "o", "", "output base name (default: the first input's name)")
	f.Float64Var(&o.idle, "idle", 2.0, "clamp any idle gap to at most this many `seconds`")
	f.Float64Var(&o.gap, "gap", 1.0, "`seconds` of pause inserted between concatenated parts")
	f.StringVar(&o.start, "start", defStartMark, "`marker` to crop the head to")
	f.StringVar(&o.stop, "stop", defStopMark, "`marker` to crop the tail to")
	f.BoolVar(&o.noSVG, "no-svg", false, "stop at the cleaned v2 cast; skip the svg render")
	setHelpArgs(cmd, [2]string{"<cast>...", "one or more raw .cast files, concatenated in order"})
	return cmd
}

func runCastBuild(inputs []string, o castBuildOpts) error {
	parts := make([]*cast.Cast, 0, len(inputs))
	for _, in := range inputs {
		path := in
		if !strings.HasSuffix(path, ".cast") {
			path += ".cast"
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		c, err := cast.Parse(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if v := c.Version(); v != 3 {
			return fmt.Errorf("%s is asciicast v%d; the pipeline needs v3 (record under asciinema 3, or `asciinema convert -f asciicast-v3 %s out.cast`)", path, v, path)
		}
		name := filepath.Base(path)
		head, tail := c.Crop(o.start, o.stop)
		if stripped := c.TrimExit(); stripped > 0 || tail == 0 {
			render.Warn(fmt.Sprintf("%s: dirty tail (no %s marker or exit junk) — trimmed %d + %d trailing events",
				name, o.stop, tail, stripped))
		}
		c.IdleLimit(o.idle)
		render.Info(fmt.Sprintf("%s: cropped %d head events → %.1fs", name, head, c.Duration()))
		parts = append(parts, c)
	}

	merged := parts[0]
	if len(parts) > 1 {
		merged = cast.Concat(o.gap, parts...)
	}

	out := o.out
	if out == "" {
		out = strings.TrimSuffix(filepath.Base(inputs[0]), ".cast")
	}

	v2, err := merged.ToV2()
	if err != nil {
		return fmt.Errorf("to v2: %w", err)
	}
	v2Path := out + ".v2.cast"
	if err := writeCast(v2Path, v2); err != nil {
		return err
	}
	render.OK(fmt.Sprintf("wrote %s (%.1fs, %d events)", v2Path, merged.Duration(), len(v2.Events)))

	if o.noSVG {
		return nil
	}
	return renderSVG(v2Path, out+".svg")
}

func writeCast(path string, c *cast.Cast) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return c.Write(f)
}

// renderSVG pipes the cleaned v2 cast through svg-term-cli (via npx). svg-term reads asciicast
// v2 only — that is why ToV2 runs first. A missing npx is a SOFT failure: the cleaned cast still
// stands, so you can render later or with --no-svg on the next run.
func renderSVG(v2Path, svgPath string) error {
	npx, err := exec.LookPath("npx")
	if err != nil {
		render.Warn("npx not found — kept the cleaned cast, skipped the svg (install node, or pass --no-svg)")
		return nil
	}
	in, err := os.Open(v2Path)
	if err != nil {
		return err
	}
	defer in.Close()
	cmd := exec.Command(npx, "svg-term-cli", "--out", svgPath, "--window")
	cmd.Stdin = in
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("svg-term: %w", err)
	}
	render.OK("wrote " + svgPath)
	return nil
}
