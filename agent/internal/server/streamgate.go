// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/internal/pb"
)

// gatedStream describes one long-lived streaming RPC the stream gate owns.
type gatedStream struct {
	// method is the RPC name, the "method" label of the per-stream metrics.
	method string
	// noun completes "not accepting new …" in the drain refusal. Each RPC has
	// always said it its own way, and the wording is what clients see, so it is
	// kept rather than unified.
	noun string
}

// gatedStreams is every streaming RPC of the Agent service. A streaming RPC
// missing here would bypass the drain and the stream cap, so a test checks the
// table against pb.Agent_ServiceDesc. Unary RPCs (Version among them, see S-02
// and version.go) never reach a stream interceptor.
var gatedStreams = map[string]gatedStream{
	pb.Agent_Exec_FullMethodName:                 {method: "Exec", noun: "sessions"},
	pb.Agent_Logs_FullMethodName:                 {method: "Logs", noun: "requests"},
	pb.Agent_PortForward_FullMethodName:          {method: "PortForward", noun: "forwards"},
	pb.Agent_WatchContainerEvents_FullMethodName: {method: "WatchContainerEvents", noun: "watches"},
}

// StreamInterceptor returns the gate every streaming RPC passes before its
// handler runs. It owns the per-stream bookkeeping the handlers used to repeat,
// each in its own words:
//
//  1. the drain check — refused with Unavailable once StartDrain was called;
//  2. a stream slot (ResourceExhausted beyond -max-streams), taken before the
//     handler's first Recv, which blocks until the client speaks: counting only
//     established streams would leave "open and never speak" uncapped;
//  3. the shutdown drain's accounting (wg and active), for the stream's whole
//     lifetime;
//  4. the metrics: streams started/active per method, and the bytes the handler
//     tallied (see streamTally), reported once when the stream ends.
//
// Authorization stays in the handlers, which resolve the container's service
// first. That puts 3 before it, where the handlers used to count only after
// authorizing. The difference is deliberate: a stream admitted just before a
// drain began, whose first message arrived after, used to run uncounted — the
// drain reported "drained", and the session was cut off by the graceful-stop
// bound without the wait that lets it write its end-of-session audit record.
// The price is that a stream which never sends its first message holds the
// drain until the drain window ends rather than until the graceful-stop bound,
// both well inside Swarm's stop_grace_period. A denied stream is counted only
// for the moment it takes to deny it; denials are audited and counted exactly
// as before, by the handler.
//
// Chain it after the transport-auth interceptors (see main.go): a caller
// without the shared secret must not occupy a slot or hold up a drain.
func (s *Server) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		g, ok := gatedStreams[info.FullMethod]
		if !ok {
			return handler(srv, ss)
		}
		if s.draining.Load() {
			return status.Error(codes.Unavailable, "agent is shutting down; not accepting new "+g.noun)
		}
		release, err := s.streams.Acquire()
		if err != nil {
			s.log.Warn("stream refused: stream limit reached", "method", g.method, "in_use", s.streams.InUse())
			return err
		}
		defer release()
		defer s.trackStream()()

		s.metrics.StreamStarted(g.method)
		tally := &streamTally{}
		defer func() {
			s.metrics.BytesTransferred(tally.in.Load(), tally.out.Load())
			s.metrics.StreamEnded(g.method)
		}()
		return handler(srv, &talliedStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), tallyKey{}, tally)})
	}
}

// streamTally is the byte count of one stream: in = payload received from the
// client and passed on (exec stdin, forwarded bytes), out = payload sent to the
// client (exec and log output, forwarded bytes). The handler counts into it,
// and the same numbers go into its end-of-stream audit record, so the audit
// log and the metrics cannot disagree.
type streamTally struct {
	in, out atomic.Int64
}

type tallyKey struct{}

// tallyFrom returns the stream's tally from its context. Outside the gate (a
// handler called directly, as unit tests do) it returns a fresh one, so the
// handler counts the same way and simply nobody reports the result.
func tallyFrom(ctx context.Context) *streamTally {
	if t, ok := ctx.Value(tallyKey{}).(*streamTally); ok {
		return t
	}
	return &streamTally{}
}

// talliedStream hands the handler a context that carries the tally.
type talliedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (t *talliedStream) Context() context.Context { return t.ctx }
