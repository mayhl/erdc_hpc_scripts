// Package setup holds the self-contained data the `mu setup` commands need — the mise
// tier configs embedded in the binary, so onboarding a fresh box needs no separate
// .config clone (mu is the source of truth; `mu setup toolchain` emits these to
// MISE_CONFIG_DIR for the running shell and config-resolves them to install). Kept a
// leaf package (no cli/render deps) so both the command and its tests import it cheaply.
package setup

import (
	"embed"
	"io/fs"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// miseFS embeds the mise config tree — the base CLI tier (config.toml), the nvim/hpc
// tier (config.hpc.toml, MISE_ENV∋hpc), the fmt module (config.fmt.toml, MISE_ENV∋fmt),
// and the always-merged Cat-2 runtimes (conf.d/toolchains.toml, node). These are the
// authoritative tier definitions — the .config/mise copies are generated from here.
//
//go:embed all:mise
var miseFS embed.FS

// MiseFiles returns the embedded tier tree as a map of MISE_CONFIG_DIR-relative path →
// content (e.g. "config.toml", "conf.d/toolchains.toml"). The caller writes these to
// MISE_CONFIG_DIR (the running shell reads them) or stages them to a temp dir to install.
func MiseFiles() (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(miseFS, "mise", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := miseFS.ReadFile(p)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(p, "mise/")] = string(b)
		return nil
	})
	return out, err
}

// BaseToolNames returns the tool keys in the base CLI tier (config.toml) — the tier
// installed on every machine. The doctor checks these are present; the hpc/fmt tiers are
// conditionally installed (MISE_ENV), so verifying them by name would false-warn.
func BaseToolNames() ([]string, error) {
	files, err := MiseFiles()
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tools map[string]any `toml:"tools"`
	}
	if err := toml.Unmarshal([]byte(files["config.toml"]), &doc); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(doc.Tools))
	for n := range doc.Tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// DumpManifest renders the embedded tiers for `--dump-manifest`: each file under a
// header, in a stable order (config.toml first, then the rest alphabetically), so the
// effective toolchain is inspectable without a checkout.
func DumpManifest() (string, error) {
	files, err := MiseFiles()
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if a, b := names[i] == "config.toml", names[j] == "config.toml"; a != b {
			return a // config.toml (the base tier) leads
		}
		return names[i] < names[j]
	})
	var b strings.Builder
	for _, n := range names {
		b.WriteString("# ===== " + n + " =====\n")
		b.WriteString(files[n])
		if !strings.HasSuffix(files[n], "\n") {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}
