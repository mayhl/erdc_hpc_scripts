package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
		Use:   "cast [<script.sh>... | <name>]",
		Short: "Record and build terminal-demo recordings (asciicast → svg).",
		Long: "Capture a terminal demo and post-process it into a shareable svg. The argument's\n" +
			"suffix picks the capture mode: '.sh' scripts are auto-typed by asciinema-automation\n" +
			"(workflow 1; several record in order — `mu cast demo/*.sh` — then build into one svg\n" +
			"automatically), a bare <name> starts a live `asciinema rec` you drive by hand\n" +
			"(workflow 2), built afterward with `mu cast build <name>...`. Bound the real content\n" +
			"by echoing START_RECORDING / STOP_RECORDING.",
		Args: cobra.ArbitraryArgs,
	}
	// `#$ expect` and the final shell-exit wait share asciinema-automation's global timeout;
	// a slow-network clone can outlive the 30s default, so expose it (auto mode only).
	timeout := c.Flags().Int("timeout", 0, "auto mode: `seconds` asciinema-automation waits on an expect/exit (its -t; 0 = its 30s default)")
	c.RunE = func(cmd *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return cmd.Help()
		case strings.HasSuffix(args[0], ".sh"):
			return runCastAutoAll(args, *timeout) // workflow 1: scripted
		case len(args) > 1:
			return fmt.Errorf("live mode takes one <name>; globs are for '.sh' scripts")
		default:
			return runCastLive(args[0]) // workflow 2: live
		}
	}
	c.AddCommand(castBuildCmd())
	return c
}

// recordExec runs an interactive recorder (asciinema / asciinema-automation) with _RECORD_FLAG
// set so the shell it spawns applies the clean record profile (p10k theme, no ambient plugins),
// inheriting the terminal so you see and drive the session. extraPath, when non-empty, is
// prepended to PATH (the auto-mode banner shim).
func recordExec(bin, extraPath string, args ...string) error {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "_RECORD_FLAG=1")
	if extraPath != "" {
		cmd.Env = append(cmd.Env, "PATH="+extraPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// bannerShim makes asciinema 3 legible to asciinema-automation 0.2.2, which gates ALL typing
// on asciinema's first output line containing "recording asciicast to <file>" — the v2 banner.
// v3 prints an ":::"-style banner instead, so 0.2.2 types nothing and either times out (fresh
// file) or false-succeeds (early exit). The shim echoes the v2 line then execs the real binary
// ($1=rec, $2=<file> in automation's spawn). Also pins WHICH asciinema automation drives — a
// stray v2 install earlier on its PATH would record v2 casts the build pipeline rejects. Drop
// when automation understands asciinema 3.
func bannerShim() (dir string, cleanup func(), err error) {
	asc, err := asciinema3Path()
	if err != nil {
		return "", nil, err
	}
	dir, err = os.MkdirTemp("", "mu-cast-shim")
	if err != nil {
		return "", nil, err
	}
	shim := "#!/bin/sh\n# managed by mu — v2 banner for asciinema-automation, then the real asciinema\necho \"recording asciicast to $2\"\nexec " + asc + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "asciinema"), []byte(shim), 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// asciinema3Path returns the first asciinema on PATH reporting major version 3. Plain
// LookPath isn't enough: asciinema-automation pip-depends on python asciinema 2.4.0, so
// every install of it (pipx/venv/pyenv) carries a v2 binary that can shadow the real one
// and record casts the v3-native build pipeline rejects.
func asciinema3Path() (string, error) {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		p := filepath.Join(d, "asciinema")
		if info, err := os.Stat(p); err != nil || info.IsDir() {
			continue
		}
		if out, err := exec.Command(p, "--version").Output(); err == nil &&
			strings.Contains(string(out), "asciinema 3") {
			return p, nil
		}
	}
	return "", fmt.Errorf("asciinema 3 not found on PATH — add `cast` to MU_MODULES and run `mu setup toolchain`")
}

// runCastLive starts a live `asciinema rec` you type into (workflow 2). The raw cast is named
// from the bare argument; build it afterward with `mu cast build <name>`.
func runCastLive(name string) error {
	asc, err := asciinema3Path()
	if err != nil {
		return err
	}
	out := name
	if !strings.HasSuffix(out, ".cast") {
		out += ".cast"
	}
	// asciinema 3 refuses to overwrite an existing file — re-recording a take is the point here
	_ = os.Remove(out)
	render.Info("live recording → " + out + " (echo START_RECORDING / STOP_RECORDING to bound it; exit the shell to stop)")
	if err := recordExec(asc, "", "rec", out); err != nil {
		return err
	}
	render.OK("recorded " + out + " — render with `mu cast build " + strings.TrimSuffix(out, ".cast") + "`")
	return nil
}

// runCastAutoAll records each script in order (the `mu cast demo/*.sh` glob), aborting on the
// first failure so a broken part doesn't cascade into the rest, then builds the takes into one
// svg with the default pipeline (rerun `mu cast build` by hand to tune flags).
func runCastAutoAll(scripts []string, timeout int) error {
	for _, s := range scripts {
		if !strings.HasSuffix(s, ".sh") {
			return fmt.Errorf("mixed args: %s — a multi-arg record must be all '.sh' scripts", s)
		}
	}
	names := make([]string, len(scripts))
	for i, s := range scripts {
		if err := runCastAuto(s, timeout); err != nil {
			return err
		}
		names[i] = strings.TrimSuffix(s, ".sh")
	}
	return runCastBuild(names, defaultBuildOpts())
}

// runCastAuto records a '.sh' script by auto-typing it with asciinema-automation (workflow 1).
// The '#$'-directives (`#$ wait`, etc.) in the script pace the typing.
func runCastAuto(script string, timeout int) error {
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("no script %s", script)
	}
	auto, err := exec.LookPath("asciinema-automation")
	if err != nil {
		return fmt.Errorf("asciinema-automation not found — add `cast` to MU_MODULES and run `mu setup toolchain`")
	}
	out := strings.TrimSuffix(script, ".sh") + ".cast" // keep the script's dir — the cast lands beside it
	args := []string{script, out}
	if timeout > 0 {
		args = append(args, "-t", strconv.Itoa(timeout))
	}
	shimDir, cleanup, err := bannerShim()
	if err != nil {
		return err
	}
	defer cleanup()
	// asciinema 3 refuses to overwrite an existing file, and asciinema-automation swallows
	// the refusal (exit 0, cast untouched) — a silent no-op sold as a take. Remove first.
	_ = os.Remove(out)
	render.Info("auto-recording " + script + " → " + out + " (asciinema-automation)")
	if err := recordExec(auto, shimDir, args...); err != nil {
		// A killed recorder leaves a startup-only cast that builds into a junk svg —
		// call it out so the partial doesn't get mistaken for a take.
		if _, statErr := os.Stat(out); statErr == nil {
			render.Warn("partial cast left at " + out + " — re-record before building")
		}
		return err
	}
	render.OK("recorded " + out)
	return nil
}

type castBuildOpts struct {
	out   string
	idle  float64
	gap   float64
	start string
	stop  string
	noSVG bool
	mp4   bool
}

// defaultBuildOpts is the shared baseline — the build subcommand's flag defaults and the
// auto-record path's build use the same values.
func defaultBuildOpts() castBuildOpts {
	return castBuildOpts{idle: 2.0, gap: 1.0, start: defStartMark, stop: defStopMark}
}

func castBuildCmd() *cobra.Command {
	var o castBuildOpts
	cmd := &cobra.Command{
		Use:   "build <cast>...",
		Short: "Clean raw casts and render an svg (crop → idle-limit → concat → svg).",
		Long: "Post-process one or more raw asciicast v3 recordings into a single svg: crop to the\n" +
			"START/STOP markers, clamp idle gaps, stitch the parts in order, convert to asciicast\n" +
			"v2 (svg-term reads v2 only), and render. A <cast> may be named with or without the\n" +
			"'.cast' suffix. Writes <out>.v2.cast (the cleaned svg source) and <out>.svg;\n" +
			"--mp4 adds <out>.mp4 (agg + ffmpeg) for embedding in slides.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runCastBuild(args, o)
		},
	}
	f := cmd.Flags()
	d := defaultBuildOpts()
	f.StringVarP(&o.out, "out", "o", "", "output base name (default: the first input's name)")
	f.Float64Var(&o.idle, "idle", d.idle, "clamp any idle gap to at most this many `seconds`")
	f.Float64Var(&o.gap, "gap", d.gap, "`seconds` of pause inserted between concatenated parts")
	f.StringVar(&o.start, "start", d.start, "`marker` to crop the head to")
	f.StringVar(&o.stop, "stop", d.stop, "`marker` to crop the tail to")
	f.BoolVar(&o.noSVG, "no-svg", false, "stop at the cleaned v2 cast; skip the svg render")
	f.BoolVar(&o.mp4, "mp4", false, "also render an mp4 via agg + ffmpeg, for slide embeds")
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
		out = strings.TrimSuffix(inputs[0], ".cast") // keep the input's dir — outputs land beside it
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

	if !o.noSVG {
		if err := renderSVG(v2Path, out+".svg"); err != nil {
			return err
		}
	}
	if o.mp4 {
		return renderMP4(v2Path, out+".mp4")
	}
	return nil
}

func writeCast(path string, c *cast.Cast) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return c.Write(f)
}

// renderSVG pipes the cleaned v2 cast through svg-term (svg-term reads asciicast v2 only — that
// is why ToV2 runs first). It prefers the declared `svg-term` binary from the `cast` mise tier;
// absent that, it falls back to `npx svg-term-cli` (no install needed). Missing both is a SOFT
// failure: the cleaned cast still stands, so you can render later or pass --no-svg.
func renderSVG(v2Path, svgPath string) error {
	var cmd *exec.Cmd
	if bin, err := exec.LookPath("svg-term"); err == nil {
		cmd = exec.Command(bin, "--out", svgPath, "--window")
	} else if npx, err := exec.LookPath("npx"); err == nil {
		cmd = exec.Command(npx, "svg-term-cli", "--out", svgPath, "--window")
	} else {
		render.Warn("svg-term not found — kept the cleaned cast, skipped the svg (add `cast` to MU_MODULES + `mu setup toolchain`, or pass --no-svg)")
		return nil
	}
	in, err := os.Open(v2Path)
	if err != nil {
		return err
	}
	defer in.Close()
	cmd.Stdin = in
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("svg-term: %w", err)
	}
	render.OK("wrote " + svgPath)
	return nil
}

// renderMP4 renders the cleaned v2 cast to an mp4 for slide embedding: agg rasterizes the
// cast to a gif (agg is gif-only), then the base tier's ffmpeg wraps it into a player-safe
// mp4 (yuv420p + even dimensions — QuickTime/PowerPoint reject other layouts). Missing
// tools are a SOFT failure, matching renderSVG: the cleaned cast and svg still stand.
func renderMP4(v2Path, mp4Path string) error {
	agg, aggErr := exec.LookPath("agg")
	ffmpeg, ffErr := exec.LookPath("ffmpeg")
	if aggErr != nil || ffErr != nil {
		render.Warn("agg/ffmpeg not found — skipped the mp4 (add `cast` to MU_MODULES + `mu setup toolchain`)")
		return nil
	}
	dir, err := os.MkdirTemp("", "mu-cast-mp4-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	gif := filepath.Join(dir, "cast.gif")
	cmd := exec.Command(agg, v2Path, gif)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("agg: %w", err)
	}
	cmd = exec.Command(ffmpeg, "-y", "-loglevel", "error", "-i", gif,
		"-movflags", "+faststart", "-pix_fmt", "yuv420p",
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", mp4Path)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	render.OK("wrote " + mp4Path)
	return nil
}
