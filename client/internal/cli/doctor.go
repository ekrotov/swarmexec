// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/agentdeploy"
	"swarmexec/client/internal/clockskew"
	"swarmexec/client/internal/clusterread"
	"swarmexec/client/internal/endpoint"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmops"
	"swarmexec/client/internal/updatecheck"
	"swarmexec/internal/dockerfmt"
)

type doctorFlags struct {
	connectTimeout time.Duration
	json           bool
}

func newDoctorCmd(g *globalFlags) *cobra.Command {
	f := &doctorFlags{}
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the swarm: manager, agent deployment, and per-node agent health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd, g, f)
		},
	}
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "per-node connect timeout")
	cmd.Flags().BoolVar(&f.json, "json", false, "output JSON instead of a table")
	return cmd
}

type doctorReport struct {
	Client string `json:"client"`
	Proto  string `json:"proto"`
	// LatestRelease is a release newer than Client, when the daily check
	// (ui.check_updates) knows one. Information, not a failure.
	LatestRelease string `json:"latest_release,omitempty"`
	Manager       string `json:"manager"`
	AgentDeployed bool   `json:"agent_deployed"`
	Image         string `json:"image,omitempty"`
	Healthy       bool   `json:"healthy"`
	// SpecDrift is how the deployed agent service differs from the one init
	// writes. Empty is the normal answer; anything here means the cluster is
	// running a spec from an older swarmexec, or one edited by hand.
	SpecDrift []agentdeploy.Finding `json:"spec_drift,omitempty"`
	Nodes     []doctorNode          `json:"nodes,omitempty"`
	// ConfigError is why the agents were not probed: the agent configuration
	// is incomplete (no secret, no certificates). The rest of the report,
	// manager and whether the agent is deployed, is still the answer.
	ConfigError string `json:"config_error,omitempty"`
}

type doctorNode struct {
	Node    string `json:"node"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Proto   string `json:"proto,omitempty"`
	// Clock is this node's clock against ours, signed and rounded ("+3.1s"),
	// or empty when the agent is too old to report its time. ClockOff is set
	// only when the drift is past clockskew.Warn, so a consumer does not have
	// to re-apply the threshold.
	Clock    string `json:"clock,omitempty"`
	ClockOff bool   `json:"clock_off,omitempty"`
	// Error is why the node is not ok, as the agent error explanation reads:
	// a rejected secret, a TLS failure and a refused connection all share the
	// status "unreachable" and differ only here.
	Error string `json:"error,omitempty"`
}

func runDoctor(cmd *cobra.Command, g *globalFlags, f *doctorFlags) error {
	dockerEP := endpoint.Resolve(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, dockerEP)
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}
	// An incomplete agent config does not stop doctor: it is the command for
	// exactly that situation, and "agent: NOT DEPLOYED" is the answer on a
	// machine that has not run init yet.
	cfgErr := cfg.Validate()
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()
	// Asked alongside the cluster probes; at most a 3 s wait, usually a
	// cache read.
	latest := g.startNewerRelease(ctx, cfg)

	dcli, err := dockerEP.Connect(ctx)
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}

	infoRes, err := dcli.Info(ctx, client.InfoOptions{})
	info := infoRes.Info
	if err != nil {
		printManagerUnreachable(out, g.version, latest(), f.json, err)
		return &cliError{code: exitTransport, silent: true}
	}

	agentDep := swarmops.AgentDeployed(ctx, dcli)
	var image string
	var drift []agentdeploy.Finding
	if svc := swarmops.AgentService(ctx, dcli); svc != nil {
		if cs := svc.Spec.TaskTemplate.ContainerSpec; cs != nil {
			image = cs.Image
		}
		drift = agentdeploy.Drift(*svc)
	}

	nodes, err := resolve.New(dcli, addrModeOf(cfg)).Nodes(ctx)
	if err != nil {
		return &cliError{code: exitTransport, err: fmt.Errorf("list swarm nodes: %w", err)}
	}
	var rows []doctorNode
	nodesOK, configError := true, ""
	if cfgErr != nil {
		nodesOK, configError = false, cfgErr.Error()
	} else {
		ac := agentcall.New(cfg)
		defer ac.Close()
		rows, nodesOK = doctorRows(ctx, ac.Versions(ctx, nodes, f.connectTimeout), g.version, ac.Explain)
	}
	report := doctorReport{
		Client: g.version.Binary, Proto: g.version.Proto, LatestRelease: latest(), Manager: info.Name,
		AgentDeployed: agentDep, Image: image, SpecDrift: drift,
		// Drift counts against healthy on purpose. The whole reason it is a
		// check is that the cluster looked healthy while it was not.
		Healthy: agentDep && nodesOK && len(drift) == 0,
		Nodes:   rows, ConfigError: configError,
	}
	if f.json {
		if err := printJSON(out, report); err != nil {
			return err
		}
	} else {
		printDoctorTable(out, report, info.Swarm.Nodes)
	}
	if !report.Healthy {
		return &cliError{code: exitTransport, silent: true}
	}
	return nil
}

// printManagerUnreachable is doctor's whole answer when the manager does not.
func printManagerUnreachable(out io.Writer, v Version, latest string, asJSON bool, err error) {
	if asJSON {
		_ = printJSON(out, doctorReport{Client: v.Binary, Proto: v.Proto, LatestRelease: latest, Manager: "unreachable", Healthy: false})
		return
	}
	printDoctorClient(out, v.Binary, v.Proto, latest)
	fmt.Fprintf(out, "manager: UNREACHABLE — %v\n", err)
}

// printDoctorClient is the client line, and the update line under it when a
// newer release exists.
func printDoctorClient(out io.Writer, binary, proto, latest string) {
	fmt.Fprintf(out, "client:  %s (proto %s)\n", binary, proto)
	if latest != "" {
		fmt.Fprintf(out, "update:  %s is available — see %s\n", latest, updatecheck.InstallURL)
	}
}

// doctorRows classifies each node's agent against this client; ok is false
// when any of them needs attention.
// explain turns a node's error into the sentence the operator reads.
func doctorRows(ctx context.Context, results []agentcall.NodeVersion, v Version, explain func(context.Context, error) error) (rows []doctorNode, ok bool) {
	ok = true
	rows = make([]doctorNode, 0, len(results))
	for _, r := range results {
		st, ver, proto, nodeOK := clusterread.ClassifyNode(r, v.Binary, v.Proto)
		if !nodeOK {
			ok = false
		}
		row := doctorNode{
			Node: r.Node.Name, Status: st, Version: ver, Proto: proto,
			Clock: r.Clock.String(), ClockOff: r.Clock.Beyond(clockskew.Warn),
		}
		// A clock far enough off to mistime certificates and scramble log
		// correlation is a finding, not a note: it is the cause operators
		// reach for last, after the hours the symptoms already cost.
		if row.ClockOff {
			ok = false
		}
		if r.Err != nil {
			row.Error = oneLine(explain(ctx, r.Err).Error())
		}
		rows = append(rows, row)
	}
	return rows, ok
}

// printDoctorTable is the human form of the report; swarmNodes is the
// manager's node count.
func printDoctorTable(out io.Writer, r doctorReport, swarmNodes int) {
	printDoctorClient(out, r.Client, r.Proto, r.LatestRelease)
	fmt.Fprintf(out, "manager: %s (%s)\n", r.Manager, plural(swarmNodes, "node"))
	if r.AgentDeployed {
		fmt.Fprintf(out, "agent:   service deployed")
		if r.Image != "" {
			fmt.Fprintf(out, " (%s)", r.Image)
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintln(out, "agent:   NOT DEPLOYED — run `swarmexec init`")
	}

	if r.ConfigError != "" {
		fmt.Fprintf(out, "config:  agent configuration incomplete, agents not probed: %s\n", r.ConfigError)
	}
	if len(r.SpecDrift) > 0 {
		fmt.Fprintf(out, "spec:    DRIFTED from what `swarmexec init` writes (%s)\n", plural(len(r.SpecDrift), "difference"))
		for _, d := range r.SpecDrift {
			fmt.Fprintf(out, "         - %s\n", d)
		}
		fmt.Fprintf(out, "         %s\n", agentdeploy.DriftHint)
	}

	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	// The ERROR column appears only when some node has one, so a healthy
	// table stays as narrow as it was.
	withErr, withClock := false, false
	for _, row := range r.Nodes {
		withErr = withErr || row.Error != ""
		// The clock column earns its place only when some node reported a
		// time: on a cluster of older agents it would be a column of dashes.
		withClock = withClock || row.Clock != ""
	}
	head := "NODE\tAGENT\tVERSION\tPROTO"
	if withClock {
		head += "\tCLOCK"
	}
	if withErr {
		head += "\tERROR"
	}
	fmt.Fprintln(w, head)
	for _, row := range r.Nodes {
		line := fmt.Sprintf("%s\t%s\t%s\t%s", row.Node, row.Status, dockerfmt.OrDash(row.Version), dockerfmt.OrDash(row.Proto))
		if withClock {
			line += "\t" + clockCell(row)
		}
		if withErr {
			line += "\t" + dockerfmt.OrDash(row.Error)
		}
		fmt.Fprintln(w, line)
	}
	_ = w.Flush()

	fmt.Fprintln(out)
	if r.Healthy {
		fmt.Fprintln(out, "✓ all agents healthy")
		return
	}
	fmt.Fprintln(out, "⚠ some checks need attention (see above)")
}

// oneLine keeps a table cell to one line: the first line of an explanation
// that may carry a hint on the next.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// clockCell is a node's clock offset, marked when it is past the threshold —
// the mark is what makes the column scannable, since every other value in it
// is a number that is fine.
func clockCell(row doctorNode) string {
	if row.Clock == "" {
		return "-"
	}
	if row.ClockOff {
		return row.Clock + " ⚠"
	}
	return row.Clock
}
