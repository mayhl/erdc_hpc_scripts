package cli

import (
	"reflect"
	"testing"
)

// splitRemotePath only fires for a path anchored with / ~ or $ AND already carrying a slash
// (the dir boundary); a bare word or a rootless "~"/"$VAR" gives nothing to list yet.
func TestSplitRemotePath(t *testing.T) {
	cases := []struct {
		in          string
		dir, prefix string
		ok          bool
	}{
		{"/p/work/scr", "/p/work/", "scr", true},
		{"/p/work/", "/p/work/", "", true},
		{"~/proj/ca", "~/proj/", "ca", true},
		{"$WORKDIR/r", "$WORKDIR/", "r", true},
		{"scratch", "", "", false}, // bare word — no root to anchor
		{"~", "", "", false},       // no slash yet
		{"$WORKDIR", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		dir, prefix, ok := splitRemotePath(c.in)
		if dir != c.dir || prefix != c.prefix || ok != c.ok {
			t.Errorf("splitRemotePath(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, dir, prefix, ok, c.dir, c.prefix, c.ok)
		}
	}
}

// rpathSafe permits path/var characters (the dir is expanded unquoted on the remote shell)
// and rejects anything that could inject.
func TestRpathSafe(t *testing.T) {
	for _, s := range []string{"/p/work/", "~/proj_1/", "$WORKDIR/a.b-c/"} {
		if !rpathSafe(s) {
			t.Errorf("rpathSafe(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"/p/`whoami`/", "/a;rm/", "/a b/", "/a$(x)/", "/a|b/"} {
		if rpathSafe(s) {
			t.Errorf("rpathSafe(%q) = true, want false", s)
		}
	}
}

// filterRemoteEntries drops ./ and ../, honors the prefix, rejoins the dir, and (dirsOnly)
// keeps only the trailing-slash directory entries.
func TestFilterRemoteEntries(t *testing.T) {
	lines := []string{"./", "../", "case_a/", "case_b/", "notes.txt", "cache/"}
	// dirs only, prefix "ca"
	got := filterRemoteEntries(lines, "/p/", "ca", true)
	want := []string{"/p/case_a/", "/p/case_b/", "/p/cache/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dirs-only = %v, want %v", got, want)
	}
	// files + dirs, prefix "" → everything but ./ ../
	got = filterRemoteEntries(lines, "/p/", "", false)
	want = []string{"/p/case_a/", "/p/case_b/", "/p/notes.txt", "/p/cache/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("all-entries = %v, want %v", got, want)
	}
	// a prefix that matches a file only, dirsOnly → empty
	if got := filterRemoteEntries(lines, "/p/", "notes", true); len(got) != 0 {
		t.Errorf("dirs-only on a file prefix = %v, want empty", got)
	}
}
