// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/moby/moby/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
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
	Client        string       `json:"client"`
	Proto         string       `json:"proto"`
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

	dcli, err := dockerEP.connect(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}

	infoRes, err := dcli.Info(ctx, client.InfoOptions{})
	info := infoRes.Info
	if err != nil {
		if f.json {
			_ = printJSON(out, doctorReport{Client: g.version.Binary, Proto: g.version.Proto, Manager: "unreachable", Healthy: false})
		} else {
			fmt.Fprintf(out, "client:  %s (proto %s)\nmanager: UNREACHABLE — %v\n", g.version.Binary, g.version.Proto, err)
		}
		return &cliError{code: session.TransportFailure, silent: true}
	}

	agentDep := agentDeployed(ctx, dcli)
	image := agentServiceImage(ctx, dcli)

	nodes, err := resolve.New(dcli, addrModeOf(cfg)).Nodes(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("list swarm nodes: %w", err)}
	}
	ac := agentcall.New(cfg)
	defer ac.Close()
	results := ac.Versions(ctx, nodes, f.connectTimeout)

	healthy := agentDep
	rows := make([]doctorNode, 0, len(results))
	for _, r := range results {
		st, ver, proto, ok := classifyNode(r, g.version)
		if !ok {
			healthy = false
		}
		rows = append(rows, doctorNode{Node: r.Node.Name, Status: st, Version: ver, Proto: proto})
	}

	if f.json {
		report := doctorReport{
			Client: g.version.Binary, Proto: g.version.Proto, Manager: info.Name,
			AgentDeployed: agentDep, Image: image, Healthy: healthy, Nodes: rows,
		}
		if err := printJSON(out, report); err != nil {
			return err
		}
		if !healthy {
			return &cliError{code: session.TransportFailure, silent: true}
		}
		return nil
	}

	fmt.Fprintf(out, "client:  %s (proto %s)\n", g.version.Binary, g.version.Proto)
	fmt.Fprintf(out, "manager: %s (%d nodes)\n", info.Name, info.Swarm.Nodes)
	if agentDep {
		fmt.Fprintf(out, "agent:   service deployed")
		if image != "" {
			fmt.Fprintf(out, " (%s)", image)
		}
		fmt.Fprintln(out)
	} else {
		fmt.Fprintln(out, "agent:   NOT DEPLOYED — run `swarmexec init`")
	}

	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tAGENT\tVERSION\tPROTO")
	for _, row := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", row.Node, row.Status, orDash(row.Version), orDash(row.Proto))
	}
	_ = w.Flush()

	fmt.Fprintln(out)
	if healthy {
		fmt.Fprintln(out, "✓ all agents healthy")
		return nil
	}
	fmt.Fprintln(out, "⚠ some checks need attention (see above)")
	return &cliError{code: session.TransportFailure, silent: true}
}

// classifyNode turns a node's version-probe result into a display status.
func classifyNode(r nodeHealth, v Version) (status, version, proto string, ok bool) {
	switch {
	case r.Err != nil && agentTooOld(r.Err):
		return "too old (init --force)", "-", "-", false
	case r.Err != nil:
		return "unreachable", "-", "-", false
	case r.Proto != v.Proto:
		return "PROTO MISMATCH", r.Version, r.Proto, false
	case r.Version != v.Binary && v.Binary != "" && v.Binary != "dev":
		return "ok (version skew)", r.Version, r.Proto, true
	default:
		return "ok", r.Version, r.Proto, true
	}
}

// nodeHealth is one node's Version answer (agentcall.Versions).
type nodeHealth = agentcall.NodeVersion
