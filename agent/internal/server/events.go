// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// healthPrefix is how Docker spells a health transition: the action is
// "health_status: healthy", not a separate field.
const healthPrefix = "health_status: "

// WatchContainerEvents streams one container's runtime events (CONTRACT.md §3.5).
//
// The manager cannot answer these. A task reads "running" while its container
// fails every probe, is OOM-killed, or exits and is restarted — the daemon on
// the node is the only place that knows, and knows it immediately.
//
// Scoped to a single container on purpose. The client opens this over the
// connection it already holds for the view the operator has open, so there is
// no fan-out across nodes, and nothing about the node's other containers
// crosses the wire. A node-wide stream would also expose non-swarm containers
// running on that host, which no part of this tool has any business reporting.
func (s *Server) WatchContainerEvents(req *pb.WatchContainerEventsRequest, stream pb.Agent_WatchContainerEventsServer) error {
	if s.draining.Load() {
		return status.Error(codes.Unavailable, "agent is shutting down; not accepting new watches")
	}
	if req.GetContainerId() == "" {
		return status.Error(codes.InvalidArgument, "WatchContainerEventsRequest requires container_id")
	}

	// A slot before anything else. This is a long-lived stream held open for as
	// long as a view is open, so it belongs under the same cap as exec, logs and
	// port-forward rather than being a quiet way around it.
	release, err := s.streams.acquire()
	if err != nil {
		s.log.Warn("container event watch refused: stream limit reached", "in_use", s.streams.inUse())
		return err
	}
	defer release()

	ctx := stream.Context()
	identity, service, err := s.authorize(ctx, auth.Request{
		Action:      "container.events",
		ContainerID: req.GetContainerId(),
	})
	if err != nil {
		return err
	}

	start := time.Now()
	s.audit.EventWatchStart(identity, req.GetContainerId(), service, peerAddr(ctx))
	var sent int64
	defer func() { s.audit.EventWatchEnd(identity, req.GetContainerId(), time.Since(start), sent) }()

	// Filtered at the daemon, not here: an agent that received every container's
	// events and discarded most of them would be doing the filtering in the one
	// place where a mistake leaks.
	sub := s.docker.Events(ctx, client.EventsListOptions{
		Filters: make(client.Filters).
			Add("type", string(events.ContainerEventType)).
			Add("container", req.GetContainerId()),
	})
	msgs, errs := sub.Messages, sub.Err

	for {
		select {
		case <-ctx.Done():
			return nil // the client closed the view; a clean end, not a failure

		case err := <-errs:
			if err == nil || ctx.Err() != nil {
				return nil
			}
			s.log.Warn("container event stream ended",
				"container_id", req.GetContainerId(), "err", err)
			// Reported in-band so the client can say why the live indicator
			// stopped, rather than leaving it silently frozen.
			_ = stream.Send(&pb.ContainerEvent{Error: err.Error()})
			return status.Error(codes.Internal, err.Error())

		case ev, ok := <-msgs:
			if !ok {
				return nil
			}
			if err := stream.Send(containerEvent(ev)); err != nil {
				return err
			}
			sent++
		}
	}
}

// containerEvent narrows a Docker event to the fields the client uses.
//
// Everything else is dropped, including Actor.Attributes — Docker attaches the
// container's whole label set to every event, and labels are operator-supplied
// strings that routinely carry things nobody meant to stream.
func containerEvent(ev events.Message) *pb.ContainerEvent {
	out := &pb.ContainerEvent{
		Action:       string(ev.Action),
		TimeUnixNano: ev.TimeNano,
	}
	if h, ok := strings.CutPrefix(string(ev.Action), healthPrefix); ok {
		out.Health = h
	}
	// Docker reports the exit code as a string attribute on die/stop.
	if code, ok := ev.Actor.Attributes["exitCode"]; ok {
		out.ExitCode = parseExitCode(code)
	}
	return out
}

// parseExitCode is deliberately lenient: an unparseable code must not cost the
// event itself, which still tells the operator the container died.
func parseExitCode(s string) int32 {
	var n int32
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int32(r-'0')
		if n > 255 {
			return 0
		}
	}
	return n
}
