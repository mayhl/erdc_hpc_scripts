package setup

import (
	"strings"
	"testing"
)

func TestMiseFiles(t *testing.T) {
	files, err := MiseFiles()
	if err != nil {
		t.Fatalf("MiseFiles: %v", err)
	}
	// the four tiers embed as MISE_CONFIG_DIR-relative paths, conf.d nested
	for _, want := range []string{"config.toml", "config.hpc.toml", "config.fmt.toml", "conf.d/toolchains.toml"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing embedded tier %q (have %v)", want, keys(files))
		}
	}
	if !strings.Contains(files["config.toml"], "[tools]") {
		t.Error("config.toml missing a [tools] table")
	}
	// the hpc tier overrides delta with the musl build for the glibc-2.28 login nodes
	if !strings.Contains(files["config.hpc.toml"], `matching = "musl"`) {
		t.Error("config.hpc.toml missing the musl delta override")
	}
}

func TestBaseToolNames(t *testing.T) {
	names, err := BaseToolNames()
	if err != nil {
		t.Fatalf("BaseToolNames: %v", err)
	}
	// the base tier the doctor checks — sorted, and carries the git-diff pair
	got := strings.Join(names, ",")
	for _, want := range []string{"github:dandavison/delta", "difftastic", "ripgrep"} {
		if !strings.Contains(got, want) {
			t.Errorf("base tools %v missing %q", names, want)
		}
	}
}

func TestDumpManifest(t *testing.T) {
	s, err := DumpManifest()
	if err != nil {
		t.Fatalf("DumpManifest: %v", err)
	}
	// config.toml leads, every tier is present under its header
	if i := strings.Index(s, "config.toml"); i < 0 || i > strings.Index(s, "config.hpc.toml") {
		t.Error("config.toml should head the dump")
	}
	for _, want := range []string{"config.hpc.toml", "config.fmt.toml", "conf.d/toolchains.toml"} {
		if !strings.Contains(s, want) {
			t.Errorf("dump missing %q", want)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
