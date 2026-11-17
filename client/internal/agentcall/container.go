// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	"swarmexec/internal/pb"
)

// Calls about one container, on the node it runs on.

// ProbeShells reports which of bash/sh the container can actually start. It
// runs a quick non-interactive `command -v` through sh; if sh itself is
// missing it falls back to probing bash directly. Used to grey out
// unavailable shells.
func (c *Client) ProbeShells(ctx context.Context, ep resolve.Endpoint, connectTimeout time.Duration) (bash, sh bool) {
	out, code, err := c.runProbe(ctx, ep, []string{"sh", "-c", "command -v bash; command -v sh"}, connectTimeout)
	if err == nil && code == 0 {
		// sh ran (so it exists); its output names whichever shells were found.
		return strings.Contains(out, "bash"), true
	}
	_, bc, be := c.runProbe(ctx, ep, []string{"bash", "-c", "exit 0"}, connectTimeout)
	return be == nil && bc == 0, false
}

// runProbe runs cmd non-interactively in the container and returns its stdout,
// exit code, and any transport/exec error.
func (c *Client) runProbe(ctx context.Context, ep resolve.Endpoint, cmd []string, connectTimeout time.Duration) (string, int, error) {
	var buf strings.Builder
	code, err := c.RunNonInteractive(ctx, ep, cmd, connectTimeout+5*time.Second, &buf, io.Discard)
	return buf.String(), code, err
}

// RunNonInteractive runs one command in the container with no TTY and no
// stdin, and returns its exit code. timeout bounds the whole run, connect
// included. The MCP server's exec runs through here, so a command an
// assistant runs takes exactly the path a shell probe does — same connection,
// same agent RPC, same policy on the agent's side.
func (c *Client) RunNonInteractive(ctx context.Context, ep resolve.Endpoint, cmd []string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := c.Conn(pctx, ep.DialHost)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	stream, err := conn.Agent().Exec(pctx)
	if err != nil {
		conn.done(err)
		return 0, err
	}
	return session.Run(pctx, stream, session.Options{
		Start:  &pb.StartExec{ContainerId: ep.ContainerID, Cmd: cmd},
		Stdout: stdout,
		Stderr: stderr,
	})
}

// ListContainers lists the containers on host, optionally only those of one
// service. ctx bounds the whole call, connect included. The error is returned
// as the agent sent it.
func (c *Client) ListContainers(ctx context.Context, host, serviceFilter string) ([]*pb.ContainerInfo, error) {
	conn, err := c.Conn(ctx, host)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := conn.Agent().ListContainers(ctx, &pb.ListRequest{ServiceFilter: serviceFilter})
	conn.done(err)
	if err != nil {
		return nil, err
	}
	return resp.GetContainers(), nil
}

// containerCallTimeout bounds one kill or inspect beyond the connect: a single
// Engine call on the node.
const containerCallTimeout = 15 * time.Second

// KillContainer asks the container's node agent to SIGKILL it; Swarm then
// schedules a replacement task (CONTRACT.md §3.10). The agent refuses anything
// that is not a swarm task. An operator's explicit action, so it is never
// gated: an agent too old for it answers every time, explained as
// agenterr.ErrAgentTooOld.
func (c *Client) KillContainer(ctx context.Context, ep resolve.Endpoint, connectTimeout time.Duration) error {
	_, err := c.containerCall(ctx, ep, connectTimeout, func(ctx context.Context, a pb.AgentClient) ([]byte, error) {
		_, err := a.KillContainer(ctx, &pb.KillContainerRequest{ContainerId: ep.ContainerID})
		return nil, err
	})
	return err
}

// InspectContainer returns the node-local `docker container inspect` JSON of
// one swarm task container (CONTRACT.md §3.10). The document carries the
// container's environment: callers must not log it. Not gated, like
// KillContainer: the operator asked for it.
func (c *Client) InspectContainer(ctx context.Context, ep resolve.Endpoint, connectTimeout time.Duration) ([]byte, error) {
	return c.containerCall(ctx, ep, connectTimeout, func(ctx context.Context, a pb.AgentClient) ([]byte, error) {
		resp, err := a.InspectContainer(ctx, &pb.InspectContainerRequest{ContainerId: ep.ContainerID})
		return resp.GetInspectJson(), err
	})
}

// containerCall runs one unary call against ep's agent, bounded by the connect
// timeout plus containerCallTimeout, and explains a failure.
func (c *Client) containerCall(ctx context.Context, ep resolve.Endpoint, connectTimeout time.Duration, call func(context.Context, pb.AgentClient) ([]byte, error)) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout+containerCallTimeout)
	defer cancel()
	conn, err := c.Conn(cctx, ep.DialHost)
	if err != nil {
		return nil, c.Explain(ctx, err)
	}
	defer conn.Close()
	out, err := call(cctx, conn.Agent())
	conn.done(err)
	if err != nil {
		return nil, c.Explain(ctx, err)
	}
	return out, nil
}

// ContainerEvents streams one container's runtime events to fn until ctx
// ends or the stream does (nil), or it fails. Gated for Unimplemented
// (capgate.go): an agent that predates the RPC is not dialled again on every
// retry of the log view's event side channel.
func (c *Client) ContainerEvents(ctx context.Context, ep resolve.Endpoint, connectTimeout time.Duration, fn func(*pb.ContainerEvent)) error {
	_, err := gated(c, ctx, ep.DialHost, rpcEvents, func() (struct{}, error) {
		return struct{}{}, c.containerEvents(ctx, ep, connectTimeout, fn)
	})
	return err
}

func (c *Client) containerEvents(ctx context.Context, ep resolve.Endpoint, connectTimeout time.Duration, fn func(*pb.ContainerEvent)) error {
	dctx, dcancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := c.Conn(dctx, ep.DialHost)
	dcancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := conn.Agent().WatchContainerEvents(ctx,
		&pb.WatchContainerEventsRequest{ContainerId: ep.ContainerID})
	if err != nil {
		conn.done(err)
		return err
	}
	for {
		ev, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return rerr
		}
		if ev.GetError() != "" {
			return errors.New(ev.GetError())
		}
		fn(ev)
	}
}
