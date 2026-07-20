package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mayhl/mayhl_utils/internal/config"
	"github.com/mayhl/mayhl_utils/internal/hpc"
	"github.com/mayhl/mayhl_utils/internal/render"
)

const probeTimeout = 2 * time.Second

func hpcCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hpc",
		Short: "Cross-cluster HPC info (nodes, reachability, ticket).",
		Long: "Aggregate info across the configured clusters. Local-primary — run it from your\n" +
			"workstation to reach every cluster; on a login node you'll only see what's\n" +
			"reachable from there.",
	}
	c.AddCommand(hpcNodesCmd(), hpcQueueCmd(), hpcQueuesCmd(), hpcStorageCmd(), hpcUsageCmd(), hpcTicketCmd())
	return c
}

// completeNodeFlag wires cluster-name completion onto a command's --node flag —
// the one registration every -N/--node verb shares.
func completeNodeFlag(c *cobra.Command) {
	_ = c.RegisterFlagCompletionFunc("node", func(_ *cobra.Command, _ []string, tc string) ([]string, cobra.ShellCompDirective) {
		return hpc.CompleteNode(tc), cobra.ShellCompDirectiveNoFileComp
	})
}

// currentCluster resolves the cluster this shell runs on to its (name, scheduler)
// from config, or ("", "") off-HPC. $MU_NODE overrides $BC_HOST; when $BC_HOST
// carries a login-node number (e.g. login01) absent from config, it retries the
// digit-stripped base (login). A non-empty name with an empty scheduler means
// on-HPC-but-unconfigured — the caller reports that.
func currentCluster() (string, string) {
	self := os.Getenv("MU_NODE")
	if self == "" {
		self = os.Getenv("BC_HOST")
	}
	if self == "" {
		return "", ""
	}
	if s := config.SchedulerFor(self); s != "" {
		return self, s
	}
	if base := strings.TrimRight(self, "0123456789"); base != self {
		if s := config.SchedulerFor(base); s != "" {
			return base, s
		}
	}
	return self, ""
}

func hpcTicketCmd() *cobra.Command {
	var renew bool
	c := &cobra.Command{
		Use:   "ticket",
		Short: "Show local Kerberos ticket status; --renew runs pkinit (local only).",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if renew {
				renewTicket()
			}
			showTicket()
			return nil
		},
	}
	c.Flags().BoolVar(&renew, "renew", false, "obtain/refresh the ticket via pkinit (local only)")
	return c
}

func showTicket() {
	info, available := hpc.Ticket()
	if !available {
		render.Warn("klist not found — no local Kerberos here")
		return
	}
	if !info.Present {
		if onHPC() {
			// normal on a login node: the credential arrived by delegation with
			// the login; there's no local cache and no pkinit here
			render.Info("no local ticket cache — expected on an HPC node (auth is delegated at login)")
			return
		}
		render.Warn("no Kerberos ticket — run `mu hpc ticket --renew`")
		return
	}
	who := info.Principal
	if who == "" {
		who = "(unknown principal)"
	}
	if !info.Expires.IsZero() {
		rem := time.Until(info.Expires)
		if rem <= 0 {
			render.Warn(fmt.Sprintf("ticket EXPIRED for %s — run `mu hpc ticket --renew`", who))
			return
		}
		render.OK(fmt.Sprintf("ticket: %s   expires %s (in %s)", who, info.Expires.Format("Jan 2 15:04"), humanDur(rem)))
		return
	}
	render.OK("ticket: " + who)
}

func renewTicket() {
	if os.Getenv("BC_HOST") != "" || os.Getenv("MU_SYSTEM") == "hpc" {
		render.Warn("--renew is local-only; on HPC the ticket is inherited from login")
		return
	}
	user := config.HPCUser()
	if user == "" {
		render.Err("no HPC username configured (hpc_user / MU_HPC_UNAME)")
		return
	}
	render.Info("running pkinit for " + user + "…")
	cmd := exec.Command("pkinit", user)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = cmd.Run()
}

// humanDur formats a ticket's remaining life compactly (2d 3h / 5h 47m / 47m).
func humanDur(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h >= 24:
		return fmt.Sprintf("%dd %dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

func hpcNodesCmd() *cobra.Command {
	var status bool
	c := &cobra.Command{
		Use:   "nodes",
		Short: "List configured nodes; -s probes ssh reachability from here.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			defs := config.ClusterDefs()
			if len(defs) == 0 {
				// warn (yellow), but still exit non-zero without a second red line
				render.Warn("no nodes — is the cluster config set?")
				return codeErr(1)
			}
			var st map[string]string
			if status {
				hosts := make(map[string]string)
				for _, cl := range defs {
					for _, n := range cl.Nodes {
						hosts[n] = n + "." + cl.Domain
					}
				}
				st = hpc.Probe(hosts, probeTimeout)
			}
			render.NodesTable(toNodeGroups(defs), config.User(), st)
			return nil
		},
	}
	c.Flags().BoolVarP(&status, "status", "s", false, "probe ssh (port 22) reachability from here — ● up / ○ down")
	return c
}

// toNodeGroups maps config clusters to render's plain NodeGroup view (keeping render
// domain-free, like toJobRows/toQueueRows). Host = node.domain.
func toNodeGroups(defs []config.Cluster) []render.NodeGroup {
	groups := make([]render.NodeGroup, len(defs))
	for i, cl := range defs {
		rows := make([]render.NodeRow, len(cl.Nodes))
		for j, n := range cl.Nodes {
			rows[j] = render.NodeRow{Name: n, Host: n + "." + cl.Domain}
		}
		groups[i] = render.NodeGroup{Cluster: cl.Name, Nodes: rows}
	}
	return groups
}
