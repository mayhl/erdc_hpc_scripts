package cli

import (
	"reflect"
	"testing"

	"github.com/mayhl/mayhl_utils/internal/sshfs"
)

func groupsFixture() map[string]sshfs.Mount {
	return map[string]sshfs.Mount{
		"home_b":  {Groups: []string{"home"}},
		"home_c":  {Groups: []string{"add", "home"}},
		"work_c":  {Groups: []string{"sarcarst"}},
		"work_n":  {Groups: []string{"sarcarst"}},
		"scratch": {},
	}
}

func TestAllGroupsAndNames(t *testing.T) {
	reg := groupsFixture()
	want := map[string][]string{
		"add": {"home_c"}, "home": {"home_b", "home_c"}, "sarcarst": {"work_c", "work_n"},
	}
	if got := allGroups(reg); !reflect.DeepEqual(got, want) {
		t.Errorf("allGroups = %v, want %v", got, want)
	}
	if got := groupNames(reg); !reflect.DeepEqual(got, []string{"add", "home", "sarcarst"}) {
		t.Errorf("groupNames = %v", got)
	}
}

func TestRenameGroup(t *testing.T) {
	reg := groupsFixture()
	if n := renameGroup(reg, "sarcarst", "sarcast"); n != 2 {
		t.Fatalf("renamed %d, want 2", n)
	}
	groups := allGroups(reg)
	if _, ok := groups["sarcarst"]; ok {
		t.Error("old name survived")
	}
	if !reflect.DeepEqual(groups["sarcast"], []string{"work_c", "work_n"}) {
		t.Errorf("sarcast members = %v", groups["sarcast"])
	}
	if n := renameGroup(reg, "nope", "x"); n != 0 {
		t.Errorf("renaming a missing group changed %d", n)
	}
}

func TestDropGroupAll(t *testing.T) {
	reg := groupsFixture()
	if n := dropGroupAll(reg, "add"); n != 1 {
		t.Fatalf("dropped %d, want 1", n)
	}
	if !reflect.DeepEqual(reg["home_c"].Groups, []string{"home"}) {
		t.Errorf("home_c groups = %v, want [home]", reg["home_c"].Groups)
	}
	if n := dropGroupAll(reg, "add"); n != 0 {
		t.Errorf("second drop changed %d", n)
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		d    int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"sarcast", "sarcarst", 1},
		{"sarcast", "sarcats", 2},
		{"home", "home", 0},
		{"add", "all", 2},
		{"kitten", "sitting", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.d {
			t.Errorf("editDistance(%q,%q) = %d, want %d", c.a, c.b, got, c.d)
		}
	}
}

func TestNearGroup(t *testing.T) {
	existing := []string{"add", "home", "sarcast", "tsunami"}
	cases := map[string]string{
		"sarcarst": "sarcast", // one insertion — the real typo
		"sarcats":  "sarcast", // two edits on a long name
		"hom":      "home",    // one edit on a short name
		"all":      "",        // two edits on a short name: not flagged
		"navdrift": "",
		"sarcast":  "", // identical is not "near" (caller handles exists)
	}
	for g, want := range cases {
		if got := nearGroup(existing, g); got != want {
			t.Errorf("nearGroup(%q) = %q, want %q", g, got, want)
		}
	}
}

func TestExpandMountArgsGroupAndNames(t *testing.T) {
	// expandMountArgs reads the live registry; the pure inverse is covered above, so
	// here only the dedup/order contract on a synthetic registry via groupMembers.
	reg := groupsFixture()
	if got := groupMembers(reg, "home"); !reflect.DeepEqual(got, []string{"home_b", "home_c"}) {
		t.Errorf("groupMembers = %v", got)
	}
	if got := groupMembers(reg, "none"); len(got) != 0 {
		t.Errorf("missing group members = %v", got)
	}
}
