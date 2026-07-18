package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/hpc"
)

// glyphPair is one cluster's icon: a silhouette codepoint and its stack codepoint, keyed
// by a stable ID. NO names — the codepoint→cluster mapping is the sensitive association
// the private glyphs repo holds (this is a PUBLIC repo; the pre-commit guard blocks real
// cluster names, even in comments). The rendered glyph is the identifier; a bare PUA
// codepoint is meaningless without the private MuHPCIcons font. The silhouette↔stack
// pairing mirrors glyphs/build_font.py (font = source of truth); stored codepoint-only.
type glyphPair struct {
	id       int
	sil, stk rune
}

// glyphPairs is the ID→(silhouette, stack) table, ordered by silhouette codepoint so the
// IDs are stable. Keep in sync with glyphs/build_font.py GLYPHS.
var glyphPairs = []glyphPair{
	{1, 0xF8000, 0xF8001},
	{2, 0xF8005, 0xF800A},
	{3, 0xF8006, 0xF8003},
	{4, 0xF8007, 0xF8002},
	{5, 0xF8008, 0xF800B},
	{6, 0xF8009, 0xF8004},
	{7, 0xF8010, 0xF800C},
	{8, 0xF8011, 0xF800D},
	{9, 0xF8012, 0xF800E},
	{10, 0xF8013, 0xF800F},
}

// otherGlyphs are the non-pair codepoints — personal marks (MU_MARK, a separate seam) and
// the diagnostic probe — shown as a footnote, not part of the cluster-pair table.
var otherGlyphs = []rune{0xF8020, 0xF8021, 0xF8022, 0xF80F0}

// setupGlyphsCmd is `mu setup glyphs`: print the MuHPCIcons cluster-icon pairs as an
// ID | Silhouette | Stack table (the glyphs render only where the kitty symbol_map is
// loaded — your kitty, including over ssh). `--export <ID>` then patches this machine's
// $MU_ROOT/custom.sh with that pair's MU_OS_ICON lines (in-place update, or appended if
// absent) — the deployed pair-form: wide = silhouette+stack, narrow = silhouette.
func setupGlyphsCmd() *cobra.Command {
	var export int
	var dryRun bool
	var node string
	c := &cobra.Command{
		Use:   "glyphs",
		Short: "List the MuHPCIcons cluster-icon pairs; --export <ID> sets a box's icon.",
		Long: "Print the MuHPCIcons cluster-icon pairs as an ID | Silhouette | Stack table so a\n" +
			"box's icon can be chosen by sight (the glyphs render only in a terminal with the\n" +
			"MuHPCIcons symbol_map loaded — your kitty, including over ssh). `--export <ID>`\n" +
			"then patches $MU_ROOT/custom.sh with that pair's MU_OS_ICON lines (wide =\n" +
			"silhouette+stack, narrow = silhouette). Add `--node <box>` to set it on a box\n" +
			"from here: mu ssh's over and runs the same export on the box's own mu — so you\n" +
			"pick the ID in your kitty and apply it remotely in one command.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if export == 0 {
				listGlyphs()
				return nil
			}
			return exportGlyph(export, dryRun, node)
		},
	}
	c.Flags().IntVar(&export, "export", 0, "patch custom.sh with pair `ID`'s MU_OS_ICON lines")
	c.Flags().StringVarP(&node, "node", "N", "", "apply on this box over ssh instead of the local machine")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "with --export: print the lines instead of writing custom.sh")
	return c
}

// listGlyphs prints the ID | Silhouette | Stack table plus the marks/probe footnote.
func listGlyphs() {
	fmt.Println("\n ID   Silhouette   Stack")
	for _, p := range glyphPairs {
		fmt.Printf(" %2d       %c           %c\n", p.id, p.sil, p.stk)
	}
	var foot strings.Builder
	for _, r := range otherGlyphs {
		fmt.Fprintf(&foot, "  %c U+%04X", r, r)
	}
	fmt.Printf("\nMarks + probe:%s\n", foot.String())
	fmt.Println("\nSet a box's icon:  mu setup glyphs --export <ID>   (run it on the box)")
}

// exportGlyph resolves pair ID and patches (or, with dryRun, prints) custom.sh's
// MU_OS_ICON lines — on this machine, or on node over ssh when set.
func exportGlyph(id int, dryRun bool, node string) error {
	p, ok := pairFor(id)
	if !ok {
		return fmt.Errorf("no glyph pair with ID %d — run `mu setup glyphs` to see the table", id)
	}
	wide := cpEsc(p.sil) + cpEsc(p.stk)
	narrow := cpEsc(p.sil)
	if dryRun {
		fmt.Print(iconBlock(wide, narrow))
		return nil
	}
	if node != "" {
		return exportGlyphRemote(node, id, wide, narrow)
	}
	path, err := customShPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	out, action := patchContent(string(data), wide, narrow)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s %s — MU_OS_ICON set to pair %d\n", action, path, id)
	return nil
}

// exportGlyphRemote patches node's custom.sh entirely from THIS machine's mu: it reads
// the box's file over ssh, applies the same patchContent locally, and writes it back —
// so the result never depends on the box's mu version (which lags an unpushed feature).
// The content is base64'd through the write command to survive shell quoting intact, and
// the path resolves to $MU_ROOT (or the ~/.config/mu default) on the box.
func exportGlyphRemote(node string, id int, wide, narrow string) error {
	target, err := hpc.Resolve(node)
	if err != nil {
		return err
	}
	cur, err := hpc.RemoteExec(target, remoteReadCmd)
	if err != nil {
		return err
	}
	out, action := patchContent(cur, wide, narrow)
	if _, err := hpc.RemoteExec(target, remoteWriteCmd(out)); err != nil {
		return err
	}
	fmt.Printf("%s custom.sh on %s — MU_OS_ICON set to pair %d\n", action, node, id)
	return nil
}

// remoteReadCmd cats the box's custom.sh (empty, not an error, when absent), resolving
// the path from $MU_ROOT or the ~/.config/mu default in the box's own shell.
const remoteReadCmd = `cat "${MU_ROOT:-$HOME/.config/mu}/custom.sh" 2>/dev/null || true`

// remoteWriteCmd is the box-side command that writes content to custom.sh: base64'd so the
// $'…' / backslashes / comment survive the ssh + bash -lc quoting untouched, decoded on
// the box, into $MU_ROOT (or the ~/.config/mu default). Nothing here needs the box's mu.
func remoteWriteCmd(content string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	root := `"${MU_ROOT:-$HOME/.config/mu}"`
	return fmt.Sprintf(`d=%s; mkdir -p "$d" && printf %%s "%s" | base64 -d > "$d/custom.sh"`, root, b64)
}

// pairFor looks up a glyph pair by ID.
func pairFor(id int) (glyphPair, bool) {
	for _, p := range glyphPairs {
		if p.id == id {
			return p, true
		}
	}
	return glyphPair{}, false
}

// cpEsc encodes a rune as the 8-hex \U escape a zsh $'…' literal needs for the astral
// private-use plane (a 4-hex \u would truncate a codepoint above U+FFFF).
func cpEsc(r rune) string { return fmt.Sprintf(`\U%08X`, r) }

// iconBlock is the commented MU_OS_ICON export block, matching the deployed custom.sh
// shape — the unit appended when a box has no block yet.
func iconBlock(wide, narrow string) string {
	return "# p10k os_icon cluster glyph — codepoints in the private MuHPCIcons font\n" +
		fmt.Sprintf("export MU_OS_ICON=$'%s'\n", wide) +
		fmt.Sprintf("export MU_OS_ICON_NARROW=$'%s'\n", narrow)
}

// customShPath is $MU_ROOT/custom.sh — the untracked machine-local file zsh.bootstrap
// sources and the p10k theme reads MU_OS_ICON from. MU_ROOT is exported by the box's zsh
// login (shell-init), NOT bash, so a --node delegation lands here under ssh's `bash -lc`
// with it unset; fall back to the onboard default (~/.config/mu), where custom.sh lives.
func customShPath() (string, error) {
	root := os.Getenv("MU_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("$MU_ROOT unset and no home dir — can't locate custom.sh")
		}
		root = filepath.Join(home, ".config", "mu")
	}
	return filepath.Join(root, "custom.sh"), nil
}

// patchContent sets the MU_OS_ICON / MU_OS_ICON_NARROW exports in a custom.sh's text and
// returns the new text plus "updated"/"added to". If both lines already exist they are
// updated IN PLACE (position + any comment + unrelated lines preserved); otherwise the
// commented block is appended. Idempotent — re-exporting the same pair is a stable
// rewrite. Pure, so the local file patch and the --node remote patch share one behavior.
func patchContent(old, wide, narrow string) (string, string) {
	iconLine := fmt.Sprintf("export MU_OS_ICON=$'%s'", wide)
	narrowLine := fmt.Sprintf("export MU_OS_ICON_NARROW=$'%s'", narrow)

	lines := []string{}
	if len(old) > 0 {
		lines = strings.Split(strings.TrimRight(old, "\n"), "\n")
	}

	foundIcon, foundNarrow := false, false
	for i, l := range lines {
		switch t := strings.TrimSpace(l); {
		case strings.HasPrefix(t, "export MU_OS_ICON="):
			lines[i], foundIcon = iconLine, true
		case strings.HasPrefix(t, "export MU_OS_ICON_NARROW="):
			lines[i], foundNarrow = narrowLine, true
		}
	}

	action := "updated"
	if !foundIcon || !foundNarrow {
		// no clean block — drop any stray half, append a fresh commented one
		kept := lines[:0]
		for _, l := range lines {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "export MU_OS_ICON=") || strings.HasPrefix(t, "export MU_OS_ICON_NARROW=") {
				continue
			}
			kept = append(kept, l)
		}
		lines = kept
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, strings.TrimRight(iconBlock(wide, narrow), "\n"))
		action = "added to"
	}

	return strings.Join(lines, "\n") + "\n", action
}
