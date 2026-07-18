package tomledit

import (
	"strings"
	"testing"
)

// sample mirrors the shape of a real config.toml: comment-dense, `=`-aligned in places,
// indented node blocks, and commented-out placeholders — every layout the surgeon must
// leave alone.
const sample = `# mayhl_utils configuration (real values; gitignored).

hpc_user = "someuser"
fleet = ["node-a", "node-b"]

[transfer]
rsync_opts        = "-au --partial"
ssh_transfer_opts = "-q"   # quiet ssh

[[cluster]]
name   = "dsrc1"
domain = "dsrc1.example.hpc.mil"
scheduler = "pbs"

  [[cluster.node]]
  name    = "node-a"
  # account        = "…"
  # cores_per_node = …

[[cluster]]
name   = "dsrc2"
domain = "dsrc2.example.hpc.mil"

  [[cluster.node]]
  name    = "node-b"
  account = "FILL-ME"   # ← subproject to charge
`

// TestRoundTrip is the guarantee the whole package exists for: parse and re-render an
// untouched file and get the same bytes — comments, alignment, blank lines and all.
func TestRoundTrip(t *testing.T) {
	if got := Parse(sample).String(); got != sample {
		t.Errorf("round-trip changed the file:\n%s", got)
	}
}

func TestFindAndOwner(t *testing.T) {
	d := Parse(sample)

	if got := len(d.Tables("cluster")); got != 2 {
		t.Errorf("Tables(cluster) = %d, want 2", got)
	}
	dsrc2 := d.Find("cluster", "name", "dsrc2")
	if dsrc2 < 0 {
		t.Fatal("Find(cluster dsrc2) missed")
	}
	// An array table is addressed by identity, so the SECOND [[cluster]] is found by name.
	if v, ok := d.Value(dsrc2, "domain"); !ok || Unquote(v) != "dsrc2.example.hpc.mil" {
		t.Errorf("dsrc2 domain = %q,%v", v, ok)
	}
	// A node block belongs to the cluster above it, not to the file.
	nodeB := d.Find("cluster.node", "name", "node-b")
	if nodeB < 0 {
		t.Fatal("Find(cluster.node nodeB) missed")
	}
	if owner := d.Owner(nodeB); owner != dsrc2 {
		t.Errorf("Owner(nodeB) = %d, want dsrc2 (%d)", owner, dsrc2)
	}
	if owner := d.Owner(d.Find("cluster", "name", "dsrc1")); owner != -1 {
		t.Errorf("Owner(a top-level cluster) = %d, want -1", owner)
	}
	// A commented-out key is not a value.
	nodeA := d.Find("cluster.node", "name", "node-a")
	if _, ok := d.Value(nodeA, "account"); ok {
		t.Error("a commented-out account read as set")
	}
	// The value strips its trailing comment but keeps the string intact.
	if v, ok := d.Value(nodeB, "account"); !ok || Unquote(v) != "FILL-ME" {
		t.Errorf("nodeB account = %q,%v", v, ok)
	}
}

func TestSet(t *testing.T) {
	d := Parse(sample)

	// (1) Live key: the value changes, the trailing comment and the `=` alignment survive.
	d.Set(d.Find("cluster.node", "name", "node-b"), "account", Quote("ALLOC-9"))
	// (2) Commented-out placeholder: revived in place, indent kept, sample value dropped.
	nodeA := d.Find("cluster.node", "name", "node-a")
	d.Set(nodeA, "cores_per_node", "128")
	// (3) Absent key: appended to the end of that table's body, at the table's indent.
	d.Set(nodeA, "scheduler", Quote("pbs"))
	// (4) Root scalar, aligned table value.
	d.Set(0, "hpc_user", Quote("newuser"))
	d.Set(d.Tables("transfer")[0], "ssh_transfer_opts", Quote("-qq"))

	out := d.String()
	for _, want := range []string{
		`  account = "ALLOC-9"   # ← subproject to charge`, // comment preserved
		"  cores_per_node = 128",                           // revived at its placeholder's indent
		`  scheduler = "pbs"`,                              // appended inside the node block
		`hpc_user = "newuser"`,
		`ssh_transfer_opts = "-qq"   # quiet ssh`, // alignment padding preserved
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The appended key must land INSIDE nodeA's block, not after the blank line that
	// separates it from the next [[cluster]] — otherwise it silently joins the wrong table.
	if i, j := strings.Index(out, `  scheduler = "pbs"`), strings.Index(out, `[[cluster]]
name   = "dsrc2"`); i > j {
		t.Error("appended key escaped its table into the next cluster")
	}
	// Nothing else moved.
	if !strings.Contains(out, `rsync_opts        = "-au --partial"`) {
		t.Error("an untouched line was reformatted")
	}
	if strings.Count(out, "# mayhl_utils configuration") != 1 {
		t.Error("the header comment was lost or duplicated")
	}
}

// TestAssignsPrecision guards the key matcher: `account` must not match `account_id`, and a
// commented placeholder must not shadow a live assignment of the same key.
func TestAssignsPrecision(t *testing.T) {
	d := Parse("[t]\naccount_id = 3\n# key = 1\nkey = 2\n")
	if _, ok := d.Value(d.Tables("t")[0], "account"); ok {
		t.Error("account matched account_id")
	}
	if v, ok := d.Value(d.Tables("t")[0], "key"); !ok || v != "2" {
		t.Errorf("live key = %q,%v — the placeholder shadowed it", v, ok)
	}
	d.Set(d.Tables("t")[0], "key", "9")
	if got := d.String(); !strings.Contains(got, "# key = 1\nkey = 9") {
		t.Errorf("Set hit the wrong line:\n%s", got)
	}
}

func TestSplitComment(t *testing.T) {
	for _, c := range []struct{ in, val, comment string }{
		{` "x"   # note`, ` "x"   `, "# note"},
		{` "a#b"`, ` "a#b"`, ""},
		{` 128`, ` 128`, ""},
	} {
		v, cm := splitComment(c.in)
		if v != c.val || cm != c.comment {
			t.Errorf("splitComment(%q) = %q,%q want %q,%q", c.in, v, cm, c.val, c.comment)
		}
	}
}

// clusterDoc mirrors the config.toml cluster shape (placeholder names only): a cluster with
// two indented node blocks, separated by blanks, then a second cluster.
const clusterDoc = `[[cluster]]
name  = "dsrc1"
nodes = ["node-a", "node-b", "node-c"]

  [[cluster.node]]
  name    = "node-a"
  account = "ACCT00000000"

  [[cluster.node]]
  name    = "node-b"
  account = "ACCT00000000"

[[cluster]]
name = "dsrc2"
`

func TestInsertTable(t *testing.T) {
	d := Parse(clusterDoc)
	c1 := d.Find("cluster", "name", "dsrc1")
	i := d.InsertTable(c1, "cluster.node", [][2]string{{"name", `"node-c"`}})
	d.Set(i, "account", `"ACCT00000000"`)

	got := d.String()
	// the new block lands inside the dsrc1 group (after node-b, before the blank + dsrc2),
	// two-space indented like its siblings
	if !strings.Contains(got, "  name    = \"node-b\"\n  account = \"ACCT00000000\"\n\n  [[cluster.node]]\n  name = \"node-c\"") {
		t.Fatalf("misplaced/misindented insert:\n%s", got)
	}
	if strings.Contains(got, "[[cluster.node]]\nname = \"node-c\"") { // must NOT be at column 0
		t.Fatalf("insert not indented:\n%s", got)
	}
	// dsrc2 is untouched and still parses as its own cluster
	if d.Find("cluster", "name", "dsrc2") < 0 {
		t.Fatalf("dsrc2 lost after insert:\n%s", got)
	}
	// the returned index addresses the new block
	if v, ok := d.Value(i, "name"); !ok || Unquote(v) != "node-c" {
		t.Fatalf("returned index wrong: %q,%v", v, ok)
	}
}

func TestDeleteTable(t *testing.T) {
	d := Parse(clusterDoc)
	nb := d.Find("cluster.node", "name", "node-b")
	d.DeleteTable(nb)
	got := d.String()
	// the node-b BLOCK is gone (its name stays in the nodes array — decommission moves that
	// separately); node-a + dsrc2 survive, one blank separator remains
	if d.Find("cluster.node", "name", "node-b") >= 0 {
		t.Fatalf("node-b block not removed:\n%s", got)
	}
	if d.Find("cluster.node", "name", "node-a") < 0 || d.Find("cluster", "name", "dsrc2") < 0 {
		t.Fatalf("collateral damage:\n%s", got)
	}
	if !strings.Contains(got, "account = \"ACCT00000000\"\n\n[[cluster]]\nname = \"dsrc2\"") {
		t.Fatalf("separator collapsed after delete:\n%s", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("double blank left after delete:\n%s", got)
	}
}
