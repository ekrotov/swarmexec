// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/secscan"
	"swarmexec/client/internal/session"
)

// The security report is the scan the ui already shows under "!", written out
// as a file so it can be reviewed away from the terminal, attached to a ticket,
// or committed next to the stack files and diffed release over release. That
// last use is why the ordering is total and why the report states what it did
// NOT cover: a diff is only trustworthy if an absent finding means "not found"
// rather than "not looked at".

type securityFlags struct {
	output         string
	connectTimeout time.Duration
	skipAgents     bool
}

func newSecurityCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "security",
		Short: "Security scan of the swarm",
	}
	cmd.AddCommand(newSecurityReportCmd(g))
	return cmd
}

func newSecurityReportCmd(g *globalFlags) *cobra.Command {
	f := &securityFlags{}
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Write a Markdown security report for the whole cluster",
		Long: "Scan every service, network, secret, config and node in the cluster and\n" +
			"write the findings as Markdown. Writes to stdout unless -o is given, so it\n" +
			"can be piped as well as saved.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSecurityReport(cmd, g, f)
		},
	}
	cmd.Flags().StringVarP(&f.output, "output", "o", "", "write to this file instead of stdout")
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "per-node connect timeout when checking agents")
	cmd.Flags().BoolVar(&f.skipAgents, "skip-agents", false, "do not contact the node agents (faster; agent version skew is then reported as not covered)")
	return cmd
}

func runSecurityReport(cmd *cobra.Command, g *globalFlags, f *securityFlags) error {
	dockerEP := resolveEndpoint(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, dockerEP)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	ctx := cmdContext(cmd)

	dcli, err := dockerEP.connect(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}

	report, err := gatherSecurityReport(ctx, dcli, cfg, g, f, dockerEP.Context)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}

	md := report.Markdown()
	if f.output == "" {
		_, err := fmt.Fprint(cmd.OutOrStdout(), md)
		return err
	}
	if err := writeReportFile(f.output, md); err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	// To stderr, so `-o` plus a redirect of stdout stays clean.
	fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s — %s\n", f.output, reportSummaryLine(report))
	return nil
}

// writeReportFile writes the report, creating parent directories. 0o600: the
// report names every service, network and secret in the cluster and every
// weakness found in it — it is a map of where to attack, and it has no business
// being world-readable by default.
func writeReportFile(path, body string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// gatherSecurityReport collects everything the scan needs from the manager (and
// optionally the agents) and builds the report.
//
// The manager calls are required: a report missing half the cluster is worse
// than no report, because it looks complete. The agent round trip is optional
// and its absence is recorded as a gap rather than passed over.
func gatherSecurityReport(ctx context.Context, dcli *client.Client, cfg config.Config, g *globalFlags, f *securityFlags, contextName string) (secscan.Report, error) {
	svcs, err := listServicesForScan(ctx, dcli)
	if err != nil {
		return secscan.Report{}, fmt.Errorf("list services: %w", err)
	}

	cluster, err := gatherCluster(ctx, dcli)
	if err != nil {
		return secscan.Report{}, err
	}
	cluster.ClientVersion, cluster.ClientProto = g.version.Binary, g.version.Proto

	if !f.skipAgents {
		nodes, nerr := resolve.New(dcli, addrModeOf(cfg)).Nodes(ctx)
		if nerr == nil {
			applyAgentVersions(ctx, &cluster, cfg, nodes, f.connectTimeout)
		}
		// A failure to resolve dialable nodes leaves AgentsChecked false, which
		// the report turns into a stated gap. Better than a half-filled node
		// list that reads as complete.
	}

	meta := secscan.ReportMeta{
		Context:   contextLabel(ctx, dcli, contextName),
		Generated: time.Now(),
		Tool:      "swarmexec " + g.version.Binary,
	}
	return secscan.BuildReport(meta, svcs, cluster), nil
}

// applyAgentVersions asks every node's agent for its version and records the
// answer — including the failures, which are findings in their own right.
func applyAgentVersions(ctx context.Context, c *secscan.Cluster, cfg config.Config, nodes []resolve.Node, timeout time.Duration) {
	results := checkNodes(ctx, cfg, nodes, timeout)
	byName := map[string]nodeHealth{}
	for _, r := range results {
		byName[r.node.Name] = r
	}
	for i := range c.Nodes {
		r, ok := byName[c.Nodes[i].Name]
		if !ok {
			continue
		}
		switch {
		case r.err != nil && agentTooOld(r.err):
			// The same signal doctor reads: an agent predating the Version RPC
			// answers Unimplemented rather than failing to connect.
			c.Nodes[i].AgentTooOld = true
		case r.err != nil:
			c.Nodes[i].AgentError = r.err.Error()
		default:
			c.Nodes[i].AgentVersion, c.Nodes[i].AgentProto = r.version, r.proto
		}
	}
	c.AgentsChecked = true
}
