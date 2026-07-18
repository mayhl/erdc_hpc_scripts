package cli

import (
	"strings"
	"testing"

	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/tomledit"
)

const cfgSample = `hpc_user = "someuser"

[transfer]
rsync_opts = "-au"

[[cluster]]
name      = "dsrc1"
domain    = "dsrc1.example.hpc.mil"
scheduler = "pbs"
account   = "CLUSTER-ALLOC"

  [[cluster.node]]
  name    = "node-a"
  account = "NODE-ALLOC"

  [[cluster.node]]
  name = "node-b"
`

// find returns the leaf at a path in the built tree, so a test can assert on what the panel
// would actually show.
func findLeaf(t *testing.T, doc *tomledit.Doc, path ...string) (value, origin string) {
	t.Helper()
	root, _ := buildTree(doc, false)
	nodes := root
	for d, want := range path {
		for _, n := range nodes {
			// Section labels carry decoration ("[[cluster]] dsrc1"), leaves don't.
			if n.Label != want && !strings.HasSuffix(n.Label, " "+want) {
				continue
			}
			if d == len(path)-1 {
				if n.Field == nil {
					t.Fatalf("%v is a section, not a leaf", path)
				}
				return n.Field.Value, n.Origin
			}
			nodes = n.Children
			break
		}
	}
	t.Fatalf("no leaf at %v", path)
	return "", ""
}

// TestBuildTreeProvenance is the panel's reason to exist: a node's resolved value and the
// SCOPE it came from — invisible in the file, decisive for what a lookup returns.
func TestBuildTreeProvenance(t *testing.T) {
	doc := tomledit.Parse(cfgSample)

	// node-a overrides account → its own value, no note.
	if v, o := findLeaf(t, doc, "dsrc1", "node-a", "account"); v != "NODE-ALLOC" || o != "" {
		t.Errorf("node-a account = %q (%q), want its own override", v, o)
	}
	// node-b doesn't → it shows the CLUSTER's value, marked as inherited and attributed.
	wantOrigin := render.Glyph("↳ ", "< ") + "from dsrc1"
	if v, o := findLeaf(t, doc, "dsrc1", "node-b", "account"); v != "CLUSTER-ALLOC" || o != wantOrigin {
		t.Errorf("node-b account = %q (%q), want the cluster's, attributed", v, o)
	}
	// Neither scope sets cores_per_node.
	if v, o := findLeaf(t, doc, "dsrc1", "node-b", "cores_per_node"); v != "" || o != "unset" {
		t.Errorf("node-b cores_per_node = %q (%q), want unset", v, o)
	}
	// A table absent from the file is not offered at all (creating it is a hand-edit).
	root, _ := buildTree(doc, false)
	for _, n := range root {
		if n.Label == "[sshfs]" {
			t.Error("offered [sshfs], which the file doesn't have")
		}
	}
}

// TestApplyChanges walks the write-back path a save takes: a Change on an INHERITED value
// must write an override into the node's own block, not touch the cluster's line.
func TestApplyChanges(t *testing.T) {
	doc := tomledit.Parse(cfgSample)
	_, targets := buildTree(doc, false)

	apply := func(path []string, val string) {
		tgt, ok := targets[strings.Join(path, "\x00")]
		if !ok {
			t.Fatalf("no target for %v", path)
		}
		raw := val
		if tgt.quoted {
			raw = tomledit.Quote(val)
		}
		doc.Set(tgt.table, tgt.key, raw)
	}
	apply([]string{"dsrc1", "node-b", "account"}, "NEW-ALLOC")  // was inherited
	apply([]string{"dsrc1", "node-a", "cores_per_node"}, "192") // was absent
	apply([]string{"transfer", "rsync_opts"}, "-au --partial")  // plain replace
	out := doc.String()

	if !strings.Contains(out, "  name = \"node-b\"\n  account = \"NEW-ALLOC\"") {
		t.Errorf("the override didn't land in node-b's block:\n%s", out)
	}
	// The cluster's own account must be untouched — an override is not an edit of the default.
	if !strings.Contains(out, `account   = "CLUSTER-ALLOC"`) {
		t.Errorf("writing a node override rewrote the cluster's default:\n%s", out)
	}
	if !strings.Contains(out, "  cores_per_node = 192") { // bare int, not quoted
		t.Errorf("cores_per_node not written as a bare int:\n%s", out)
	}
	if !strings.Contains(out, `rsync_opts = "-au --partial"`) {
		t.Errorf("rsync_opts not replaced:\n%s", out)
	}
	if strings.Count(out, `name    = "node-a"`) != 1 {
		t.Errorf("the document was restructured:\n%s", out)
	}
}

// TestEveryLeafHasATarget is the invariant a save rests on: the panel hands back a Change
// keyed by the leaf's PATH, and configEdit looks that path up in targets — so a leaf whose
// path isn't a key there is one the user can edit and watch vanish, with no error and no
// diff, indistinguishable from a cancel. The paths here are built exactly as the widget
// builds them (Key, falling back to Label), which is what the hand-written paths in
// TestApplyChanges could not catch: section labels are DECORATED ("[[cluster]] dsrc1"), and
// for a while the decoration leaked into the path and missed every target.
func TestEveryLeafHasATarget(t *testing.T) {
	doc := tomledit.Parse(cfgSample)
	root, targets := buildTree(doc, false)

	leaves := 0
	var walk func(nodes []render.EditorNode, prefix []string)
	walk = func(nodes []render.EditorNode, prefix []string) {
		for _, n := range nodes {
			key := n.Key
			if key == "" {
				key = n.Label
			}
			path := append(append([]string(nil), prefix...), key)
			if n.Field == nil {
				walk(n.Children, path)
				continue
			}
			leaves++
			if _, ok := targets[strings.Join(path, "\x00")]; !ok {
				t.Errorf("leaf %v has no write-back target — an edit to it would silently vanish", path)
			}
		}
	}
	walk(root, nil)
	if leaves == 0 {
		t.Fatal("no leaves built — the sample config isn't exercising the tree")
	}
}

func TestIntOrEmpty(t *testing.T) {
	for _, ok := range []string{"", "0", "128", " 192 "} {
		if msg := intOrEmpty(ok, nil); msg != "" {
			t.Errorf("intOrEmpty(%q) = %q, want accepted", ok, msg)
		}
	}
	for _, bad := range []string{"-1", "many", "1.5"} {
		if msg := intOrEmpty(bad, nil); msg == "" {
			t.Errorf("intOrEmpty(%q) accepted", bad)
		}
	}
}

func TestUnconfiguredNodes(t *testing.T) {
	const doc = `[[cluster]]
name  = "dsrc1"
nodes = ["node-a", "node-b", "node-c"]

  [[cluster.node]]
  name = "node-a"

[[cluster]]
name  = "dsrc2"
nodes = ["node-d"]
`
	d := tomledit.Parse(doc)
	// node-a has a block; node-b and node-c are listed-but-unconfigured, in array order
	if got := unconfiguredNodes(d, d.Find("cluster", "name", "dsrc1")); strings.Join(got, ",") != "node-b,node-c" {
		t.Fatalf("dsrc1 unconfigured = %v, want [node-b node-c]", got)
	}
	// dsrc2's lone node has no block → unconfigured
	if got := unconfiguredNodes(d, d.Find("cluster", "name", "dsrc2")); strings.Join(got, ",") != "node-d" {
		t.Fatalf("dsrc2 unconfigured = %v", got)
	}
}

func TestArrayMembers(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`["a", "b", "c"]`, "a,b,c"},
		{`[ 'x' , "y" ]`, "x,y"},
		{`[]`, ""},
		{`["only"]`, "only"},
	} {
		if got := strings.Join(arrayMembers(c.in), ","); got != c.want {
			t.Errorf("arrayMembers(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestApplyChangesCreatesNodeBlock(t *testing.T) {
	d := tomledit.Parse("[[cluster]]\nname  = \"dsrc1\"\nnodes = [\"node-a\"]\n")
	path := []string{"dsrc1", "node-a", "cores_per_node"}
	targets := map[string]target{
		strings.Join(path, "\x00"): {table: -1, key: "cores_per_node", clusterName: "dsrc1", node: "node-a"},
	}
	applyChanges(d, targets, []render.Change{{Path: path, New: "128"}})
	got := d.String()
	ni := d.Find("cluster.node", "name", "node-a")
	if ni < 0 {
		t.Fatalf("block not created on first edit:\n%s", got)
	}
	if v, ok := d.Value(ni, "cores_per_node"); !ok || v != "128" {
		t.Fatalf("cores not written: %q %v\n%s", v, ok, got)
	}
}

// TestApplyChangesMultipleNewNodes exercises the index-robustness the two-phase apply exists
// for: each InsertTable re-parses, so the second cluster must be re-resolved by name.
func TestApplyChangesMultipleNewNodes(t *testing.T) {
	d := tomledit.Parse("[[cluster]]\nname  = \"dsrc1\"\nnodes = [\"a\"]\n\n[[cluster]]\nname  = \"dsrc2\"\nnodes = [\"b\"]\n")
	targets := map[string]target{}
	mk := func(cl, nd, k string) []string {
		p := []string{cl, nd, k}
		targets[strings.Join(p, "\x00")] = target{table: -1, key: k, clusterName: cl, node: nd}
		return p
	}
	applyChanges(d, targets, []render.Change{
		{Path: mk("dsrc1", "a", "cores_per_node"), New: "64"},
		{Path: mk("dsrc2", "b", "cores_per_node"), New: "128"},
	})
	got := d.String()
	a, b := d.Find("cluster.node", "name", "a"), d.Find("cluster.node", "name", "b")
	if a < 0 || b < 0 {
		t.Fatalf("missing blocks:\n%s", got)
	}
	if va, _ := d.Value(a, "cores_per_node"); va != "64" {
		t.Fatalf("a cores %q\n%s", va, got)
	}
	if vb, _ := d.Value(b, "cores_per_node"); vb != "128" {
		t.Fatalf("b cores %q\n%s", vb, got)
	}
	// each block landed under its OWN cluster (not swapped by a stale index)
	if d.Owner(a) != d.Find("cluster", "name", "dsrc1") || d.Owner(b) != d.Find("cluster", "name", "dsrc2") {
		t.Fatalf("blocks under wrong cluster:\n%s", got)
	}
}

func TestDecommissionNode(t *testing.T) {
	d := tomledit.Parse("[[cluster]]\nname  = \"dsrc1\"\nnodes = [\"node-a\", \"node-b\"]\n\n  [[cluster.node]]\n  name = \"node-b\"\n")
	cname, ok := decommissionNode(d, "node-b")
	if !ok || cname != "dsrc1" {
		t.Fatalf("decommission returned %q,%v", cname, ok)
	}
	got := d.String()
	ci := d.Find("cluster", "name", "dsrc1")
	if findNodeBlock(d, ci, "node-b") >= 0 {
		t.Fatalf("block not dropped:\n%s", got)
	}
	if nodes := arrayMembers(mustValue(d, ci, "nodes")); sliceHas(nodes, "node-b") || !sliceHas(nodes, "node-a") {
		t.Fatalf("nodes = %v\n%s", nodes, got)
	}
	if dec := decommissionedNodes(d, ci); !sliceHas(dec, "node-b") {
		t.Fatalf("decommissioned = %v\n%s", dec, got)
	}
	// unknown node → not found, no change
	if _, ok := decommissionNode(d, "nope"); ok {
		t.Fatal("expected not-found for an unknown node")
	}
}

func TestTomlArray(t *testing.T) {
	if got := tomlArray([]string{"a", "b"}); got != `["a", "b"]` {
		t.Fatalf("tomlArray = %q", got)
	}
	if got := tomlArray(nil); got != "[]" {
		t.Fatalf("empty tomlArray = %q", got)
	}
}
