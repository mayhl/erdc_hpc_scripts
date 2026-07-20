package proc

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
		want Process
	}{
		{
			"plain", "  100 1 alice Ss 03:15:22 /usr/sbin/thing", true,
			Process{PID: 100, PPID: 1, User: "alice", State: "Ss", Elapsed: "03:15:22", Name: "thing", Command: "/usr/sbin/thing"},
		},
		{
			"command with args and spaces", "4501 4400 bob R+ 2-03:15:22 python3 run.py --case a b", true,
			Process{PID: 4501, PPID: 4400, User: "bob", State: "R+", Elapsed: "2-03:15:22", Name: "python3", Command: "python3 run.py --case a b"},
		},
		{
			"name is arg0 basename", "7 1 root S 00:01 /opt/models/funwave -i in.txt", true,
			Process{PID: 7, PPID: 1, User: "root", State: "S", Elapsed: "00:01", Name: "funwave", Command: "/opt/models/funwave -i in.txt"},
		},
		{"short line rejected", "100 1 alice Ss 03:15:22", false, Process{}},
		{"non-numeric pid rejected", "abc 1 alice Ss 03:15:22 thing", false, Process{}},
		{"empty line rejected", "", false, Process{}},
		{"blank line rejected", "   ", false, Process{}},
	}
	for _, c := range cases {
		got, ok := parseLine(c.line)
		if ok != c.ok {
			t.Errorf("%s: parseLine(%q) ok = %v, want %v", c.name, c.line, ok, c.ok)
			continue
		}
		if ok && got != c.want {
			t.Errorf("%s: parseLine(%q) = %+v, want %+v", c.name, c.line, got, c.want)
		}
	}
}

func TestArg0(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"/usr/bin/python3 run.py", "/usr/bin/python3"},
		{"bare", "bare"},
		{"", ""},
	}
	for _, c := range cases {
		if got := arg0(c.cmd); got != c.want {
			t.Errorf("arg0(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestPsArgs(t *testing.T) {
	// per-OS flag sets diverge; pin the invariants both share
	args := psArgs()
	if len(args) != 2 {
		t.Fatalf("psArgs() = %v, want flag + column spec", args)
	}
	cols := strings.Split(args[1], ",")
	for _, want := range []string{"pid=", "ppid=", "user=", "etime="} {
		if !slices.Contains(cols, want) {
			t.Errorf("psArgs() columns %q missing %q", args[1], want)
		}
	}
}

// smoke: real ps parses, and our own PID is excluded
func TestListSmoke(t *testing.T) {
	ps, err := List()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(ps) == 0 {
		t.Fatal("List(): no processes parsed")
	}
	self := os.Getpid()
	for _, p := range ps {
		if p.PID == self {
			t.Fatalf("List() includes our own PID %d", self)
		}
		if p.PID <= 0 {
			t.Fatalf("List() parsed a non-positive PID: %+v", p)
		}
	}
}
