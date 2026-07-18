package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/render"
	"github.com/mayhl/mayhl_utils/internal/tomledit"
)

// cfgKey is one editable field of the config schema: how it's entered, how it's checked, and
// whether TOML wants it quoted. Scalars, plus (v2) whole arrays (fleet, nodes,
// decommissioned) and inline tables (submit_queue, queue_class) edited via arrKey/mapKey in
// the Editor's focused list/map sub-panel.
type cfgKey struct {
	name     string
	kind     render.FieldKind
	options  []string
	hint     string
	validate func(string, []string) string
	quoted   bool // a TOML string; false = bare int/bool
}

func strKey(name, hint string) cfgKey {
	return cfgKey{name: name, kind: render.FieldText, hint: hint, quoted: true}
}

func intKey(name, hint string) cfgKey {
	return cfgKey{name: name, kind: render.FieldText, hint: hint, validate: intOrEmpty}
}

// wallKey is a duration the schedulers will have to accept — checked here, in the panel,
// rather than surfacing as a scheduler rejection minutes into a submit.
func wallKey(name, hint string) cfgKey {
	return cfgKey{name: name, kind: render.FieldText, hint: hint, quoted: true, validate: walltimeField}
}

func enumKey(name string, options []string) cfgKey {
	return cfgKey{name: name, kind: render.FieldEnum, options: options, quoted: true}
}

// arrKey / mapKey edit a whole TOML array (fleet, nodes, decommissioned) or inline table
// (submit_queue, queue_class) through the Editor's focused list/map sub-panel — quoted:false,
// since the sub-editor re-serializes the literal itself and hands it back as Value. The shape
// validators stay as a backstop (they run on that serialized literal, which is always
// well-formed); this also surfaces fleet, which used to be a hand-edit-only key.
func arrKey(name, hint string) cfgKey {
	return cfgKey{name: name, kind: render.FieldList, hint: hint, validate: arrayField}
}

func mapKey(name, hint string) cfgKey {
	return cfgKey{name: name, kind: render.FieldMap, hint: hint, validate: mapField}
}

// arrayField / mapField loosely check the literal's shape so a fat-fingered edit is caught
// in the panel, not as a parse failure on next load. Empty clears the key.
func arrayField(v string, _ []string) string {
	if s := strings.TrimSpace(v); s != "" && (!strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]")) {
		return `a TOML array: ["a", "b"]`
	}
	return ""
}

func mapField(v string, _ []string) string {
	if s := strings.TrimSpace(v); s != "" && (!strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}")) {
		return `an inline table: { q = "name" }`
	}
	return ""
}

// intOrEmpty accepts a non-negative integer or nothing (clearing a key is a valid edit).
func intOrEmpty(v string, _ []string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err != nil || n < 0 {
		return "a whole number"
	}
	return ""
}

// The schema, by scope. Only keys of tables that ALREADY EXIST in the file are offered:
// creating a table (a new cluster or machine) stays a hand-edit, so the panel never has to
// invent a block's placement or its comments.
var (
	rootKeys = []cfgKey{
		strKey("hpc_user", "HPC login name"),
		arrKey("fleet", `--fleet query set: ["a", "b"]`),
	}

	tableKeys = map[string][]cfgKey{
		"transfer": {strKey("rsync_opts", ""), strKey("ssh_transfer_opts", "")},
		"sshfs":    {strKey("root", "local mount parent")},
		"ssh":      {strKey("ossh", "Kerberos ssh build")},
		"shell":    {enumKey("queue_aliases", []string{"pbs", "slurm", "both"})},
		"project": {
			strKey("case_glob", ""), strKey("data_dir", ""),
			strKey("tar_parent_threshold", "e.g. 1GB"), strKey("tar_hook_threshold", "e.g. 100GB"),
			strKey("watch_interval", "e.g. 60s"),
			{name: "job_hooks", kind: render.FieldEnum, options: []string{"true", "false"}},
		},
	}

	clusterKeys = []cfgKey{
		strKey("domain", ""),
		enumKey("scheduler", []string{"pbs", "slurm"}),
		strKey("account", "default allocation"),
		wallKey("interactive_walltime", "held session: 1h, 45m…"),
		enumKey("queue_flag", []string{"partition", "qos"}),
		intKey("cores_per_node", "→ MaxNodes"),
		{name: "active", kind: render.FieldEnum, options: []string{"true", "false"}},
		arrKey("nodes", `machines: ["a", "b"]`),
		arrKey("decommissioned", "retired machines (or use --decommission)"),
		mapKey("submit_queue", `{ default = "standard", debug = "debug" }`),
		mapKey("queue_class", `{ standard = "…" }`),
	}

	// A node inherits every one of these from its cluster, so each may be left blank.
	nodeKeys = []cfgKey{
		enumKey("scheduler", []string{"", "pbs", "slurm"}),
		strKey("account", "this machine's allocation"),
		wallKey("interactive_walltime", "this machine's held session"),
		enumKey("queue_flag", []string{"", "partition", "qos"}),
		intKey("cores_per_node", "cores on THIS machine"),
		mapKey("submit_queue", `this machine's { default = "…" }`),
		mapKey("queue_class", `{ standard = "…" }`),
	}
)

// acctKey specializes the `account` key against a cluster's cached subprojects: with a
// cache it becomes a picker — `show_usage` IS the list of allocations you can charge, so
// there is nothing to type — and without one it stays free text, which is also what happens
// on a machine mu has never fetched usage from. A node's account may always be cleared back
// to the cluster's, so its picker leads with the blank.
func acctKey(k cfgKey, accts []string, node bool) cfgKey {
	if k.name != "account" || len(accts) == 0 {
		return k
	}
	k.kind, k.hint = render.FieldEnum, "from show_usage"
	k.options = accts
	if node {
		k.options = append([]string{""}, accts...)
	}
	return k
}

// target is where a tree leaf writes back to: a table in the document, and the key in it.
type target struct {
	table  int
	key    string
	quoted bool
	// A leaf on an UNCONFIGURED node has no block yet (table == -1): its first edited key
	// creates the [[cluster.node]] block. clusterName+node identify where, resolved by name
	// at write time because InsertTable re-parses and renumbers table indices.
	clusterName string
	node        string
}

// configCmd is `mu config`: show the resolved config, or edit it in the panel. The two-scope
// model (cluster default, node override) makes the resolved value genuinely hard to read off
// the file, which is why `show` annotates every value with where it came from.
func configCmd() *cobra.Command {
	var interactive, allSystems bool
	var decommission string
	c := &cobra.Command{
		Use:   "config",
		Short: "Show or edit config.toml (values, with their scope).",
		Long: "Print config.toml's values annotated with the scope each resolves from — a node's\n" +
			"own block, its cluster's default, or a built-in — or edit them in place with -i.\n\n" +
			"Edits are surgical: mu rewrites only the lines you changed, so comments, ordering\n" +
			"and alignment survive. Nodes listed without a [[cluster.node]] block show as\n" +
			"unconfigured; editing one in -i creates its block. Decommissioned machines are\n" +
			"hidden unless -e/--all-systems; --decommission <node> retires one.\n\n" +
			"    mu config          # the resolved view\n" +
			"    mu config -i       # the panel\n" +
			"    mu config -e       # include decommissioned systems",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			switch {
			case decommission != "":
				return configDecommission(decommission)
			case interactive:
				return configEdit(allSystems)
			default:
				return configShow(allSystems)
			}
		},
	}
	f := c.Flags()
	f.BoolVarP(&interactive, "interactive", "i", false, "edit in the panel")
	f.BoolVarP(&allSystems, "all-systems", "e", false, "include decommissioned systems")
	f.StringVar(&decommission, "decommission", "", "retire a `node`: drop its block, move it nodes→decommissioned")
	return c
}

// configDoc reads and parses the live config.toml as text (never as a struct — see
// internal/tomledit).
func configDoc() (path, text string, doc *tomledit.Doc, err error) {
	path = config.Path()
	if path == "" {
		return "", "", nil, runErr("no config.toml — set MU_CONFIG_FILE, or create $MU_ROOT/config.toml from config.toml.example")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return "", "", nil, runErr("read %s: %s", path, e)
	}
	return path, string(b), tomledit.Parse(string(b)), nil
}

// buildTree walks the schema against the document and returns the Editor's tree plus the
// write-back target of every leaf, keyed by its path. A leaf's VALUE is what the key
// resolves to and its ORIGIN says where that came from — so an unset node key shows the
// cluster's value, and editing it writes the override into the node's own block.
func buildTree(doc *tomledit.Doc, showAll bool) ([]render.EditorNode, map[string]target) {
	targets := map[string]target{}
	var root []render.EditorNode

	leaf := func(path []string, t target, k cfgKey, value, origin string) render.EditorNode {
		targets[strings.Join(path, "\x00")] = t
		f := render.FormField{
			Label: k.name, Value: value, Kind: k.kind,
			Options: k.options, Hint: k.hint, Validate: k.validate,
		}
		return render.EditorNode{Label: k.name, Field: &f, Origin: origin}
	}
	// value reads a key straight from a table: set → its text and no note; unset → empty
	// and "unset", so the panel never implies a value the file doesn't hold.
	value := func(ti int, k cfgKey) (string, string) {
		if v, ok := doc.Value(ti, k.name); ok {
			return tomledit.Unquote(v), ""
		}
		return "", "unset"
	}

	// nodeNode builds one machine's subtree. ni>=0 is a real [[cluster.node]] block; ni<0 is
	// a node listed in `nodes` with no block yet — every key shows the value inherited from
	// the cluster, and its write target (table -1, clusterName+node) defers block creation to
	// the first edit. The inherited-value note (↳ from <cluster>) is the resolution the file
	// itself can't show.
	nodeNode := func(ci int, cname, nname string, ni int, accts []string) render.EditorNode {
		var nkids []render.EditorNode
		for _, k := range nodeKeys {
			k = acctKey(k, accts, true)
			var v, origin string
			ok := false
			if ni >= 0 {
				v, ok = doc.Value(ni, k.name)
			}
			switch {
			case ok:
				v = tomledit.Unquote(v)
			default:
				if cv, cok := doc.Value(ci, k.name); cok {
					v, origin = tomledit.Unquote(cv), render.Glyph("↳ ", "< ")+"from "+cname
				} else {
					v, origin = "", "unset"
				}
			}
			t := target{table: ni, key: k.name, quoted: k.quoted}
			if ni < 0 {
				t.clusterName, t.node = cname, nname
			}
			nkids = append(nkids, leaf([]string{cname, nname, k.name}, t, k, v, origin))
		}
		label, h := nname, render.HueLoc
		if ni < 0 {
			label, h = nname+render.Glyph("  · ", "  * ")+"unconfigured", render.HueDim
		}
		return render.EditorNode{Label: label, Key: nname, Hue: h, Children: nkids}
	}

	// Root scalars (hpc_user) live at the TOML top level, not under a [table], but a lone
	// top-level key reads as orphaned beside the section headers — so the panel gathers them
	// under a synthetic "[general]" heading. Display only: each still writes to table 0. The
	// section's Key keeps its decorated label out of the leaf paths (see the leaf-path fix).
	var general []render.EditorNode
	for _, k := range rootKeys {
		v, origin := value(0, k)
		general = append(general, leaf([]string{"general", k.name}, target{table: 0, key: k.name, quoted: k.quoted}, k, v, origin))
	}
	if len(general) > 0 {
		root = append(root, render.EditorNode{Label: "[general]", Key: "general", Hue: render.HueGroup, Children: general})
	}
	for _, name := range []string{"transfer", "sshfs", "ssh", "shell", "project"} {
		ts := doc.Tables(name)
		if len(ts) == 0 {
			continue // no such table in the file — creating one is a hand-edit
		}
		var kids []render.EditorNode
		for _, k := range tableKeys[name] {
			v, origin := value(ts[0], k)
			kids = append(kids, leaf([]string{name, k.name}, target{table: ts[0], key: k.name, quoted: k.quoted}, k, v, origin))
		}
		root = append(root, render.EditorNode{Label: "[" + name + "]", Key: name, Hue: render.HueGroup, Children: kids})
	}

	for _, ci := range doc.Tables("cluster") {
		cname := tableValue(doc, ci, "name")
		accts := clusterAccounts(cname, time.Now())
		var kids []render.EditorNode
		for _, k := range clusterKeys {
			k = acctKey(k, accts, false)
			v, origin := value(ci, k)
			kids = append(kids, leaf([]string{cname, k.name}, target{table: ci, key: k.name, quoted: k.quoted}, k, v, origin))
		}
		// The cluster's machines: a node block is owned by the cluster above it.
		for _, ni := range doc.Tables("cluster.node") {
			if doc.Owner(ni) != ci {
				continue
			}
			kids = append(kids, nodeNode(ci, cname, tableValue(doc, ni, "name"), ni, accts))
		}
		// Nodes listed in `nodes` with no block: editable-but-unconfigured — every key shows
		// the inherited value, and the first edit creates the block (v2a create-on-write).
		for _, nm := range unconfiguredNodes(doc, ci) {
			kids = append(kids, nodeNode(ci, cname, nm, -1, accts))
		}
		// Decommissioned machines are hidden unless -e/--all-systems: they're record-only
		// (a name in `decommissioned`, no block, not submittable).
		if showAll {
			for _, nm := range decommissionedNodes(doc, ci) {
				kids = append(kids, render.EditorNode{
					Label: nm + render.Glyph("  · ", "  * ") + "decommissioned",
					Key:   nm,
					Hue:   render.HueDim,
				})
			}
		}
		root = append(root, render.EditorNode{Label: "[[cluster]] " + cname, Key: cname, Hue: render.HueLoc, Children: kids})
	}
	return root, targets
}

// unconfiguredNodes returns cluster ci's `nodes`-array members that have no [[cluster.node]]
// block — the machines listed but never given per-node config, which resolve wholly to the
// cluster's defaults. Order follows the `nodes` array.
func unconfiguredNodes(doc *tomledit.Doc, ci int) []string {
	nodesRaw, ok := doc.Value(ci, "nodes")
	if !ok {
		return nil
	}
	configured := map[string]bool{}
	for _, ni := range doc.Tables("cluster.node") {
		if doc.Owner(ni) == ci {
			configured[tableValue(doc, ni, "name")] = true
		}
	}
	var out []string
	for _, nm := range arrayMembers(nodesRaw) {
		if !configured[nm] {
			out = append(out, nm)
		}
	}
	return out
}

// decommissionedNodes returns cluster ci's `decommissioned`-array members — machines that
// were retired from service, kept only as a record (no block). Shown solely under -e.
func decommissionedNodes(doc *tomledit.Doc, ci int) []string {
	if raw, ok := doc.Value(ci, "decommissioned"); ok {
		return arrayMembers(raw)
	}
	return nil
}

// decommissionNode retires a machine: drops its [[cluster.node]] block, removes its name
// from the owning cluster's `nodes` array, and appends it to that cluster's `decommissioned`
// array (created if absent). Returns the cluster name and true, or "",false if node is in no
// cluster's nodes list.
func decommissionNode(doc *tomledit.Doc, node string) (string, bool) {
	for _, ci := range doc.Tables("cluster") {
		raw, ok := doc.Value(ci, "nodes")
		if !ok || !sliceHas(arrayMembers(raw), node) {
			continue
		}
		cname := tableValue(doc, ci, "name")
		if ni := findNodeBlock(doc, ci, node); ni >= 0 {
			doc.DeleteTable(ni)                     // re-parses → re-resolve the cluster
			ci = doc.Find("cluster", "name", cname) // (its index is stable, but be explicit)
		}
		doc.Set(ci, "nodes", tomlArray(sliceRemove(arrayMembers(mustValue(doc, ci, "nodes")), node)))
		doc.Set(ci, "decommissioned", tomlArray(append(decommissionedNodes(doc, ci), node)))
		return cname, true
	}
	return "", false
}

// findNodeBlock is the [[cluster.node]] under cluster ci named name, or -1.
func findNodeBlock(doc *tomledit.Doc, ci int, name string) int {
	for _, ni := range doc.Tables("cluster.node") {
		if doc.Owner(ni) == ci && tableValue(doc, ni, "name") == name {
			return ni
		}
	}
	return -1
}

func mustValue(doc *tomledit.Doc, ci int, key string) string { v, _ := doc.Value(ci, key); return v }

// tomlArray renders names as a single-line TOML string array; empty → `[]`.
func tomlArray(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	qs := make([]string, len(names))
	for i, n := range names {
		qs[i] = tomledit.Quote(n)
	}
	return "[" + strings.Join(qs, ", ") + "]"
}

func sliceHas(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func sliceRemove(s []string, v string) []string {
	out := s[:0:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// arrayMembers parses a single-line TOML string array (`["a", "b"]`) into its members —
// enough for the config's flat name arrays (nodes, decommissioned, fleet), not a general
// TOML array parser.
func arrayMembers(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]")
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if s := tomledit.Unquote(strings.TrimSpace(p)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// tableValue is a table's key as plain text ("" when unset) — used for the name that labels
// a section.
func tableValue(doc *tomledit.Doc, ti int, key string) string {
	v, _ := doc.Value(ti, key)
	return tomledit.Unquote(v)
}

// configShow prints every scalar with the scope it resolves from.
func configShow(showAll bool) error {
	path, _, doc, err := configDoc()
	if err != nil {
		return err
	}
	root, _ := buildTree(doc, showAll)
	render.Info("config: " + path)
	printTree(root, 0)
	return nil
}

// printTree writes the resolved view: sections stand out, values read plainly, and the
// scope note stays dim — the three things carry different weight, so they can't all be dim.
func printTree(nodes []render.EditorNode, depth int) {
	indent := strings.Repeat("  ", depth)
	for _, n := range nodes {
		if n.Field == nil {
			fmt.Println(indent + hue(n.Label, n.Hue))
			printTree(n.Children, depth+1)
			continue
		}
		val := n.Field.Value
		if strings.TrimSpace(val) == "" {
			val = "--"
		}
		line := fmt.Sprintf("%s%s %s", indent, hue(fmt.Sprintf("%-20s", n.Field.Label), render.HueID), val)
		if n.Origin != "" {
			line += "  " + hue(n.Origin, render.HueDim)
		}
		fmt.Println(line)
	}
}

// configEdit opens the panel, then applies whatever changed as a surgical patch: diff,
// confirm, back up, write.
func configEdit(showAll bool) error {
	path, old, doc, err := configDoc()
	if err != nil {
		return err
	}
	// A cluster's config.toml is a REPLICA: the laptop is the source of truth, and the next
	// `mu setup sync` overwrites this file. Warn, but never refuse — a Windows user has no
	// mu laptop, so the login node is the only place they CAN edit, and for them this file
	// is the truth. `mu setup sync pull` carries an edit made here back the other way.
	if onHPC() {
		render.Warn("this machine's config.toml is a replica — a later `mu setup sync` from your laptop overwrites it (pull it back with `mu setup sync pull`)")
	}

	root, targets := buildTree(doc, showAll)
	changes, saved, err := render.Editor(render.EditorSpec{
		Title:     "config " + path,
		Root:      root,
		Collapsed: true, // a config tree is deep — open on the headings, expand into what you want
	})
	if err != nil {
		return runErr("%s", err)
	}
	if !saved || len(changes) == 0 {
		render.Info("no changes")
		return nil
	}
	applyChanges(doc, targets, changes)
	merged := doc.String()
	if merged == old {
		render.Info("no changes")
		return nil
	}
	showConfigDiff(old, merged)
	fmt.Fprintf(os.Stderr, "write %d change(s) to %s? [y/N] ", len(changes), path)
	var r string
	_, _ = fmt.Scanln(&r)
	if strings.ToLower(strings.TrimSpace(r)) != "y" {
		render.Info("aborted")
		return nil
	}
	if err := writeLocalConfig(path, []byte(old), merged); err != nil {
		return runErr("%s", err)
	}
	render.OK(fmt.Sprintf("wrote %s (backup: %s.bak)", path, path))
	return nil
}

// configDecommission retires a machine non-interactively: decommissionNode edits the doc,
// then the same diff+confirm+write path as the panel.
func configDecommission(node string) error {
	path, old, doc, err := configDoc()
	if err != nil {
		return err
	}
	cname, ok := decommissionNode(doc, node)
	if !ok {
		return runErr("no node %q in any cluster's nodes list", node)
	}
	merged := doc.String()
	if merged == old {
		render.Info("no change")
		return nil
	}
	render.Info(fmt.Sprintf("decommission %s (%s): drop its block, move nodes→decommissioned", node, cname))
	showConfigDiff(old, merged)
	fmt.Fprintf(os.Stderr, "write to %s? [y/N] ", path)
	var r string
	_, _ = fmt.Scanln(&r)
	if strings.ToLower(strings.TrimSpace(r)) != "y" {
		render.Info("aborted")
		return nil
	}
	if err := writeLocalConfig(path, []byte(old), merged); err != nil {
		return runErr("%s", err)
	}
	render.OK(fmt.Sprintf("decommissioned %s — wrote %s (backup: %s.bak)", node, path, path))
	return nil
}

// applyChanges writes the panel's edits into doc. Two phases, because create-on-write must
// not corrupt cached table indices: (1) scalar edits on existing tables — Set never
// renumbers the table list, so their indices stay valid; (2) unconfigured-node edits,
// grouped by node so each machine's [[cluster.node]] block is created once (InsertTable
// re-parses, so the cluster is re-resolved by name) and all its keys set before the next
// node's insert. Unknown paths are skipped.
func applyChanges(doc *tomledit.Doc, targets map[string]target, changes []render.Change) {
	key := func(path []string) string { return strings.Join(path, "\x00") }
	var creates []render.Change
	for _, ch := range changes {
		t, ok := targets[key(ch.Path)]
		if !ok {
			continue
		}
		if t.table < 0 {
			creates = append(creates, ch) // unconfigured node — defer to phase 2
			continue
		}
		doc.Set(t.table, t.key, rawValue(t, ch.New))
	}
	// group deferred creates by node, first-seen order, so each block is built in one go
	byNode := map[string][]render.Change{}
	var order []string
	for _, ch := range creates {
		t := targets[key(ch.Path)]
		nk := t.clusterName + "\x00" + t.node
		if _, seen := byNode[nk]; !seen {
			order = append(order, nk)
		}
		byNode[nk] = append(byNode[nk], ch)
	}
	for _, nk := range order {
		chs := byNode[nk]
		t0 := targets[key(chs[0].Path)]
		ci := doc.Find("cluster", "name", t0.clusterName)
		if ci < 0 {
			continue
		}
		idx := doc.InsertTable(ci, "cluster.node", [][2]string{{"name", tomledit.Quote(t0.node)}})
		for _, ch := range chs {
			t := targets[key(ch.Path)]
			doc.Set(idx, t.key, rawValue(t, ch.New))
		}
	}
}

// rawValue renders a change's new text as the raw TOML value Set expects — trimmed, and
// quoted when the key is a string.
func rawValue(t target, v string) string {
	raw := strings.TrimSpace(v)
	if t.quoted {
		raw = tomledit.Quote(raw)
	}
	return raw
}

// hue colors text unless the terminal (or the user) asked for plain — render.Bold always
// emits ANSI, so the gate is the caller's per house convention.
func hue(text, h string) string {
	if render.Plain() {
		return text
	}
	return render.Bold(text, h)
}
