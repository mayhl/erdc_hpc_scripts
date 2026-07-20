package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
)

func TestWalltimeField(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"", true},
		{"1:00:00", true},
		{"168:00:00", true},
		{"0:30:00", true},
		{"12:60:00", false},
		{"12:00", false},
		{"::", false},
		// The field takes the duration shorthand too, and refuses a bare number — the parser
		// is queue.ParseWalltime, tested there; this pins that the FIELD uses it.
		{"12h", true},
		{"1.5h", true},
		{"10m", true},
		{"90", false},
	}
	for _, c := range cases {
		if got := walltimeField(c.in, nil) == ""; got != c.ok {
			t.Errorf("walltimeField(%q) ok=%v, want %v", c.in, got, c.ok)
		}
	}
}

func TestIntField(t *testing.T) {
	for in, ok := range map[string]bool{"": true, "4": true, "0": false, "-2": false, "four": false} {
		if got := intField(in, nil) == ""; got != ok {
			t.Errorf("intField(%q) ok=%v, want %v", in, got, ok)
		}
	}
}

// TestQueueSeed locks the queue-field seeding shared by the sub/tunnel/shell forms: config
// default for a bare sub form, the literal for -q, config entry (or pending) for class
// flags, options deduped with the sentinel first.
func TestQueueSeed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := `
[[cluster]]
name = "alpha"
domain = "a.example.mil"
nodes = ["hpc1"]
submit_queue = { default = "standard", gpu = "gpu_short" }
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MU_CONFIG_FILE", path)
	config.ResetForTest()
	defer config.ResetForTest()

	// bare → config default selected; options = sentinel + configured entries
	val, pending, opts := queueSeed("alpha", &queueSel{}, true)
	if val != "standard" || pending != "" {
		t.Errorf("bare seed = %q pending %q", val, pending)
	}
	if got := strings.Join(opts, ","); got != schedDefault+",standard,gpu_short" {
		t.Errorf("options = %q", got)
	}

	// -q literal wins and joins the options
	if val, _, opts = queueSeed("alpha", &queueSel{queue: "special"}, true); val != "special" || !strings.Contains(strings.Join(opts, ","), "special") {
		t.Errorf("-q seed = %q opts %v", val, opts)
	}

	// class flag with a config entry resolves; debug falls to its literal; vis stays pending
	if val, pending, _ = queueSeed("alpha", &queueSel{gpu: true}, true); val != "gpu_short" || pending != "" {
		t.Errorf("gpu seed = %q pending %q", val, pending)
	}
	if val, pending, _ = queueSeed("alpha", &queueSel{debug: true}, true); val != "debug" || pending != "" {
		t.Errorf("debug seed = %q pending %q", val, pending)
	}
	if val, pending, _ = queueSeed("alpha", &queueSel{vis: true}, true); val != schedDefault || pending != "vis" {
		t.Errorf("vis seed = %q pending %q", val, pending)
	}

	// tunnel/shell pass bareDefault=false: a flagless form starts on the scheduler default,
	// NOT submit_queue.default — that entry is where batch work goes. A flag still resolves.
	if val, _, opts = queueSeed("alpha", &queueSel{}, false); val != schedDefault {
		t.Errorf("bare interactive seed = %q, want the scheduler default", val)
	}
	if got := strings.Join(opts, ","); got != schedDefault+",standard,gpu_short" {
		t.Errorf("interactive options = %q, want the configured queues offered anyway", got)
	}
	if val, _, _ = queueSeed("alpha", &queueSel{gpu: true}, false); val != "gpu_short" {
		t.Errorf("gpu interactive seed = %q", val)
	}
}

// TestQueuePatches covers the queue-backed forms' Load off the seeded cache: the enum keeps
// only up Exe queues, a pending class flag's single match is preselected, and the walltime /
// nodes validators clamp against the SELECTED queue's limits — the walltime compare in
// seconds, so shorthand can't slip past an HH:MM:SS limit.
func TestQueuePatches(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte(`
[[cluster]]
name = "alpha"
domain = "a.example.mil"
nodes = ["hpc1"]
cores_per_node = 4
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MU_CONFIG_FILE", cfg)
	config.ResetForTest()
	defer config.ResetForTest()

	stubQueues(
		t, "hpc1",
		queue.QueueInfo{Name: "standard", MaxWalltime: "168:00:00", MaxCores: "8", Type: "Exe", Enabled: "Y", Running: "Y"},
		queue.QueueInfo{Name: "debug", MaxWalltime: "01:00:00", Type: "Exe", Enabled: "Y", Running: "Y"},
		queue.QueueInfo{Name: "viz_std", Type: "Exe", Enabled: "Y", Running: "Y"},
		queue.QueueInfo{Name: "transfer_rt", Type: "Route", Enabled: "Y", Running: "Y"}, // routing — never offered
		queue.QueueInfo{Name: "broken", Type: "Exe", Enabled: "N", Running: "Y"},        // down — never offered
	)
	ix := queueFields{queue: sfQueue, walltime: sfWalltime, nodes: sfNodes}
	byIx := func(ps []render.FieldPatch) map[int]render.FieldPatch {
		m := map[int]render.FieldPatch{}
		for _, p := range ps {
			m[p.Index] = p
		}
		return m
	}
	vals := func(q, w, n string) []string {
		return []string{sfScript: "", sfQueue: q, sfAccount: "", sfWalltime: w, sfNodes: n, sfName: ""}
	}

	patches := queuePatches("hpc1", "alpha", "", ix)
	if len(patches) != 3 {
		t.Fatalf("got %d patches, want queue+walltime+nodes", len(patches))
	}
	m := byIx(patches)

	// enum: sentinel first, then only up Exe queues, in listing order
	if got := strings.Join(m[sfQueue].Options, ","); got != schedDefault+",standard,debug,viz_std" {
		t.Errorf("queue options = %q", got)
	}
	if m[sfQueue].Value != "" {
		t.Errorf("no pending key must preselect nothing, got %q", m[sfQueue].Value)
	}

	// pending class flag: the single VIS match is selected
	if p := byIx(queuePatches("hpc1", "alpha", "vis", ix)); p[sfQueue].Value != "viz_std" {
		t.Errorf("pending vis preselect = %q, want viz_std", p[sfQueue].Value)
	}

	// walltime validate: seconds compare against the selected queue's HH:MM:SS max
	for _, tc := range []struct {
		name, q, v, msg string
	}{
		{"shorthand over the max is caught", "debug", "1.5h", "over the debug max 01:00:00"},
		{"under the max passes", "debug", "45m", ""},
		{"exactly the max passes", "debug", "1h", ""},
		{"a roomier queue takes it", "standard", "2h", ""},
		{"garbage falls to the base check", "debug", "junk", "want " + wallHint},
		{"empty stays optional", "debug", "", ""},
		{"a blank limit means no clamp", "viz_std", "5h", ""},
		{"the sentinel has no limits", schedDefault, "5h", ""},
	} {
		t.Run("walltime/"+tc.name, func(t *testing.T) {
			if got := m[sfWalltime].Validate(tc.v, vals(tc.q, tc.v, "")); got != tc.msg {
				t.Errorf("validate(%q on %q) = %q, want %q", tc.v, tc.q, got, tc.msg)
			}
		})
	}

	// nodes validate: over the queue's MaxCores/cores_per_node ceiling
	for _, tc := range []struct {
		name, q, v, msg string
	}{
		{"over the ceiling is caught", "standard", "3", "over the standard max 2 nodes"},
		{"at the ceiling passes", "standard", "2", ""},
		{"garbage falls to the base check", "standard", "x", "want a positive integer"},
		{"no MaxCores means no ceiling", "debug", "9", ""},
	} {
		t.Run("nodes/"+tc.name, func(t *testing.T) {
			if got := m[sfNodes].Validate(tc.v, vals(tc.q, "", tc.v)); got != tc.msg {
				t.Errorf("validate(%q on %q) = %q, want %q", tc.v, tc.q, got, tc.msg)
			}
		})
	}

	// tunnel/shell shape: an absent nodes field (-1) gets no patch
	if got := queuePatches("hpc1", "alpha", "", queueFields{queue: tfQueue, walltime: tfWalltime, nodes: -1}); len(got) != 2 {
		t.Errorf("nodes=-1 got %d patches, want 2", len(got))
	}

	// nothing submittable after the filters → nil, the form keeps its config seed
	stubQueues(t, "hpc2", queue.QueueInfo{Name: "transfer_rt", Type: "Route"})
	if got := queuePatches("hpc2", "alpha", "", ix); got != nil {
		t.Errorf("all-routing cache must patch nothing, got %v", got)
	}
}

// TestSeedWalltime mirrors TestResolveWalltime's precedence — -t, then the --debug slot, then
// the config default — so the field a held session OPENS on can't disagree with what the flag
// path would submit. Two deliberate divergences from resolve: the seed keeps the value as
// typed (normalized on the way out), and an over-max -t is NOT capped here (the form's live
// validate flags it instead).
func TestSeedWalltime(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	silent := write("silent.sh", "#!/bin/bash\n#PBS -l select=1\necho hi\n")
	declared := write("declared.sh", "#!/bin/bash\n#PBS -l walltime=24:00:00\necho hi\n")
	unreadable := filepath.Join(dir, "lives-on-the-cluster.sh")

	cfg := write("config.toml", `
[[cluster]]
name = "alpha"
domain = "a.example.mil"
scheduler = "pbs"
nodes = ["hpc1"]
interactive_walltime = "1h"

[[cluster]]
name = "beta"
domain = "b.example.mil"
nodes = ["hpc2"]

[[cluster]]
name = "gamma"
domain = "g.example.mil"
nodes = ["hpc3"]
interactive_walltime = "junk"
`)
	t.Setenv("MU_CONFIG_FILE", cfg)
	config.ResetForTest()
	defer config.ResetForTest()

	stubQueues(
		t, "hpc1",
		queue.QueueInfo{Name: "debug", MaxWalltime: "00:30:00"},
		queue.QueueInfo{Name: "standard", MaxWalltime: "168:00:00"},
	)

	for _, tc := range []struct {
		name                string
		node, label, script string
		walltime, queueVal  string
		sel                 queueSel
		expect              string
	}{
		{name: "-t wins, left as typed", node: "hpc1", label: "alpha", walltime: "1.5h", queueVal: "standard", expect: "1.5h"},
		{name: "-t still beats --debug", node: "hpc1", label: "alpha", walltime: "10m", queueVal: "debug", sel: queueSel{debug: true}, expect: "10m"},
		{name: "-t over the max is NOT capped here", node: "hpc1", label: "alpha", walltime: "2h", queueVal: "debug", expect: "2h"},
		{name: "-t wins even past a declared script", node: "hpc1", label: "alpha", script: "declared", walltime: "2h", queueVal: "debug", expect: "2h"},
		{name: "--debug takes the whole slot, over the config default", node: "hpc1", label: "alpha", queueVal: "debug", sel: queueSel{debug: true}, expect: "00:30:00"},
		{name: "--dbg is the same slot", node: "hpc1", label: "alpha", queueVal: "debug", sel: queueSel{dbg: true}, expect: "00:30:00"},
		{name: "config default applies, left as typed", node: "hpc1", label: "alpha", queueVal: "standard", expect: "1h"},
		{name: "--debug with no cached slot falls to the config default", node: "hpc1", label: "alpha", queueVal: "mystery", sel: queueSel{debug: true}, expect: "1h"},
		{name: "nothing asked, nothing seeded", node: "hpc2", label: "beta", queueVal: "standard", expect: ""},
		{name: "a bad config default seeds nothing", node: "hpc3", label: "gamma", queueVal: "standard", expect: ""},
		{name: "a script that declares its own walltime blocks the seed", node: "hpc1", label: "alpha", script: "declared", queueVal: "debug", sel: queueSel{debug: true}, expect: ""},
		{name: "an unreadable script counts as declaring one", node: "hpc1", label: "alpha", script: "unreadable", queueVal: "debug", sel: queueSel{debug: true}, expect: ""},
		{name: "a silent script may be seeded", node: "hpc1", label: "alpha", script: "silent", queueVal: "standard", expect: "1h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := map[string]string{"": "", "silent": silent, "declared": declared, "unreadable": unreadable}[tc.script]
			sel := tc.sel
			if got := seedWalltime(tc.node, tc.label, script, tc.walltime, tc.queueVal, &sel); got != tc.expect {
				t.Errorf("got %q, want %q", got, tc.expect)
			}
		})
	}
}

// TestEitherScriptOrJob pins the tunnel form's cross-field rule: submit a script or adopt a
// job, never both, never neither — the check RunE does for the flag path.
func TestEitherScriptOrJob(t *testing.T) {
	vals := func(script, job string) []string {
		return []string{tfScript: script, tfJob: job, tfQueue: "", tfAccount: "", tfPort: "", tfLocal: ""}
	}
	if msg := eitherScriptOrJob("", vals("serve.sh", "")); msg != "" {
		t.Errorf("script alone rejected: %s", msg)
	}
	if msg := eitherScriptOrJob("", vals("", "4501")); msg != "" {
		t.Errorf("job alone rejected: %s", msg)
	}
	if msg := eitherScriptOrJob("", vals("serve.sh", "4501")); msg == "" {
		t.Error("script AND job accepted")
	}
	if msg := eitherScriptOrJob("", vals("", "")); msg == "" {
		t.Error("neither script nor job accepted")
	}
}
