package modules

import "testing"

func TestEnabled(t *testing.T) {
	cases := []struct {
		name string
		env  string
		ask  string
		want bool
	}{
		{"space separated", "fmt git", "git", true},
		{"comma separated", "fmt,git", "git", true},
		{"comma+space mix", "fmt, git", "git", true},
		{"tab separated", "fmt\tgit", "git", true},
		{"case-insensitive entry", "FMT Git", "git", true},
		{"case-insensitive query", "fmt git", "GIT", true},
		{"query whitespace trimmed", "fmt git", " git ", true},
		{"not listed", "fmt", "git", false},
		{"no substring match", "github", "git", false},
		{"empty env", "", "git", false},
		{"empty name", "fmt git", "", false},
		{"blank name", "fmt git", "  ", false},
	}
	for _, c := range cases {
		t.Setenv("MU_MODULES", c.env)
		if got := Enabled(c.ask); got != c.want {
			t.Errorf("%s: MU_MODULES=%q Enabled(%q) = %v, want %v", c.name, c.env, c.ask, got, c.want)
		}
	}
}
