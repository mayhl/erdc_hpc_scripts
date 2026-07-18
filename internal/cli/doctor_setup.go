package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/setup"
)

// doctorSetupCmd is `mu doctor setup`: a read-only health check of THIS machine's mu
// setup — shell wiring, toolchain, whether the installed build matches its source, and
// tracked-repo drift. Mirrors `mu doctor fmt`. Read-only; exits non-zero only on a FAIL
// (missing/dormant pieces WARN but don't block — setup is progressive).
func doctorSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Check this machine's mu setup (shell wiring, toolchain, build, repo drift).",
		Long: "Report this machine's mu setup: is the shell integration wired into your rc, is\n" +
			"the toolchain installed, does the running mu match its source checkout, and are\n" +
			"your tracked repos (.config, mayhl_utils) clean. Read-only — it changes nothing.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rows := []render.StatusRow{
				checkShellInit(),
				checkToolchainPresent(),
				checkBuildCurrent(),
			}
			rows = append(rows, checkRepoDrift(".config", filepath.Join(os.Getenv("HOME"), ".config"))...)
			if root := os.Getenv("MU_ROOT"); root != "" {
				rows = append(rows, checkRepoDrift("mayhl_utils", root)...)
			}
			render.StatusTable("Setup", rows)

			ok, warn, fail := tallyRows(rows)
			summary := fmt.Sprintf("setup: %d ok, %d warn, %d fail", ok, warn, fail)
			switch {
			case fail > 0:
				render.EventErr("doctor", summary)
			case warn > 0:
				render.EventWarn("doctor", summary)
			default:
				render.EventOK("doctor", summary)
			}
			if fail > 0 {
				return codeErr(1)
			}
			return nil
		},
	}
}

// checkShellInit looks for the mu shell-integration line — in the top-level rc files and
// in any ~/.config shell files they source (where the wiring often actually lives).
func checkShellInit() render.StatusRow {
	home := os.Getenv("HOME")
	files := []string{
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".config", "fish", "config.fish"),
	}
	for _, pat := range []string{"zsh*.zsh", "bash*.sh", "*.sh"} {
		if m, err := filepath.Glob(filepath.Join(home, ".config", pat)); err == nil {
			files = append(files, m...)
		}
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		s := string(b)
		// New self-contained wiring, or the legacy `source $MU_ROOT/init.sh` bootstrap.
		eval := strings.Contains(s, "mu setup") || strings.Contains(s, "mu shell-init")
		legacy := strings.Contains(s, "init.sh") && strings.Contains(s, "MU_ROOT")
		if eval || legacy {
			rel := strings.TrimPrefix(f, home+"/")
			detail := "wired in ~/" + rel
			if legacy && !eval {
				detail += ` (legacy init.sh — mu setup --eval is the self-contained path)`
			}
			return render.StatusRow{Level: "ok", Name: "shell-init", Detail: detail}
		}
	}
	return render.StatusRow{Level: "warn", Name: "shell-init", Detail: `not wired — add eval "$(mu setup --eval <shell>)" to your rc`}
}

// checkToolchainPresent reports whether mise and the manifest tools are installed.
func checkToolchainPresent() render.StatusRow {
	mise, present := misePath()
	if !present {
		return render.StatusRow{Level: "warn", Name: "toolchain", Detail: "mise not installed — run mu setup toolchain"}
	}
	names, err := setup.BaseToolNames()
	if err != nil {
		return render.StatusRow{Level: "warn", Name: "toolchain", Detail: "mise ok; manifest unreadable: " + err.Error()}
	}
	out, _ := exec.Command(mise, "ls").CombinedOutput()
	ls := string(out)
	var missing []string
	for _, spec := range names {
		if tool := toolName(spec); !strings.Contains(ls, tool) {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return render.StatusRow{Level: "warn", Name: "toolchain", Detail: "missing " + strings.Join(missing, ", ") + " — run mu setup toolchain"}
	}
	return render.StatusRow{Level: "ok", Name: "toolchain", Detail: fmt.Sprintf("mise + %d base tools", len(names))}
}

// checkBuildCurrent compares the running binary's VCS stamp to MU_ROOT's HEAD — the
// "is my installed mu current with the source?" check (SHA-based; a semver/release
// scheme would slot in here later).
func checkBuildCurrent() render.StatusRow {
	ver := muVersion()
	if ver == "" {
		return render.StatusRow{Level: "warn", Name: "build", Detail: "no VCS stamp on this build"}
	}
	root := os.Getenv("MU_ROOT")
	if root == "" {
		return render.StatusRow{Level: "ok", Name: "build", Detail: ver + " (MU_ROOT unset — not compared to source)"}
	}
	// Compare like-for-like: the running binary's stamp is `git describe`, so the source
	// reference is the repo's describe now — not a raw HEAD SHA (which never matches a tag).
	src := gitField(root, "describe", "--tags", "--always", "--dirty")
	if src == "" {
		return render.StatusRow{Level: "ok", Name: "build", Detail: ver + " (MU_ROOT not a git repo)"}
	}
	if ver == src {
		detail := "current: " + ver
		if strings.HasSuffix(ver, "-dirty") {
			detail += " (source tree dirty)"
		}
		return render.StatusRow{Level: "ok", Name: "build", Detail: detail}
	}
	return render.StatusRow{Level: "warn", Name: "build", Detail: fmt.Sprintf("installed %s ≠ source %s — run mu rebuild", ver, src)}
}

// checkRepoDrift reports a tracked repo as clean, dirty (uncommitted), and/or ahead of
// upstream (unpushed). Returns no row when dir isn't a git work tree (skip).
func checkRepoDrift(name, dir string) []render.StatusRow {
	if gitField(dir, "rev-parse", "--is-inside-work-tree") != "true" {
		return nil
	}
	var notes []string
	level := "ok"
	if gitField(dir, "status", "--porcelain", "--untracked-files=no") != "" {
		notes = append(notes, "uncommitted changes")
		level = "warn"
	}
	if ahead := gitField(dir, "rev-list", "--count", "@{u}..HEAD"); ahead != "" && ahead != "0" {
		notes = append(notes, ahead+" unpushed")
	}
	detail := "clean"
	if len(notes) > 0 {
		detail = strings.Join(notes, ", ")
	}
	return []render.StatusRow{{Level: level, Name: "repo " + name, Detail: detail}}
}

// toolName reduces a mise spec ("github:dandavison/delta@0.18.2", "difftastic@latest")
// to the short tool name mise ls lists it under.
func toolName(spec string) string {
	s := spec
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func tallyRows(rows []render.StatusRow) (ok, warn, fail int) {
	for _, r := range rows {
		switch r.Level {
		case "ok":
			ok++
		case "warn":
			warn++
		default:
			fail++
		}
	}
	return ok, warn, fail
}
