package cli

// Site-command collate: the -f/--fleet and -a/--all fan-out shared by the show_* wrapper
// verbs (storage, usage). collateJobs stays separate — its fetch is scheduler-shaped
// (per-target command + user scope), not a fixed site command.

import (
	"errors"
	"fmt"

	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/render"
)

// fetchSite runs a site command on node over remote-exec and returns its raw output.
// A broken/absent site command degrades to a warning and empty output, never a crash;
// a dead ticket aborts. Shared by the show_* wrapper verbs.
func fetchSite(node, siteCmd string) (string, string, error) {
	target, err := hpc.Resolve(node)
	if err != nil {
		return "", "", usageErr("%s", err)
	}
	if err := hpc.EnsureTicket(); err != nil {
		return "", "", runErr("%s", err)
	}
	out, err := hpc.RemoteExec(target, siteCmd)
	if err != nil {
		render.Warn(fmt.Sprintf("%s: %s failed (broken or unsupported on this system?): %v", node, siteCmd, err))
		return node, "", nil
	}
	return node, out, nil
}

// fetchSiteLocal runs a site command on the current cluster (no ssh) — the on-HPC path.
func fetchSiteLocal(siteCmd string) (string, string, error) {
	self, _ := currentCluster()
	if self == "" {
		return "", "", usageErr("not on an HPC cluster — use --node <n> or pipe the %s output", siteCmd)
	}
	out, err := hpc.LocalExec(siteCmd)
	if err != nil {
		render.Warn(fmt.Sprintf("%s: local %s failed (broken or unsupported here?): %v", self, siteCmd, err))
		return self, "", nil
	}
	return self, out, nil
}

// collateSite fans out siteCmd over the given targets concurrently (bounded per fetch,
// like collateJobs), parses each output, and tags every row with the target label — the
// config cluster name, the user's vocabulary, over whatever name the site tool prints.
// Down targets degrade to "label: reason" notes, never a hang or a total failure. The
// Kerberos ticket is ensured once up front. scope is "fleet" or "all", driving the label
// and the empty-set message.
func collateSite[T any](targets []queueTarget, scope, siteCmd string, parse func(string) []T, tag func(*T, string)) (string, []T, []string, error) {
	if len(targets) == 0 {
		if scope == "fleet" {
			return "", nil, nil, usageErr("nothing in the fleet — set a `fleet = [...]` node list or `active = true` on a cluster, or use --all-systems")
		}
		return "", nil, nil, usageErr("no clusters configured — add clusters to config.toml")
	}
	if err := hpc.EnsureTicket(); err != nil {
		return "", nil, nil, runErr("%s", err)
	}
	type result struct {
		label string
		rows  []T
		err   error
	}
	results := make([]result, len(targets))
	sp := render.NewSpinner(fmt.Sprintf("Collating %s 0/%d", siteCmd, len(targets)))
	sp.Start()
	done := make(chan struct{}, len(targets))
	for i := range targets {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			t := targets[i]
			results[i] = result{label: t.label}
			if t.node == "" {
				results[i].err = errors.New("no nodes configured")
				return
			}
			target, err := hpc.Resolve(t.node)
			if err != nil {
				results[i].err = err
				return
			}
			out, err := hpc.RemoteExecTimeout(target, siteCmd, collateTimeout)
			if err != nil {
				results[i].err = err
				return
			}
			rows := parse(out)
			for j := range rows {
				tag(&rows[j], t.label)
			}
			results[i].rows = rows
		}(i)
	}
	for n := 1; n <= len(targets); n++ {
		<-done
		sp.SetMessage(fmt.Sprintf("Collating %s %d/%d", siteCmd, n, len(targets)))
	}
	sp.Stop()
	label := scope
	if scope == "all" {
		label = "all systems"
	}
	var all []T
	var down []string
	for _, r := range results {
		if r.err != nil {
			down = append(down, fmt.Sprintf("%s: %v", r.label, r.err))
			continue
		}
		all = append(all, r.rows...)
	}
	return label, all, down, nil
}
