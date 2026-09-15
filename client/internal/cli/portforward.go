// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	pb "swarmexec/internal/pb"
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
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPortForward(cmd, g, f, args)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.address, "address", "127.0.0.1",
		"local address to bind; loopback by default so the forwarded port is not exposed to your network")
	fl.StringVar(&f.node, "node", "", "node hint/override for container-id targets")
	fl.DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to the agent")
	return cmd
}

func runPortForward(cmd *cobra.Command, g *globalFlags, f *portForwardFlags, args []string) error {
	dockerEP := resolveEndpoint(g.dockerContext)
	cfg, err := g.resolveConfig(cmd, dockerEP)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}

	localPort, remotePort, err := parsePortSpec(args[1])
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}

	ctx := cmdContext(cmd)

	dcli, err := dockerEP.connect(ctx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))
	ep, err := r.Resolve(ctx, resolve.Request{Target: args[0], NodeHint: f.node})
	if err != nil {
		if amb, ok := err.(*resolve.AmbiguousError); ok {
			ep, err = pickCandidate(amb, false)
		}
		if err != nil {
			return &cliError{code: session.TransportFailure, err: err}
		}
	}

	err = runForwarder(ctx, cfg, *ep, forwardParams{
		address:        f.address,
		localPort:      localPort,
		remotePort:     remotePort,
		connectTimeout: f.connectTimeout,
	}, cmd.OutOrStdout(), cmd.ErrOrStderr())
	if err != nil {
		return &cliError{code: session.TransportFailure, err: enrichAgentError(ctx, dcli, err)}
	}
	return nil
}

// parsePortSpec accepts "8080" (same port both sides) or "9090:8080"
// (local:remote), matching the kubectl port-forward spelling.
func parsePortSpec(spec string) (local, remote uint32, err error) {
	parsePort := func(s, what string) (uint32, error) {
		n, perr := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
		if perr != nil || n == 0 || n > 65535 {
			return 0, fmt.Errorf("invalid %s port %q: expected 1-65535", what, s)
		}
		return uint32(n), nil
	}

	l, r, found := strings.Cut(spec, ":")
	if !found {
		p, perr := parsePort(spec, "")
		return p, p, perr
	}
	if local, err = parsePort(l, "local"); err != nil {
		return 0, 0, err
	}
	if remote, err = parsePort(r, "remote"); err != nil {
		return 0, 0, err
	}
	return local, remote, nil
}

type forwardParams struct {
	address        string
	localPort      uint32
	remotePort     uint32
	connectTimeout time.Duration
}

// forwarder holds one gRPC connection to the agent plus the local listener. It
// opens a PortForward stream per accepted connection; HTTP/2 multiplexes those
// over the single transport, so no connection id is needed on the wire.
//
// Setup (startForwarder) is separate from serving (Serve) so a caller can learn
// the bound address — and surface a bind conflict — before committing to a
// blocking loop. The TUI needs exactly that; the command just chains the two.
type forwarder struct {
	conn        *grpc.ClientConn
	ln          net.Listener
	containerID string
	remotePort  uint32
}

// startForwarder dials the agent and binds the local port. On success the
// caller owns the forwarder and must Close it.
func startForwarder(ctx context.Context, cfg config.Config, ep resolve.Endpoint, p forwardParams) (*forwarder, error) {
	dctx, dcancel := context.WithTimeout(ctx, p.connectTimeout)
	conn, err := dial.Dial(dctx, ep.DialHost, cfg.Port, cfg)
	dcancel()
	if err != nil {
		return nil, err
	}

	bind := net.JoinHostPort(p.address, strconv.FormatUint(uint64(p.localPort), 10))
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", bind)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("listen on %s: %w", bind, err)
	}
	return &forwarder{conn: conn, ln: ln, containerID: ep.ContainerID, remotePort: p.remotePort}, nil
}

// LocalAddr is the address actually bound, which is what to show the operator:
// with local port 0 the kernel picks one, and only this reports which.
func (f *forwarder) LocalAddr() net.Addr { return f.ln.Addr() }

func (f *forwarder) Close() error {
	err := f.ln.Close()
	f.conn.Close()
	return err
}

// Serve accepts connections until ctx is cancelled or the listener fails.
// onConnErr, when non-nil, is called for each connection that fails; a single
// bad connection never tears the listener down, because the operator keeps the
// forward and simply reconnects their client.
func (f *forwarder) Serve(ctx context.Context, onConnErr func(error)) error {
	// Unblock Accept on cancellation.
	go func() {
		<-ctx.Done()
		_ = f.ln.Close()
	}()

	client := pb.NewAgentClient(f.conn)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		local, aerr := f.ln.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return nil // cancelled: a clean stop, not a failure
			}
			return fmt.Errorf("accept on %s: %w", f.ln.Addr(), aerr)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer local.Close()
			if ferr := forwardConn(ctx, client, local, f.containerID, f.remotePort); ferr != nil && onConnErr != nil {
				onConnErr(ferr)
			}
		}()
	}
}

// runForwarder is the blocking command-line path: set up, announce, serve.
func runForwarder(ctx context.Context, cfg config.Config, ep resolve.Endpoint, p forwardParams, stdout, stderr io.Writer) error {
	fw, err := startForwarder(ctx, cfg, ep, p)
	if err != nil {
		return err
	}
	defer fw.Close()

	fmt.Fprintf(stdout, "forwarding %s -> %s:%d (%s) — press Ctrl-C to stop\n",
		fw.LocalAddr(), shortID(ep.ContainerID), p.remotePort, ep.NodeName)

	return fw.Serve(ctx, func(cerr error) {
		fmt.Fprintf(stderr, "swarmexec: forward connection failed: %v\n", cerr)
	})
}

// forwardSetupError makes a stream-setup failure actionable. Against an agent
// that predates PortForward every single connection fails, so leaving the raw
// "unknown method PortForward" to repeat in the operator's face would bury the
// one thing they need to do about it.
func forwardSetupError(err error) error {
	if agentTooOld(err) {
		return errAgentTooOld
	}
	return err
}

// forwardConn bridges one accepted local connection to the container port over
// a single PortForward stream.
func forwardConn(ctx context.Context, client pb.AgentClient, local net.Conn, containerID string, port uint32) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := client.PortForward(ctx)
	if err != nil {
		return forwardSetupError(err)
	}
	if err := stream.Send(&pb.ForwardClientMessage{
		Payload: &pb.ForwardClientMessage_Start{
			Start: &pb.StartForward{ContainerId: containerID, Port: port},
		},
	}); err != nil {
		return forwardSetupError(err)
	}

	// Wait for readiness before piping, so a closed target port surfaces as an
	// error here instead of as a mysteriously silent connection.
	first, err := stream.Recv()
	if err != nil {
		return forwardSetupError(err)
	}
	switch pl := first.Payload.(type) {
	case *pb.ForwardServerMessage_Ready:
		// connected
	case *pb.ForwardServerMessage_Error:
		return errors.New(pl.Error)
	default:
		return fmt.Errorf("agent sent %T before ready", pl)
	}

	// local -> container
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := local.Read(buf)
			if n > 0 {
				// Copy out of the reused buffer before it enters a protobuf
				// message (CONTRACT.md §6).
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				if serr := stream.Send(&pb.ForwardClientMessage{
					Payload: &pb.ForwardClientMessage_Data{Data: chunk},
				}); serr != nil {
					cancel()
					return
				}
			}
			if rerr != nil {
				// Local peer closed its write side: half-close toward the
				// container and keep reading the other direction.
				_ = stream.CloseSend()
				return
			}
		}
	}()

	// container -> local
	for {
		msg, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return rerr
		}
		switch pl := msg.Payload.(type) {
		case *pb.ForwardServerMessage_Data:
			if _, werr := local.Write(pl.Data); werr != nil {
				return nil // local peer went away; not an agent-side failure
			}
		case *pb.ForwardServerMessage_Error:
			return errors.New(pl.Error)
		}
	}
}
