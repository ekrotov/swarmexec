// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmops"
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
		Short: "Security report of the cluster, and checks of stack files before deploying",
	}
	// check is stack check under the security heading (review P-02b): the one
	// place an operator looking for "scan this file" will look.
	cmd.AddCommand(newSecurityReportCmd(g), newStackCheckCmd(g))
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

// reportOptions is the report run these flags ask for.
func (f *securityFlags) reportOptions(g *globalFlags, cfg config.Config, contextName string) swarmops.SecurityReportOptions {
	return swarmops.SecurityReportOptions{
		ContextName:    contextName,
		ClientVersion:  g.version.Binary,
		ClientProto:    g.version.Proto,
		Tool:           "swarmexec " + g.version.Binary,
		SkipAgents:     f.skipAgents,
		ConnectTimeout: f.connectTimeout,
		AddrMode:       addrModeOf(cfg),
	}
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

	ac := agentcall.New(cfg)
	defer ac.Close()
	report, err := swarmops.GatherSecurityReport(ctx, dcli, ac, f.reportOptions(g, cfg, dockerEP.Context))
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
