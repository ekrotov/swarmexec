// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package portfwd is the client half of port-forwarding: the local listener,
// the gRPC stream per accepted connection, the two pumps that carry bytes
// between them, and the registry of forwards the TUI keeps alive across
// cluster switches.
//
// It lived in the cli package, next to 16 000 lines of TUI, which is the wrong
// place for a data path: a forward moves an operator's traffic and has to get
// half-close and teardown exactly right, and that is easier to see — and to
// test — on its own.
package portfwd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/resolve"
	pb "swarmexec/internal/pb"
)

// Params is one forward as the operator asked for it.
type Params struct {
	// Address is the local bind address; DefaultAddress keeps it on loopback.
	Address string
	// LocalPort is 0 to let the kernel pick; LocalAddr then reports which.
	LocalPort uint32
	// RemotePort is the port inside the container.
	RemotePort uint32
	// ConnectTimeout bounds the dial to the agent, not the forward itself.
	ConnectTimeout time.Duration
}

// Forwarder holds one gRPC connection to the agent plus the local listener. It
// opens a PortForward stream per accepted connection; HTTP/2 multiplexes those
// over the single transport, so no connection id is needed on the wire.
//
// Setup (Start) is separate from serving (Serve) so a caller can learn
// the bound address — and surface a bind conflict — before committing to a
// blocking loop. The TUI needs exactly that; the command just chains the two.
type Forwarder struct {
	conn        *agentcall.Conn
	ln          net.Listener
	containerID string
	remotePort  uint32
	auth        agenterr.Auth // which credential an Unauthenticated is about
	onConnOK    func()        // see SetOnConnOK
}

// Start binds the local port and dials the agent. On success the
// caller owns the Forwarder and must Close it.
//
// Bind first, dial second, and the order is the point: the local bind is the
// cheap check that fails for a reason the operator can act on immediately (the
// port is taken), while the dial goes over ssh to another machine and can take
// the whole connect timeout. Doing it the other way round — as this did — meant
// a forward that was never going to work still paid for a full agent connection
// before saying so.
//
// The listener existing slightly before the agent is reachable is harmless:
// Serve is a separate call, so nothing is accepted in between. Early arrivals
// wait in the listen backlog and are reset if the dial fails and we close it.
func Start(ctx context.Context, ac *agentcall.Client, ep resolve.Endpoint, p Params) (*Forwarder, error) {
	bind := net.JoinHostPort(p.Address, strconv.FormatUint(uint64(p.LocalPort), 10))
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", bind, err)
	}

	dctx, dcancel := context.WithTimeout(ctx, p.ConnectTimeout)
	conn, err := ac.Conn(dctx, ep.DialHost)
	dcancel()
	if err != nil {
		ln.Close()
		return nil, err
	}
	return &Forwarder{conn: conn, ln: ln, containerID: ep.ContainerID, remotePort: p.RemotePort, auth: agenterr.AuthOf(ac.Config().SecretMode())}, nil
}

// LocalAddr is the address actually bound, which is what to show the operator:
// with local port 0 the kernel picks one, and only this reports which.
// onConnOK, when set before Serve, is called for each connection that ended
// cleanly. The TUI uses it to take the failure marker off a forward that works
// again.
func (f *Forwarder) SetOnConnOK(fn func()) { f.onConnOK = fn }

func (f *Forwarder) LocalAddr() net.Addr { return f.ln.Addr() }

func (f *Forwarder) Close() error {
	err := f.ln.Close()
	f.conn.Close()
	return err
}

// Serve accepts connections until ctx is cancelled or the listener fails.
// onConnErr, when non-nil, is called for each connection that fails; a single
// bad connection never tears the listener down, because the operator keeps the
// forward and simply reconnects their client.
func (f *Forwarder) Serve(ctx context.Context, onConnErr func(error)) error {
	return f.serve(ctx, f.conn.Agent(), onConnErr)
}

// serve takes the agent client as an argument so the accept loop can be tested
// against a fake agent without a TLS listener and a real agentcall.Conn. The
// loop is the part worth testing: one bad connection must not take the
// listener down with it.
func (f *Forwarder) serve(ctx context.Context, client pb.AgentClient, onConnErr func(error)) error {
	// Unblock Accept on cancellation.
	go func() {
		<-ctx.Done()
		_ = f.ln.Close()
	}()

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
			ferr := bridge(ctx, client, local, f.containerID, f.remotePort)
			if ferr == nil && f.onConnOK != nil {
				f.onConnOK()
			}
			if ferr != nil && onConnErr != nil {
				// Against an agent that predates PortForward every single
				// connection fails, so leaving the raw "unknown method
				// PortForward" to repeat in the operator's face would bury
				// the one thing they need to do about it; Explain says it.
				onConnErr(agenterr.Explain(ctx, ferr, agenterr.Options{Auth: f.auth}))
			}
		}()
	}
}

// bridge bridges one accepted local connection to the container port over
// a single PortForward stream.
func bridge(ctx context.Context, client pb.AgentClient, local net.Conn, containerID string, port uint32) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := client.PortForward(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&pb.ForwardClientMessage{
		Payload: &pb.ForwardClientMessage_Start{
			Start: &pb.StartForward{ContainerId: containerID, Port: port},
		},
	}); err != nil {
		return err
	}

	// Wait for readiness before piping, so a closed target port surfaces as an
	// error here instead of as a mysteriously silent connection.
	if err := awaitReady(stream); err != nil {
		return err
	}
	go pumpToAgent(local, stream, cancel)
	return pumpFromAgent(ctx, stream, local)
}

// awaitReady reads the agent's first message: ready, or why not.
func awaitReady(stream pb.Agent_PortForwardClient) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	switch pl := first.Payload.(type) {
	case *pb.ForwardServerMessage_Ready:
		return nil // connected
	case *pb.ForwardServerMessage_Error:
		return errors.New(pl.Error)
	default:
		return fmt.Errorf("agent sent %T before ready", pl)
	}
}

// pumpToAgent copies local -> container until either side gives up: a failed
// send cancels the bridge, the local peer closing half-closes toward the
// container (the other direction keeps going).
func pumpToAgent(local net.Conn, stream pb.Agent_PortForwardClient, cancel context.CancelFunc) {
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
}

// pumpFromAgent copies container -> local until the stream ends; only an
// agent-side failure is an error.
func pumpFromAgent(ctx context.Context, stream pb.Agent_PortForwardClient, local net.Conn) error {
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
