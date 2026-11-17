// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmops"
	"swarmexec/client/internal/updatecheck"
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
	LatestRelease string       `json:"latest_release,omitempty"`
	Manager       string       `json:"manager"`
	AgentDeployed bool         `json:"agent_deployed"`
	Image         string       `json:"image,omitempty"`
	Healthy       bool         `json:"healthy"`
	Nodes         []doctorNode `json:"nodes,omitempty"`
}

type doctorNode struct {
	Node    string `json:"node"`
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
	Proto   string `json:"proto,omitempty"`
}

func runDoctor(cmd *cobra.Command, g *globalFlags, f *doctorFlags) error {
	dockerEP := resolveEndpoint(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, dockerEP)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	ctx := cmdContext(cmd)
	out := cmd.OutOrStdout()
	// Asked alongside the cluster probes; at most a 3 s wait, usually a
	// cache read.
	latest := g.startNewerRelease(ctx, cfg)

	dcli, err := dockerEP.connect(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}

	infoRes, err := dcli.Info(ctx, client.InfoOptions{})
	info := infoRes.Info
	if err != nil {
		printManagerUnreachable(out, g.version, latest(), f.json, err)
		return &cliError{code: session.TransportFailure, silent: true}
	}

	agentDep := swarmops.AgentDeployed(ctx, dcli)
	image := swarmops.AgentServiceImage(ctx, dcli)

	nodes, err := resolve.New(dcli, addrModeOf(cfg)).Nodes(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("list swarm nodes: %w", err)}
	}
	ac := agentcall.New(cfg)
	defer ac.Close()
	results := ac.Versions(ctx, nodes, f.connectTimeout)

	rows, nodesOK := doctorRows(results, g.version)
	report := doctorReport{
		Client: g.version.Binary, Proto: g.version.Proto, LatestRelease: latest(), Manager: info.Name,
		AgentDeployed: agentDep, Image: image, Healthy: agentDep && nodesOK, Nodes: rows,
	}
	if f.json {
		if err := printJSON(out, report); err != nil {
			return err
		}
	} else {
		printDoctorTable(out, report, info.Swarm.Nodes)
	}
	if !report.Healthy {
		return &cliError{code: session.TransportFailure, silent: true}
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
func doctorRows(results []agentcall.NodeVersion, v Version) (rows []doctorNode, ok bool) {
	ok = true
	rows = make([]doctorNode, 0, len(results))
	for _, r := range results {
		st, ver, proto, nodeOK := swarmops.ClassifyNode(r, v.Binary, v.Proto)
		if !nodeOK {
			ok = false
		}
		rows = append(rows, doctorNode{Node: r.Node.Name, Status: st, Version: ver, Proto: proto})
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

	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tAGENT\tVERSION\tPROTO")
	for _, row := range r.Nodes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", row.Node, row.Status, orDash(row.Version), orDash(row.Proto))
	}
	_ = w.Flush()

	fmt.Fprintln(out)
	if r.Healthy {
		fmt.Fprintln(out, "✓ all agents healthy")
		return
	}
	fmt.Fprintln(out, "⚠ some checks need attention (see above)")
}
