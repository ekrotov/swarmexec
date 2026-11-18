// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"time"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/mcp"
	"swarmexec/client/internal/mcpaccess"
)

type mcpFlags struct {
	defaultAccess  string
	allow          []string
	contexts       []string
	connectTimeout time.Duration
}

func newMCPCmd(g *globalFlags) *cobra.Command {
	f := &mcpFlags{}
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the swarm to an AI assistant over MCP (stdio)",
		Long: "Run a Model Context Protocol server on stdin/stdout, so an AI assistant can\n" +
			"inspect the swarm: services, tasks, logs, stats, stack diffs. Register it with\n\n" +
			"  claude mcp add swarmexec -- swarmexec mcp\n\n" +
			"A service is visible only with the service label swarmexec.mcp.enable=true\n" +
			"(compose: deploy.labels). swarmexec.mcp.logs=true allows its logs,\n" +
			"swarmexec.mcp.exec=true a non-interactive command in its containers. An\n" +
			"explicit \"false\" always wins. --default list makes unlabelled services\n" +
			"visible (listed, inspected, their stats read), never their logs or exec.\n\n" +
			"The labels themselves are never shown to the assistant, and no tool changes a\n" +
			"service, stack or label; the one tool that is not read-only is exec_run, which\n" +
			"runs a command in a service that has swarmexec.mcp.exec=true. A service that mounts the Docker socket is\n" +
			"never exec'd into, whatever its labels say.\n\n" +
			"ssh cannot prompt here: stdin and stdout are the protocol. Load the key for\n" +
			"an ssh:// context into ssh-agent first. Logging goes to the log file.\n\n" +
			"Only the docker context the server starts with (--context, $DOCKER_CONTEXT,\n" +
			"$DOCKER_HOST or docker's current one) is reachable, plus those named with\n" +
			"--contexts. Every other context does not exist as far as the assistant can\n" +
			"tell: it is not listed, and asking for it is \"unknown docker context\".\n\n" +
			"Masking secrets in env and inspect output is cosmetic once exec is granted:\n" +
			"a command run in the container can print its environment.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMCP(cmd, g, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.defaultAccess, "default", "none", "access for services without swarmexec.mcp.enable: none | list (\"read\" is accepted for list)")
	fl.StringSliceVar(&f.allow, "allow", nil, "enable optional tools: report (security_report)")
	fl.StringSliceVar(&f.contexts, "contexts", nil, "further docker contexts the assistant may use, besides the one the server starts with (comma list or repeated)")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to an agent")
	return cmd
}

func runMCP(cmd *cobra.Command, g *globalFlags, f *mcpFlags) error {
	def, err := mcpaccess.ParseDefault(f.defaultAccess)
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}
	allowReport, err := mcp.ParseAllow(f.allow)
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}
	extra, err := mcp.ParseContexts(f.contexts)
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}

	// From here on stdout carries the protocol and nothing else. A stray line
	// on it — a library's fmt.Println, a helper that prints a notice — is a
	// corrupt frame and a dead session, so the real stdout goes to the
	// transport alone and every other writer that asks for os.Stdout gets
	// stderr, which the assistant's host logs.
	protocol := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = protocol }()

	// ssh must never ask. There is no terminal to ask on: stdin is the
	// protocol too, so a passphrase prompt would read a JSON frame as the
	// answer — or hang the session waiting for one.
	sshNonInteractive.Store(true)

	s := mcp.New(mcp.Options{
		Policy:         mcpaccess.Policy{Default: def},
		AllowReport:    allowReport,
		ConnectTimeout: f.connectTimeout,
		DockerContext:  g.dockerContext,
		Contexts:       extra,
		ClientVersion:  g.version.Binary,
		ClientProto:    g.version.Proto,
		Dial:           mcpDialer(cmd, g),
		IsSSHAuthError: isSSHAuthError,
	})
	defer s.Close()

	if err := s.Serve(cmdContext(cmd), os.Stdin, protocol); err != nil {
		return &cliError{code: exitNo, err: err}
	}
	return nil
}

// mcpDialer reaches a docker context for the MCP server the way every other
// command does: one endpoint resolution for the manager connection and the
// agent tunnel, and the agent configuration from flags, environment and file
// for that endpoint.
func mcpDialer(cmd *cobra.Command, g *globalFlags) func(context.Context, string) (mcp.Endpoint, error) {
	return func(ctx context.Context, name string) (mcp.Endpoint, error) {
		ep := resolveEndpoint(name)
		cfg, err := g.resolveConfig(cmd, ep)
		if err != nil {
			return mcp.Endpoint{}, err
		}
		dcli, err := ep.connect(ctx)
		if err != nil {
			return mcp.Endpoint{}, err
		}
		return mcp.Endpoint{Label: ep.Context, Docker: dcli, Config: cfg}, nil
	}
}
