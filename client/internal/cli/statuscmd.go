// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/endpoint"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmops"
)

// `stats` and `status` are the cli twins of the MCP tools `stats` and
// `cluster_overview` (review P-03): the assistant could see per-container load
// and a one-screen health summary that the operator had no command for. Both
// run the same swarmops code as the tools, so the two cannot drift.

// clusterConn is a command's connection to one cluster: the manager, the
// resolver over it, and (when the config allows) the agents.
type clusterConn struct {
	ctx   context.Context
	label string
	dcli  *client.Client
	r     *resolve.Resolver
	cfg   config.Config
	// cfgErr is an agent configuration that does not validate: manager-only
	// reads still work, agent reads say why they cannot.
	cfgErr error
}

// connectCluster resolves the endpoint and config and connects the manager.
func connectCluster(cmd *cobra.Command, g *globalFlags) (*clusterConn, error) {
	ep := endpoint.Resolve(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, ep)
	if err != nil {
		return nil, &cliError{code: exitUsage, err: err}
	}
	ctx := cmdContext(cmd)
	dcli, err := ep.Connect(ctx)
	if err != nil {
		return nil, &cliError{code: exitTransport, err: err}
	}
	return &clusterConn{ctx: ctx, label: dockerctx.ResolveName(g.dockerContext), dcli: dcli,
		r: resolve.New(dcli, addrModeOf(cfg)), cfg: cfg, cfgErr: cfg.Validate()}, nil
}

// agents is the cluster's agent client; the caller closes it.
func (c *clusterConn) agents() (*agentcall.Client, error) {
	if c.cfgErr != nil {
		return nil, &cliError{code: exitUsage, err: c.cfgErr}
	}
	return agentcall.New(c.cfg), nil
}

// --- stats -------------------------------------------------------------------

type statsFlags struct {
	json           bool
	connectTimeout time.Duration
	wait           time.Duration
}

func newStatsCmd(g *globalFlags) *cobra.Command {
	f := &statsFlags{}
	cmd := &cobra.Command{
		Use:   "stats [service...]",
		Short: "Show live CPU and memory of each container, measured by the node agents",
		Long: "One reading of every running container of the named services (all services\n" +
			"when none is named): CPU, memory against its limit, and health. The agents\n" +
			"measure on the nodes; the manager API only knows what was reserved.\n\n" +
			"An agent samples only while someone asks, so a first reading can take a few\n" +
			"seconds — stats waits up to --wait for CPU (a delta of two samples) and then\n" +
			"prints what it has: \"warming up\" for a container still without a reading,\n" +
			"\"unavailable\" where the node's agent did not answer. Same data as the MCP\n" +
			"tool stats. When no agent answered at all, stats exits 125 and prints no\n" +
			"table: an empty reading is not a measurement.",
		Example: "  swarmexec stats\n" +
			"  swarmexec stats web api\n" +
			"  swarmexec stats --json web",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStats(cmd, g, f, args)
		},
	}
	cmd.Flags().BoolVar(&f.json, "json", false, "output JSON instead of a table")
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "per-node connect timeout")
	cmd.Flags().DurationVar(&f.wait, "wait", swarmops.StatsWaitDefault, "how long to wait for agents still warming up (0: ask once)")
	return cmd
}

func runStats(cmd *cobra.Command, g *globalFlags, f *statsFlags, args []string) error {
	c, err := connectCluster(cmd, g)
	if err != nil {
		return err
	}
	defer c.dcli.Close()
	if err := checkServicesExist(c.ctx, c.dcli, args); err != nil {
		return err
	}
	ac, err := c.agents()
	if err != nil {
		return err
	}
	defer ac.Close()
	wanted := map[string]bool{}
	for _, a := range args {
		wanted[a] = true
	}
	rows, err := swarmops.GatherStats(c.ctx, c.r, ac, func(svc string) bool { return len(wanted) == 0 || wanted[svc] },
		swarmops.StatsPoll{ConnectTimeout: f.connectTimeout, Wait: f.wait, Interval: swarmops.StatsPollDefault})
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}
	if swarmops.NoAgentAnswered(rows) {
		// Every row says "unavailable": an empty reading is not a measurement.
		return &cliError{code: exitTransport, err: errors.New("no node agent answered the stats request: the usage is unknown (swarmexec doctor shows why)")}
	}
	if f.json {
		return printJSON(cmd.OutOrStdout(), rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no running containers")
		return nil
	}
	return printStats(cmd.OutOrStdout(), cmd.ErrOrStderr(), rows)
}

// checkServicesExist refuses a service name the manager does not know — a
// typo would otherwise read as "no running containers".
func checkServicesExist(ctx context.Context, dcli *client.Client, names []string) error {
	if len(names) == 0 {
		return nil
	}
	res, err := dcli.ServiceList(ctx, client.ServiceListOptions{})
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}
	known := map[string]bool{}
	for _, s := range res.Items {
		known[s.Spec.Name] = true
	}
	for _, n := range names {
		if !known[n] {
			return &cliError{code: exitTransport, err: fmt.Errorf("no service %q", n)}
		}
	}
	return nil
}

// printStats is the table, then — once each, on stderr — what "warming up"
// and "unavailable" mean, so the rows stay one line each.
func printStats(out, errOut io.Writer, rows []swarmops.StatsRow) error {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tSLOT\tNODE\tCONTAINER\tCPU\tMEMORY\tHEALTH")
	notes := map[string]bool{}
	var order []string
	for _, r := range rows {
		slot := "-"
		if r.Slot > 0 {
			slot = fmt.Sprint(r.Slot)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Service, slot, r.Node, r.Container, r.CPU, r.Memory, orDash(r.Health))
		if r.Note != "" && !notes[r.Note] {
			notes[r.Note] = true
			order = append(order, r.Note)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, n := range order {
		fmt.Fprintln(errOut, "note: "+n)
	}
	return nil
}

// --- status ------------------------------------------------------------------

type statusFlags struct {
	json           bool
	connectTimeout time.Duration
}

func newStatusCmd(g *globalFlags) *cobra.Command {
	f := &statusFlags{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the cluster at a glance: nodes, services, replicas and health",
		Long: "One screen of the whole cluster: every node with its role, state and\n" +
			"availability, every service with its replicas and the health of its\n" +
			"containers, and a summary line counting the degraded ones (running short,\n" +
			"or with an unhealthy container). Health comes from the node agents and is\n" +
			"best effort: without them the services are listed without it, and a note\n" +
			"says so. Same data as the MCP tool cluster_overview.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, g, f)
		},
	}
	cmd.Flags().BoolVar(&f.json, "json", false, "output JSON instead of tables")
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "per-node connect timeout")
	return cmd
}

func runStatus(cmd *cobra.Command, g *globalFlags, f *statusFlags) error {
	c, err := connectCluster(cmd, g)
	if err != nil {
		return err
	}
	defer c.dcli.Close()
	usage := map[string]agentcall.ContainerUsage{}
	if ac, aerr := c.agents(); aerr == nil {
		usage = swarmops.ClusterUsage(c.ctx, c.r, ac, f.connectTimeout)
		ac.Close()
	}
	ov, err := swarmops.GatherOverview(c.ctx, c.dcli, c.r, c.label, usage, nil)
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}
	if f.json {
		return printJSON(cmd.OutOrStdout(), ov)
	}
	return printStatus(cmd.OutOrStdout(), cmd.ErrOrStderr(), ov)
}

func printStatus(out, errOut io.Writer, ov swarmops.Overview) error {
	fmt.Fprintf(out, "context %s · manager %s · engine %s\n\n", ov.Context, orDash(ov.Manager), orDash(ov.Engine))
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tROLE\tSTATE\tAVAIL\tENGINE")
	for _, n := range ov.Nodes {
		name := n.Hostname
		if n.Leader {
			name += " (leader)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, orDash(n.Role), orDash(n.State), orDash(n.Availability), orDash(n.Engine))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)
	if len(ov.Services) == 0 {
		fmt.Fprintln(errOut, "no services")
	} else {
		w = tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "SERVICE\tSTACK\tMODE\tREPLICAS\tHEALTH\tUPDATE")
		for _, s := range ov.Services {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, orDash(s.Stack), orDash(s.Mode), s.Replicas,
				healthTallyText(s.Health), orDash(s.UpdateState))
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, ov.Summary)
	if ov.Note != "" {
		fmt.Fprintln(errOut, "note: "+ov.Note)
	}
	return nil
}

// healthTallyText renders a service's health counts, only the non-zero ones:
// "2 healthy, 1 unhealthy". "-" when the agents gave no health at all.
func healthTallyText(h *swarmops.HealthTally) string {
	if h == nil {
		return "-"
	}
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{h.Healthy, "healthy"}, {h.Unhealthy, "unhealthy"}, {h.Starting, "starting"}, {h.NoCheck, "no check"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}
