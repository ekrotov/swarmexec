package cli

import (
	"context"
	"fmt"
	"sync"
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
	return cmd
}

func runDoctor(cmd *cobra.Command, g *globalFlags, f *doctorFlags) error {
	cfg, err := g.resolveConfig(cmd)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	out := cmd.OutOrStdout()

	dcli, err := newDockerClient(g.dockerContext)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}

	fmt.Fprintf(out, "client:  %s (proto %s)\n", g.version.Binary, g.version.Proto)

	info, err := dcli.Info(ctx)
	if err != nil {
		fmt.Fprintf(out, "manager: UNREACHABLE — %v\n", err)
		return &cliError{code: session.TransportFailure, silent: true}
	}
	fmt.Fprintf(out, "manager: %s (%d nodes)\n", info.Name, info.Swarm.Nodes)

	healthy := true
	if agentDeployed(ctx, dcli) {
		fmt.Fprintln(out, "agent:   service deployed")
	} else {
		fmt.Fprintln(out, "agent:   NOT DEPLOYED — run `swarmexec init`")
		healthy = false
	}

	nodes, err := resolve.New(dcli, addrModeOf(cfg)).Nodes(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("list swarm nodes: %w", err)}
	}

	results := checkNodes(ctx, cfg, nodes, f.connectTimeout)
	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tAGENT\tVERSION\tPROTO")
	for _, r := range results {
		st, ver, proto := "ok", r.version, r.proto
		switch {
		case r.err != nil && agentTooOld(r.err):
			st, ver, proto, healthy = "too old (init --force)", "-", "-", false
		case r.err != nil:
			st, ver, proto, healthy = "unreachable", "-", "-", false
		case r.proto != g.version.Proto:
			st, healthy = "PROTO MISMATCH", false
		case r.version != g.version.Binary && g.version.Binary != "" && g.version.Binary != "dev":
			st = "ok (version skew)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.node.Name, st, orDash(ver), orDash(proto))
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

type nodeHealth struct {
	node    resolve.Node
	version string
	proto   string
	err     error
}

// checkNodes queries every node's agent Version RPC in parallel.
func checkNodes(ctx context.Context, cfg config.Config, nodes []resolve.Node, connectTimeout time.Duration) []nodeHealth {
	out := make([]nodeHealth, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n resolve.Node) {
			defer wg.Done()
			v, p, err := checkNodeVersion(ctx, cfg, n, connectTimeout)
			out[i] = nodeHealth{node: n, version: v, proto: p, err: err}
		}(i, n)
	}
	wg.Wait()
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
