package cli

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/git"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// gitCmd is the opt-in `mu git` module (gated by MU_MODULES=git): pretty, read-only
// views of the .config signwip/pushsigned workflow. It never signs or pushes — the
// shell `gsw`/`gps` remain the source of truth (bootstrap-safe).
func gitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "git",
		Short: "Pretty, read-only views of the git signwip/pushsigned workflow.",
		Long: "Colored previews of the .config git workflow — what `gsw`/`gps` would sign\n" +
			"or push, in the house visual language. Read-only: the shell tools still do the\n" +
			"actual signing/pushing. Opt-in via MU_MODULES=git.",
	}
	rv, sw, ps, dr := gitReviewedCmd(), gitSignwipCmd(), gitPushsignedCmd(), gitDoctorCmd()
	setHelpLabel(rv, "preview", render.HueLoc)
	setHelpLabel(sw, "preview", render.HueLoc)
	setHelpLabel(ps, "preview", render.HueLoc)
	setHelpLabel(dr, "check", render.HueUser)
	c.AddCommand(rv, sw, ps, dr)
	// The house help renderer is inherited from root (wrapHelp(root)); here we just
	// give the module its heading, MU_MODULES-gated badge, and shell-front-door panel.
	setHelpTitle(c, "Git Workflow Previews")
	setHelpLabel(c, "opt-in", render.HueGroup)
	setHelpShortcuts(
		c,
		[2]string{"grv", "git reviewed — un-tag reviewed WIP oldest-first (previews via mu git)"},
		[2]string{"gsw", "git signwip — sign the reviewed WIP (previews via mu git)"},
		[2]string{"gps", "git pushsigned — push the signed prefix (previews via mu git)"},
	)
	return c
}

func gitReviewedCmd() *cobra.Command {
	var interactive bool
	c := &cobra.Command{
		Use:   "reviewed [N]",
		Short: "Which [unreviewed] WIP `git reviewed` would un-tag (oldest-first).",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if interactive {
				if len(args) > 0 {
					return usageErr("a count and -i are mutually exclusive")
				}
				return gitReviewedPick()
			}
			n := 0 // 0 = all
			if len(args) == 1 && args[0] != "all" {
				v, err := strconv.Atoi(args[0])
				if err != nil || v < 0 {
					return fmt.Errorf("count must be a non-negative number or 'all' (got %q)", args[0])
				}
				n = v
			}
			r, err := git.ReviewedPreview(n)
			if err != nil {
				return err
			}
			if !r.HasBase {
				render.Warn("no signed commit to base on")
				return nil
			}
			render.GitReviewed(r)
			return nil
		},
	}
	c.Flags().BoolVarP(&interactive, "interactive", "i", false,
		"pick the un-tag cut point in the interactive browser (prints the count; grv -i's front end)")
	setHelpArgs(c,
		[2]string{"[N]", "how many tags to preview un-tagging, oldest-first — a count or 'all' (default all)"})
	return c
}

// gitReviewedPick is the `-i` cut-point picker: the [unreviewed] WIP oldest→newest,
// `i` overlays the full message, and enter on a row means "un-tag through here".
// It prints ONLY the picked count on stdout (0 = cancelled) — grv -i captures it and
// does the actual strip — so the picker itself renders on stderr.
func gitReviewedPick() error {
	if !render.Interactive() {
		return usageErr("mu git reviewed -i needs a terminal (stdin is not a tty)")
	}
	var tagged []git.ReviewRow // latest fetch, oldest→newest — maps the picked hash back to a count
	fetch := func() []render.SelectRow {
		r, err := git.ReviewedPreview(0)
		if err != nil || !r.HasBase {
			return nil
		}
		msgs, _ := git.WipMessages() // one bulk git call; a blip just blanks the pane
		tagged = tagged[:0]
		for _, row := range r.Rows {
			if row.Act == "untag" { // n=0 preview marks every tagged commit untag
				tagged = append(tagged, row)
			}
		}
		rows := make([]render.SelectRow, 0, len(tagged))
		for i, row := range tagged {
			rows = append(rows, render.SelectRow{
				ID:    row.Hash,
				Cells: []string{strconv.Itoa(i + 1), row.Hash, git.StripTag(row.Subject)},
				// Yellow hash matches git's own --oneline (the accepted git-native exception).
				Hues:    []string{render.HueDim, render.HueWarn, ""},
				Preview: msgs[row.Hash],
			})
		}
		return rows
	}
	ids, err := render.Select(render.SelectSpec{
		Verb:     "un-tag through",
		Columns:  []string{"N", "HASH", "SUBJECT"},
		Fetch:    fetch,
		PickOne:  true,
		StderrUI: true,
		Preview:  true, // live pane follows the cursor; `i` keeps the full overlay
		Detail: func(id string) string {
			msg, err := git.Message(id)
			if err != nil {
				return ""
			}
			return msg
		},
	})
	if err != nil {
		return err
	}
	n := 0 // cancelled (or the list emptied under us) → 0, grv aborts
	if len(ids) == 1 {
		for i, row := range tagged {
			if row.Hash == ids[0] {
				n = i + 1
				break
			}
		}
	}
	fmt.Println(n)
	return nil
}

func gitSignwipCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "signwip",
		Short: "Which unsigned WIP would sign vs skip ([unreviewed]).",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			s, err := git.SignwipPreview()
			if err != nil {
				return err
			}
			if !s.HasBase {
				render.Warn("no signed commit to base on")
				return nil
			}
			render.GitSignwip(s)
			return nil
		},
	}
}

func gitPushsignedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pushsigned",
		Short: "The contiguous signed prefix that would push vs held WIP.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			p, err := git.PushsignedPreview()
			if err != nil {
				return err
			}
			if !p.HasUpstream {
				render.Warn("no upstream set for the current branch")
				return nil
			}
			render.GitPushsigned(p)
			return nil
		},
	}
}

func gitDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "git on PATH and the .config git workflow files present.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			d := git.DoctorReport()
			rows := make([]render.StatusRow, 0, len(d.Files)+1)
			if d.GitPath != "" {
				rows = append(rows, render.StatusRow{Level: "ok", Name: "git", Detail: d.GitPath})
			} else {
				rows = append(rows, render.StatusRow{Level: "error", Name: "git", Detail: "not found on PATH"})
			}
			for _, f := range d.Files {
				if f.Exists {
					rows = append(rows, render.StatusRow{Level: "ok", Name: f.Name, Detail: f.Path})
				} else {
					rows = append(rows, render.StatusRow{Level: "warn", Name: f.Name, Detail: f.Path + " (missing)"})
				}
			}
			render.StatusTable("git — doctor", rows)
			return nil
		},
	}
}
