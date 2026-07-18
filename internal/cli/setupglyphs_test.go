package cli

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestGlyphPairsInventory(t *testing.T) {
	if len(glyphPairs) != 10 {
		t.Fatalf("pair count %d — sync with glyphs/build_font.py GLYPHS", len(glyphPairs))
	}
	seen := map[rune]bool{}
	for _, p := range glyphPairs {
		for _, r := range []rune{p.sil, p.stk} {
			if r < 0xF8000 || r > 0xF80FF {
				t.Fatalf("pair %d: U+%04X outside the MuHPCIcons symbol_map range", p.id, r)
			}
			if seen[r] {
				t.Fatalf("codepoint U+%04X used twice", r)
			}
			seen[r] = true
		}
	}
}

func TestIconBlock(t *testing.T) {
	// pair 1 → wide = silhouette+stack (8-hex \U for the astral PUA), narrow = silhouette
	p, _ := pairFor(1)
	got := iconBlock(cpEsc(p.sil)+cpEsc(p.stk), cpEsc(p.sil))
	want := "# p10k os_icon cluster glyph — codepoints in the private MuHPCIcons font\n" +
		"export MU_OS_ICON=$'\\U000F8000\\U000F8001'\n" +
		"export MU_OS_ICON_NARROW=$'\\U000F8000'\n"
	if got != want {
		t.Fatalf("block:\n got %q\nwant %q", got, want)
	}
}

func TestPatchContent(t *testing.T) {
	// absent (empty) → append the commented block, report "added to"
	out, action := patchContent("", `\U000F8000\U000F8001`, `\U000F8000`)
	if action != "added to" {
		t.Fatalf("add action=%q", action)
	}
	if !strings.Contains(out, "export MU_OS_ICON=$'\\U000F8000\\U000F8001'") ||
		!strings.Contains(out, "export MU_OS_ICON_NARROW=$'\\U000F8000'") {
		t.Fatalf("added content:\n%s", out)
	}

	// a second, different pair → in-place UPDATE (no duplicate export lines)
	out, action = patchContent(out, `\U000F8011\U000F800D`, `\U000F8011`)
	if action != "updated" {
		t.Fatalf("update action=%q", action)
	}
	if n := strings.Count(out, "export MU_OS_ICON="); n != 1 {
		t.Fatalf("expected exactly one MU_OS_ICON line after update, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, `export MU_OS_ICON=$'\U000F8011\U000F800D'`) {
		t.Fatalf("update content:\n%s", out)
	}

	// re-applying the same pair is a stable rewrite (still one line, still updated)
	out2, action := patchContent(out, `\U000F8011\U000F800D`, `\U000F8011`)
	if action != "updated" || out2 != out {
		t.Fatalf("idempotent: action=%q stable=%v", action, out2 == out)
	}
}

func TestRemoteWriteCmd(t *testing.T) {
	content := "# hi\nexport MU_OS_ICON=$'\\U000F8000\\U000F8001'\n"
	cmd := remoteWriteCmd(content)
	// resolves the path from $MU_ROOT (or the ~/.config/mu default) on the box
	if !strings.Contains(cmd, `${MU_ROOT:-$HOME/.config/mu}`) || !strings.Contains(cmd, "base64 -d") {
		t.Fatalf("cmd shape: %q", cmd)
	}
	// the embedded base64 must round-trip back to the exact content (survives quoting)
	const pre = `printf %s "`
	start := strings.Index(cmd, pre) + len(pre)
	end := strings.Index(cmd[start:], `"`) + start
	dec, err := base64.StdEncoding.DecodeString(cmd[start:end])
	if err != nil || string(dec) != content {
		t.Fatalf("round-trip: err=%v decoded=%q", err, dec)
	}
}

func TestCustomShPath(t *testing.T) {
	// MU_ROOT set wins
	t.Setenv("MU_ROOT", "/opt/mu")
	if p, err := customShPath(); err != nil || p != "/opt/mu/custom.sh" {
		t.Fatalf("with MU_ROOT: %q %v", p, err)
	}
	// unset (the --node-over-bash-lc case) falls back to $HOME/.config/mu
	t.Setenv("MU_ROOT", "")
	t.Setenv("HOME", "/home/box")
	if p, err := customShPath(); err != nil || p != "/home/box/.config/mu/custom.sh" {
		t.Fatalf("fallback: %q %v", p, err)
	}
}

// patchContent must preserve unrelated content around an in-place update.
func TestPatchContentPreservesOtherLines(t *testing.T) {
	seed := "# my aliases\nalias k=kubectl\nexport MU_OS_ICON=$'\\U000F0000'\nexport MU_OS_ICON_NARROW=$'\\U000F0000'\nexport EDITOR=nvim\n"
	out, _ := patchContent(seed, `\U000F8000\U000F8001`, `\U000F8000`)
	for _, keep := range []string{"# my aliases", "alias k=kubectl", "export EDITOR=nvim"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("lost unrelated line %q:\n%s", keep, out)
		}
	}
	if !strings.Contains(out, `export MU_OS_ICON=$'\U000F8000\U000F8001'`) {
		t.Fatalf("icon not updated in place:\n%s", out)
	}
}
