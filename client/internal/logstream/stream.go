// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/dockerfmt"
	pb "swarmexec/internal/pb"
)

const (
	// logReconnectDelay is how long to wait between attempts to find the
	// replacement container after a followed log stream ends.
	logReconnectDelay = time.Second
	// logReconnectWindow bounds how long to keep waiting for a replacement before
	// giving up (a rolling update normally schedules the new task within seconds).
	logReconnectWindow = 30 * time.Second
)

// Params carries the Logs request options.
type Params struct {
	Follow         bool
	Tail           uint32
	Timestamps     bool
	Since          time.Duration
	ConnectTimeout time.Duration

	// Wake, when non-nil, cuts the wait between attempts to find a replacement
	// container short. The TUI feeds it from the container's own event stream,
	// so a restart is noticed by the node telling us rather than by asking the
	// manager again a second later. Nothing depends on it: with a nil channel,
	// or none arriving, the polling below behaves exactly as before.
	Wake <-chan struct{}

	// ReconnectDelay overrides logReconnectDelay, the pause between attempts
	// to find a replacement container (and before re-attaching to the same
	// one). Zero means the default; tests shorten it.
	ReconnectDelay time.Duration
}

func (p Params) reconnectDelay() time.Duration {
	if p.ReconnectDelay > 0 {
		return p.ReconnectDelay
	}
	return logReconnectDelay
}

// Run is a log stream's plumbing, the same for every container it ends up
// following: the agents to read from, the resolver that finds a replacement
// container, what to ask for, and where the two streams go.
type Run struct {
	Agents         *agentcall.Client
	Resolver       *resolve.Resolver
	Params         Params
	Stdout, Stderr io.Writer
}

// Stream dials the agent and streams the container's logs into the run's
// Stdout/Stderr. It is shared by the `logs` command and the interactive `ui`.
// Returns nil when the stream ends, or when ctx ends — cancelled OR past its
// deadline; both are the caller saying stop.
func Stream(ctx context.Context, run Run, ep resolve.Endpoint) error {
	p, stdout, stderr := run.Params, run.Stdout, run.Stderr
	dctx, dcancel := context.WithTimeout(ctx, p.ConnectTimeout)
	conn, err := run.Agents.Conn(dctx, ep.DialHost)
	dcancel()
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := conn.Agent().Logs(ctx, &pb.LogsRequest{
		ContainerId:  ep.ContainerID,
		Follow:       p.Follow,
		Tail:         p.Tail,
		Timestamps:   p.Timestamps,
		SinceSeconds: uint32(p.Since.Seconds()),
	})
	if err != nil {
		return fmt.Errorf("open logs stream: %w", err)
	}

	for {
		chunk, err := stream.Recv()
		if err != nil {
			// The caller's context ending is an END, not a failure, however
			// it ended. Cancellation was already handled; a DEADLINE was not,
			// and that is the same event from the caller's side — a log view
			// closed, a Ctrl-C, a bounded follow. Checking ctx rather than the
			// error's code is what makes the two alike: the error is only a
			// consequence of the context, and under load it arrives in
			// whichever form the transport chose.
			if ctx.Err() != nil ||
				errors.Is(err, io.EOF) || status.Code(err) == codes.Canceled || errors.Is(err, context.Canceled) {
				return nil
			}
			return agenterr.Readable(err)
		}
		switch pl := chunk.Payload.(type) {
		case *pb.LogChunk_Stdout:
			_, _ = stdout.Write(pl.Stdout)
		case *pb.LogChunk_Stderr:
			_, _ = stderr.Write(pl.Stderr)
		case *pb.LogChunk_Error:
			return fmt.Errorf("agent: %s", pl.Error)
		}
	}
}

// Follow follows a target's logs across container replacements. It
// streams the current container; when the stream ends cleanly while following
// (the container was replaced by a rolling update, restart or reschedule), it
// re-resolves the target's running container and reconnects, emitting a notice
// via notify. It returns when: follow is false and the stream ends; ctx is
// cancelled; a real transport/agent error occurs; or the target has no running
// container left. notify may be nil.
func Follow(ctx context.Context, run Run, t resolve.FollowTarget, ep resolve.Endpoint, notify func(string)) error {
	p := run.Params
	lastCID := ep.ContainerID
	cur := run
	for {
		err := Stream(ctx, cur, ep)
		ended := time.Now()
		if err != nil {
			// The caller stopped us: Ctrl-C, a closed view, an expired
			// deadline. Whatever the stream reported is a consequence of that,
			// not something the operator can act on, so this is an END and not
			// a failure — the same rule the port-forward path applies when the
			// client goes away. Without it a cancelled follow exits non-zero
			// and prints a transport error for a stream nobody was reading.
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !p.Follow || ctx.Err() != nil || t.Service == "" {
			return nil
		}

		// The container ended while following. Wait for its successor — during a
		// rolling update the replacement may still be scheduling.
		c, ok, err := waitForSuccessor(ctx, run.Resolver, t, p.Wake, p.reconnectDelay(), notify)
		if !ok {
			return err
		}
		ep = resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		cur = run
		if c.ContainerID == lastCID {
			// Same container the manager still lists as running (the stream
			// ended, so it most likely just died): resume from where the last
			// stream ended. Tail 0 means "everything" on the wire, so the
			// resume point is a Since, and a pause keeps this from spinning
			// until the manager catches up.
			cur.Params.Tail = 0
			cur.Params.Since = time.Since(ended) + time.Second
			select {
			case <-time.After(p.reconnectDelay()):
			case <-ctx.Done():
				return nil
			}
		} else {
			// A new container: all of its log is unseen.
			cur.Params.Tail, cur.Params.Since = 0, 0
			if notify != nil {
				notify(fmt.Sprintf("── container replaced; reconnected to %s on %s ──", dockerfmt.ShortID(c.ContainerID), dockerfmt.OrDash(c.NodeName)))
			}
		}
		lastCID = c.ContainerID
	}
}

// waitForSuccessor polls for the FollowTarget's replacement container until one
// is running, the service is gone, the wait window elapses, or ctx is cancelled.
// It returns ok=false (with a nil error on a clean stop) when following should
// end; err is non-nil only for an unrecovered resolve error.
func waitForSuccessor(ctx context.Context, r *resolve.Resolver, t resolve.FollowTarget, wake <-chan struct{}, delay time.Duration, notify func(string)) (resolve.Candidate, bool, error) {
	var lastErr error
	for waited := time.Duration(0); ; waited += delay {
		c, ok, err := r.Successor(ctx, t)
		switch {
		case ok:
			return c, true, nil
		case errors.Is(err, resolve.ErrTargetGone):
			if notify != nil {
				notify("── target has no running container left; stopping ──")
			}
			return resolve.Candidate{}, false, nil
		case ctx.Err() != nil:
			return resolve.Candidate{}, false, nil
		}
		lastErr = err // nil while the replacement is merely still scheduling
		if waited >= logReconnectWindow {
			if notify != nil {
				notify("── no replacement container appeared; stopping ──")
			}
			return resolve.Candidate{}, false, lastErr
		}
		select {
		case <-ctx.Done():
			return resolve.Candidate{}, false, nil
		case <-wake:
			// The node reported that the container started, died or restarted.
			// Ask again now instead of sitting out the rest of the delay.
			//
			// The gain is bounded by logReconnectDelay — up to a second, not the
			// "sub-second reconnect" the plan promised, because the wait was
			// already a second. What it does remove is the case that looks
			// worst: a container that restarts in place, where the replacement
			// exists immediately and the view still paused before showing it.
		case <-time.After(delay):
		}
	}
}

// FollowTargetOf derives the logical replica to keep following from the
// raw target argument: "service.slot" pins the slot, a bare name is treated as a
// service. A task/container id also lands here as a Service name that will not
// resolve, so Successor reports the target gone and following simply stops — the
// same outcome as before, just with a friendly notice.
func FollowTargetOf(target string) resolve.FollowTarget {
	target = strings.TrimSpace(target)
	if i := strings.LastIndex(target, "."); i > 0 {
		if slot, err := strconv.Atoi(target[i+1:]); err == nil {
			return resolve.FollowTarget{Service: target[:i], Slot: slot}
		}
	}
	return resolve.FollowTarget{Service: target}
}
