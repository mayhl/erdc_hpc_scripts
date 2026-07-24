package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/queue"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// collateTimeout bounds each cluster's fetch during --all fan-out — long enough for
// ssh + Kerberos + login-profile + squeue, short enough that a wedged cluster
// doesn't stall the whole collate.
const collateTimeout = 30 * time.Second

// clusterResult is one cluster's collate outcome: its jobs (plus any model-hook
// progress, keyed by short id), or the error that dropped it (unreachable,
// timed out, misconfigured).
type clusterResult struct {
	cluster string
	jobs    []queue.Job
	prog    map[string]string
	err     error
}

// queueTarget is one collate fetch: which node to run the scheduler on and the label
// each returned job is tagged with. label is the cluster name (cluster scopes) or the
// node/system name (an explicit `fleet` list), so a DSRC split across separate schedulers
// stays distinguishable in the merged table.
type queueTarget struct {
	label     string
	scheduler string
	node      string // node name to resolve + fetch from ("" → no node configured)
}

// clusterTargets picks one representative node (Nodes[0]) per cluster — the scope used by
// --all and by --fleet's fallback when no explicit `fleet` list is configured. Clusters
// with no node/scheduler are kept so fetchTarget reports them as a warning, not a silent drop.
func clusterTargets(cs []config.Cluster) []queueTarget {
	t := make([]queueTarget, 0, len(cs))
	for _, c := range cs {
		node := ""
		if len(c.Nodes) > 0 {
			node = c.Nodes[0]
		}
		t = append(t, queueTarget{label: c.Name, scheduler: c.Scheduler, node: node})
	}
	return t
}

// fleetTargets builds one target per node in the explicit `fleet` list, each labeled by
// its node/system name and carrying that node's cluster-declared scheduler.
func fleetTargets(nodes []string) []queueTarget {
	t := make([]queueTarget, 0, len(nodes))
	for _, n := range nodes {
		t = append(t, queueTarget{label: n, scheduler: config.SchedulerFor(n), node: n})
	}
	return t
}

// fleetScope resolves the --fleet target set: the explicit `fleet` node list when
// configured (one fetch per listed system — so a multi-scheduler DSRC like navy isn't
// collapsed to a single representative node), else a soft fallback to one node per active
// cluster (never worse than the prior behavior).
func fleetScope() []queueTarget {
	if nodes := config.Fleet(); len(nodes) > 0 {
		return fleetTargets(nodes)
	}
	return clusterTargets(config.ActiveClusters())
}

// allSystemsScope resolves --all-systems: every distinct queue = the fleet list plus one
// representative node for each configured cluster (incl. inactive) whose nodes are NOT
// already covered by the fleet. A proper superset of --fleet that reaches unlisted/inactive
// clusters without re-querying a cluster's shared scheduler. With no `fleet` list it reduces
// to one node per cluster (the prior --all behavior).
func allSystemsScope() []queueTarget {
	fleet := config.Fleet()
	inFleet := make(map[string]bool, len(fleet))
	for _, n := range fleet {
		inFleet[n] = true
	}
	targets := fleetTargets(fleet)
	for _, c := range config.ClusterDefs() {
		covered := false
		for _, n := range c.Nodes {
			if inFleet[n] {
				covered = true
				break
			}
		}
		if covered {
			continue // a fleet node already fetches this cluster's queue
		}
		node := ""
		if len(c.Nodes) > 0 {
			node = c.Nodes[0]
		}
		targets = append(targets, queueTarget{label: c.Name, scheduler: c.Scheduler, node: node})
	}
	return targets
}

// -f/--fleet's NoOptDefVal is the shared fleetAuto sentinel (declared in projectsync.go):
// a bare -f (no value) carries it, meaning "the configured fleet"; an explicit value
// (-f navy / -f n1,n2) overrides it with a named fleet or an ad-hoc node list.

// scopeTargets picks the collate fan-out for a -f/-e pair. -e/--all-systems wins (widest);
// otherwise the -f value selects the fleet scope: the sentinel/empty = the configured fleet,
// a value = a named fleet or an ad-hoc comma-list of nodes. Errors if a listed node is unknown.
func scopeTargets(fleetArg string, all bool) ([]queueTarget, string, error) {
	if all {
		return allSystemsScope(), "all", nil
	}
	targets, err := fleetArgScope(fleetArg)
	return targets, "fleet", err
}

// fleetArgScope resolves the -f value into collate targets: the sentinel/empty (bare -f)
// falls to the configured `fleet` list; a value is looked up as a named fleet FIRST (so a
// name that is both a fleet and a node resolves to the fleet), else split as an ad-hoc
// comma-list of node names.
func fleetArgScope(arg string) ([]queueTarget, error) {
	if arg == "" || arg == fleetAuto {
		return fleetScope(), nil
	}
	if nodes := namedFleetNodes(arg); len(nodes) > 0 {
		return fleetTargets(nodes), nil
	}
	nodes, err := parseFleetNodes(arg)
	if err != nil {
		return nil, err
	}
	return fleetTargets(nodes), nil
}

// namedFleetNodes resolves a config-declared named fleet to its node list. Named fleets
// are not a config schema yet, so this is the resolution seam — it returns nil today, which
// makes `-f <name>` fall through to the ad-hoc node-list path. FUTURE: back it with a
// `[fleet.<name>]` block so `-f navy` picks up a saved subset with zero change here.
func namedFleetNodes(_ string) []string { return nil }

// parseFleetNodes splits an ad-hoc -f node list ("a,b,c") and validates each token is a
// configured node, so a typo fails loud up front instead of surfacing as a per-target "no
// scheduler configured" warning after the fan-out.
func parseFleetNodes(arg string) ([]string, error) {
	known := make(map[string]bool)
	for _, n := range config.NodeNames() {
		known[n] = true
	}
	var nodes []string
	for _, tok := range strings.Split(arg, ",") {
		if tok = strings.TrimSpace(tok); tok == "" {
			continue
		}
		if !known[tok] {
			return nil, usageErr("unknown node %q in --fleet — not a configured node (see `mu hpc nodes`)", tok)
		}
		nodes = append(nodes, tok)
	}
	if len(nodes) == 0 {
		return nil, usageErr("--fleet needs a node list, e.g. -f node1,node2")
	}
	return nodes, nil
}

// siteScopeHelp carries a verb's own wording for the four WHERE flags — the
// registration is shared, the help text is not.
type siteScopeHelp struct {
	node, local, fleet, all string
}

// addSiteScopeFlags registers the WHERE flags the site show-verbs share
// (-N/--node, -l/--local, -f/--fleet, -e/--all-systems), their mutual
// exclusion, and --node completion. The queue-verb analog is addQueueScopeFlags.
// -f is a value flag (bare -f = the configured fleet via the fleetAuto sentinel; an
// explicit -f navy / -f n1,n2 narrows to a named fleet or an ad-hoc node list); it stays
// distinguishable from "not given" by the empty string.
func addSiteScopeFlags(c *cobra.Command, node *string, local *bool, fleet *string, all *bool, help siteScopeHelp) {
	c.Flags().StringVarP(node, "node", "N", "", help.node)
	c.Flags().BoolVarP(local, "local", "l", false, help.local)
	c.Flags().StringVarP(fleet, "fleet", "f", "", help.fleet)
	c.Flags().Lookup("fleet").NoOptDefVal = fleetAuto
	c.Flags().BoolVarP(all, "all-systems", "e", false, help.all)
	c.MarkFlagsMutuallyExclusive("node", "local", "fleet", "all-systems")
	completeNodeFlag(c)
}

// collateJobs fans out over the given targets concurrently, each fetched with a bounded
// timeout, and returns the display label plus the merged jobs (tagged by label), their
// model-hook progress ("label/id" keys), and "label: reason" notes for any that failed —
// so a down target degrades to a warning, never a hang or a total failure. The Kerberos
// ticket is ensured once up front. scope is "fleet" or "all", driving both the label and
// the empty-set message.
func collateJobs(targets []queueTarget, scope string, who userSel) (string, []queue.Job, map[string]string, []string, error) {
	if len(targets) == 0 {
		if scope == "fleet" {
			return "", nil, nil, nil, usageErr("nothing in the fleet — set a `fleet = [...]` node list or `active = true` on a cluster, or use --all-systems")
		}
		return "", nil, nil, nil, usageErr("no clusters configured — add clusters to config.toml")
	}
	if err := hpc.EnsureTicket(); err != nil {
		return "", nil, nil, nil, runErr("%s", err)
	}
	// Fan out concurrently; a spinner tracks how many of the N cluster fetches have
	// returned (order is nondeterministic — a down/slow one just ticks the count
	// when its bounded remote-exec times out, then surfaces as a warning later).
	sp := render.NewSpinner(fmt.Sprintf("Collating queues 0/%d", len(targets)))
	sp.Start()
	results := collateFan(targets, who, true, func(n int) {
		sp.SetMessage(fmt.Sprintf("Collating queues %d/%d", n, len(targets)))
	})
	sp.Stop()
	label := scope
	if scope == "all" {
		label = "all systems"
	}
	jobs, prog, down := mergeResults(results)
	return label, jobs, prog, down, nil
}

// collateFan is the bare concurrent fan-out over targets — no spinner, no ticket
// preamble — so the collate table (spinner around it) and the cross-cluster picker
// (re-fetching silently behind the TUI, where a spinner would corrupt the frame)
// share one fetch. onDone (optional) fires with the running completion count.
// hooks=false also skips the per-target model-hooks ssh — the picker doesn't show
// progress, so its refresh shouldn't double every cluster's traffic.
func collateFan(targets []queueTarget, who userSel, hooks bool, onDone func(int)) []clusterResult {
	results := make([]clusterResult, len(targets))
	done := make(chan struct{}, len(targets))
	for i := range targets {
		go func(i int) {
			results[i] = fetchTarget(targets[i], who, hooks)
			done <- struct{}{}
		}(i)
	}
	for n := 1; n <= len(targets); n++ {
		<-done
		if onDone != nil {
			onDone(n)
		}
	}
	return results
}

// fetchTarget runs one target's scheduler over the bounded remote-exec, tagging each job
// with the target's label. The model-hooks fetch launches first so it runs concurrent
// with the snapshot on the same system; its failures lose the progress, never the target.
func fetchTarget(t queueTarget, who userSel, hooks bool) clusterResult {
	if t.node == "" {
		return clusterResult{cluster: t.label, err: errors.New("no nodes configured")}
	}
	cmd, parse := fetchSpec(t.scheduler, who)
	if cmd == "" {
		return clusterResult{cluster: t.label, err: errors.New("no scheduler configured")}
	}
	target, err := hpc.Resolve(t.node)
	if err != nil {
		return clusterResult{cluster: t.label, err: err}
	}
	var hooksCh <-chan map[string]string
	if hooks {
		hooksCh = fetchHookProgress(t.node, false)
	}
	out, err := hpc.RemoteExecTimeout(target, cmd, collateTimeout)
	if err != nil {
		return clusterResult{cluster: t.label, err: err}
	}
	jobs := parse(out)
	for i := range jobs {
		jobs[i].Cluster = t.label
	}
	return clusterResult{cluster: t.label, jobs: jobs, prog: awaitHookProgress(hooksCh)}
}

// mergeResults flattens per-cluster results into one job list (in cluster order) plus
// the "label/id"-keyed hook progress (short ids can collide across systems) and
// "cluster: reason" notes for the failures. Pure — the fan-out's testable core.
func mergeResults(results []clusterResult) ([]queue.Job, map[string]string, []string) {
	var jobs []queue.Job
	var down []string
	prog := map[string]string{}
	for _, r := range results {
		if r.err != nil {
			down = append(down, fmt.Sprintf("%s: %v", r.cluster, r.err))
			continue
		}
		jobs = append(jobs, r.jobs...)
		for id, p := range r.prog {
			prog[r.cluster+"/"+id] = p
		}
	}
	return jobs, prog, down
}
