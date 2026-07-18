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

// cfgKey is one editable scalar of the config schema: how it's entered, how it's checked,
// and whether TOML wants it quoted. Maps (submit_queue, queue_class) and arrays (fleet) are
// deliberately absent — v1 edits values, not structure, and an inline table reads better in
// the file than through a panel.
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
	rootKeys = []cfgKey{strKey("hpc_user", "HPC login name")}

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
	}

	// A node inherits every one of these from its cluster, so each may be left blank.
	nodeKeys = []cfgKey{
		enumKey("scheduler", []string{"", "pbs", "slurm"}),
		strKey("account", "this machine's allocation"),
		wallKey("interactive_walltime", "this machine's held session"),
		enumKey("queue_flag", []string{"", "partition", "qos"}),
		intKey("cores_per_node", "cores on THIS machine"),
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
}

// configCmd is `mu config`: show the resolved config, or edit it in the panel. The two-scope
// model (cluster default, node override) makes the resolved value genuinely hard to read off
// the file, which is why `show` annotates every value with where it came from.
func configCmd() *cobra.Command {
	var interactive bool
	c := &cobra.Command{
		Use:   "config",
		Short: "Show or edit config.toml (values, with their scope).",
		Long: "Print config.toml's values annotated with the scope each resolves from — a node's\n" +
			"own block, its cluster's default, or a built-in — or edit them in place with -i.\n\n" +
			"Edits are surgical: mu rewrites only the lines you changed, so comments, ordering\n" +
			"and alignment survive. Declaring a NEW cluster or machine stays a hand-edit; so do\n" +
			"the inline maps (submit_queue, queue_class) and the fleet list.\n\n" +
			"`account` becomes a picker once `mu hpc usage` has listed your subprojects — it\n" +
			"caches their names, so the panel itself never needs a ticket.\n\n" +
			"    mu config          # the resolved view\n" +
			"    mu config -i       # the panel",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if interactive {
				return configEdit()
			}
			return configShow()
		},
	}
	c.Flags().BoolVarP(&interactive, "interactive", "i", false, "edit in the panel")
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
func buildTree(doc *tomledit.Doc) ([]render.EditorNode, map[string]target) {
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

	// Root scalars (hpc_user) live at the TOML top level, not under a [table], but a lone
	// top-level key reads as orphaned beside the section headers — so the panel gathers them
	// under a synthetic "[general]" heading. Display only: each still writes to table 0. The
	// section's Key keeps its decorated label out of the leaf paths (see the leaf-path fix).
	var general []render.EditorNode
	for _, k := range rootKeys {
		v, origin := value(0, k)
		general = append(general, leaf([]string{"general", k.name}, target{0, k.name, k.quoted}, k, v, origin))
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
			kids = append(kids, leaf([]string{name, k.name}, target{ts[0], k.name, k.quoted}, k, v, origin))
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
			kids = append(kids, leaf([]string{cname, k.name}, target{ci, k.name, k.quoted}, k, v, origin))
		}
		// The cluster's machines: a node block is owned by the cluster above it.
		for _, ni := range doc.Tables("cluster.node") {
			if doc.Owner(ni) != ci {
				continue
			}
			nname := tableValue(doc, ni, "name")
			var nkids []render.EditorNode
			for _, k := range nodeKeys {
				k = acctKey(k, accts, true)
				v, ok := doc.Value(ni, k.name)
				origin := ""
				switch {
				case ok:
					v = tomledit.Unquote(v)
				default:
					// Fall back exactly as config.siteFor does, and SAY so — the whole
					// point of the panel is making that resolution visible.
					if cv, cok := doc.Value(ci, k.name); cok {
						// ↳ marks a value INHERITED from the wider scope — the one thing the
						// file itself cannot show you. Only inherited values get it; "unset"
						// came from nowhere.
						v, origin = tomledit.Unquote(cv), render.Glyph("↳ ", "< ")+"from "+cname
					} else {
						v, origin = "", "unset"
					}
				}
				nkids = append(nkids, leaf([]string{cname, nname, k.name}, target{ni, k.name, k.quoted}, k, v, origin))
			}
			kids = append(kids, render.EditorNode{Label: nname, Hue: render.HueLoc, Children: nkids})
		}
		// Nodes listed in `nodes` but with no [[cluster.node]] block: surface them as
		// unconfigured so the gap is visible. They resolve entirely to cluster defaults
		// until given a block; -i creates one (v2a).
		for _, nm := range unconfiguredNodes(doc, ci) {
			kids = append(kids, render.EditorNode{
				Label: nm + render.Glyph("  · ", "  * ") + "unconfigured (inherits " + cname + ")",
				Key:   nm,
				Hue:   render.HueDim,
			})
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
func configShow() error {
	path, _, doc, err := configDoc()
	if err != nil {
		return err
	}
	root, _ := buildTree(doc)
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
func configEdit() error {
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

	root, targets := buildTree(doc)
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
	for _, ch := range changes {
		t, ok := targets[strings.Join(ch.Path, "\x00")]
		if !ok {
			continue
		}
		raw := strings.TrimSpace(ch.New)
		if t.quoted {
			raw = tomledit.Quote(raw)
		}
		doc.Set(t.table, t.key, raw)
	}
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

// hue colors text unless the terminal (or the user) asked for plain — render.Bold always
// emits ANSI, so the gate is the caller's per house convention.
func hue(text, h string) string {
	if render.Plain() {
		return text
	}
	return render.Bold(text, h)
}
