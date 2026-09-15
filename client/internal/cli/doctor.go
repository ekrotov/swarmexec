// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	pb "swarmexec/internal/pb"
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

	info, err := dcli.Info(ctx)
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
	results := checkNodes(ctx, cfg, nodes, f.connectTimeout)

	healthy := agentDep
	rows := make([]doctorNode, 0, len(results))
	for _, r := range results {
		st, ver, proto, ok := classifyNode(r, g.version)
		if !ok {
			healthy = false
		}
		rows = append(rows, doctorNode{Node: r.node.Name, Status: st, Version: ver, Proto: proto})
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
	case r.err != nil && agentTooOld(r.err):
		return "too old (init --force)", "-", "-", false
	case r.err != nil:
		return "unreachable", "-", "-", false
	case r.proto != v.Proto:
		return "PROTO MISMATCH", r.version, r.proto, false
	case r.version != v.Binary && v.Binary != "" && v.Binary != "dev":
		return "ok (version skew)", r.version, r.proto, true
	default:
		return "ok", r.version, r.proto, true
	}
}

type nodeHealth struct {
	node    resolve.Node
	version string
	proto   string
	err     error
}

// checkNodes queries every node's agent Version RPC in parallel.
func checkNodes(ctx context.Context, cfg config.Config, nodes []resolve.Node, connectTimeout time.Duration) []nodeHealth {
	out := make([]nodeHealth, len(nodes))
	forEachNode(nodes, func(i int, n resolve.Node) {
		v, p, err := checkNodeVersion(ctx, cfg, n, connectTimeout)
		out[i] = nodeHealth{node: n, version: v, proto: p, err: err}
	})
	return out
}

func checkNodeVersion(ctx context.Context, cfg config.Config, n resolve.Node, connectTimeout time.Duration) (version, proto string, err error) {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := dial.Dial(dctx, n.DialHost, cfg.Port, cfg)
	cancel()
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	resp, err := pb.NewAgentClient(conn).Version(ctx, &pb.VersionRequest{})
	if err != nil {
		return "", "", err
	}
	return resp.Version, resp.ProtoVersion, nil
}
