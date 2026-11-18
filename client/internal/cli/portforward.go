// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/endpoint"
	"swarmexec/client/internal/portfwd"
	"swarmexec/client/internal/resolve"
)

type portForwardFlags struct {
	address        string
	node           string
	connectTimeout time.Duration
}

func newPortForwardCmd(g *globalFlags) *cobra.Command {
	f := &portForwardFlags{}
	cmd := &cobra.Command{
		Use:     "port-forward [flags] <service|service.slot|task-id|container-id> [local:]remote",
		Aliases: []string{"pf"},
		Short:   "Forward a local port to a port inside a container in the swarm",
		Long: "Forward a local TCP port to a port inside a container anywhere in the\n" +
			"swarm, without publishing that port on the cluster.\n\n" +
			"The local port defaults to the remote one, so `port-forward api 8080`\n" +
			"listens on 127.0.0.1:8080 and forwards to port 8080 in the container.\n\n" +
			"Targeting a service forwards to exactly ONE of its tasks (the one the\n" +
			"target resolves to), not across all replicas — a forward that silently\n" +
			"load-balanced would make debugging misleading.",
		Example: "  swarmexec port-forward db 5432\n" +
			"  swarmexec pf web 9090:8080\n" +
			"  swarmexec port-forward web.2 8080",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPortForward(cmd, g, f, args)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.address, "address", portfwd.DefaultAddress,
		"local address to bind; loopback by default so the forwarded port is not exposed to your network")
	fl.StringVar(&f.node, "node", "", "node hint/override for container-id targets")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to the agent")
	return cmd
}

func runPortForward(cmd *cobra.Command, g *globalFlags, f *portForwardFlags, args []string) error {
	dockerEP := endpoint.Resolve(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, dockerEP)
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: exitUsage, err: err}
	}

	localPort, remotePort, err := portfwd.ParsePortSpec(args[1])
	if err != nil {
		return &cliError{code: exitUsage, err: err}
	}

	if !portfwd.IsLoopback(f.address) {
		// Same warning the TUI form shows: the internal port this reaches is
		// now open to whoever can reach this machine.
		fmt.Fprintf(cmd.ErrOrStderr(), "swarmexec: warning: --address %s is not loopback; the forwarded port is reachable from your network\n", f.address)
	}

	ctx := cmdContext(cmd)

	dcli, err := dockerEP.Connect(ctx)
	if err != nil {
		return &cliError{code: exitTransport, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))
	ep, err := r.Resolve(ctx, resolve.Request{Target: args[0], NodeHint: f.node})
	if err != nil {
		if amb, ok := err.(*resolve.AmbiguousError); ok {
			ep, err = pickCandidate(amb, false, cmd.ErrOrStderr())
		}
		if err != nil {
			return &cliError{code: exitTransport, err: err}
		}
	}

	ac := agentcall.New(cfg)
	defer ac.Close()
	err = runForwarder(ctx, ac, *ep, portfwd.Params{
		Address:        f.address,
		LocalPort:      localPort,
		RemotePort:     remotePort,
		ConnectTimeout: f.connectTimeout,
	}, cmd.OutOrStdout(), cmd.ErrOrStderr())
	if err != nil {
		return &cliError{code: exitTransport, err: explainSession(ctx, cfg, dcli, err)}
	}
	return nil
}

// runForwarder is the blocking command-line path: set up, announce, serve.
// The announcement stays here rather than in portfwd: what a command prints is
// the command layer's business, and it is the only thing the two callers of
// the forwarder do differently.
func runForwarder(ctx context.Context, ac *agentcall.Client, ep resolve.Endpoint, p portfwd.Params, stdout, stderr io.Writer) error {
	fw, err := portfwd.Start(ctx, ac, ep, p)
	if err != nil {
		return err
	}
	defer fw.Close()

	fmt.Fprintf(stdout, "forwarding %s -> %s:%d (%s) — press Ctrl-C to stop\n",
		fw.LocalAddr(), shortID(ep.ContainerID), p.RemotePort, ep.NodeName)

	return fw.Serve(ctx, func(cerr error) {
		fmt.Fprintf(stderr, "swarmexec: forward connection failed: %v\n", cerr)
	})
}

// annotateForwards appends the forward markers for a container to its tree row.
func annotateForwards(label string, fws []portfwd.Entry) string {
	if len(fws) == 0 {
		return label
	}
	parts := make([]string, 0, len(fws))
	for i := range fws {
		parts = append(parts, fws[i].Label())
	}
	return label + "  " + strings.Join(parts, " ")
}
