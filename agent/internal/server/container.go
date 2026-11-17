// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"

	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// Calls about one swarm task container on this node that the manager cannot
// make: it can neither signal a container on another node nor see its runtime
// state. CONTRACT.md §3.10.
//
// Both refuse containers that are not swarm tasks. The agent mounts the Docker
// socket of a host that may run anything else besides the swarm — a node's
// monitoring, a CI runner — and nothing in this tool has any business stopping
// those or handing out their environment.

// KillContainer sends SIGKILL to one swarm task container. Swarm sees the task
// fail and schedules a replacement per the service's restart policy; the agent
// never restarts anything itself. Authorized as "kill" and audited, refusal
// included.
func (s *Server) KillContainer(ctx context.Context, req *pb.KillContainerRequest) (*pb.KillContainerResponse, error) {
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "KillContainerRequest requires container_id")
	}
	identity, service, err := s.authorize(ctx, auth.Request{Action: "kill", ContainerID: id})
	if err != nil {
		return nil, err
	}
	res, err := s.swarmTask(ctx, id, "kill")
	if err == nil {
		_, kerr := s.docker.ContainerKill(ctx, orID(res.Container.ID, id), client.ContainerKillOptions{Signal: "KILL"})
		err = dockerStatus(kerr, "kill container")
	}
	if err != nil {
		s.audit.ContainerKill(identity, id, service, false, status.Convert(err).Message())
		return nil, err
	}
	s.audit.ContainerKill(identity, id, service, true, "")
	return &pb.KillContainerResponse{}, nil
}

// InspectContainer returns the Engine's inspect document of one swarm task
// container, verbatim. Authorized as "container.inspect" and audited; the
// document is never logged (it carries the container's environment).
func (s *Server) InspectContainer(ctx context.Context, req *pb.InspectContainerRequest) (*pb.InspectContainerResponse, error) {
	id := req.GetContainerId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "InspectContainerRequest requires container_id")
	}
	identity, service, err := s.authorize(ctx, auth.Request{Action: "container.inspect", ContainerID: id})
	if err != nil {
		return nil, err
	}
	res, err := s.swarmTask(ctx, id, "inspect")
	var raw []byte
	if err == nil {
		raw = res.Raw
		if len(raw) == 0 {
			// The Engine's own bytes when the client kept them; otherwise the
			// decoded document, which carries the same fields.
			raw, err = json.Marshal(res.Container)
			if err != nil {
				err = status.Errorf(codes.Internal, "encode container inspect: %v", err)
			}
		}
	}
	if err != nil {
		s.audit.ContainerInspect(identity, id, service, false, status.Convert(err).Message())
		return nil, err
	}
	s.audit.ContainerInspect(identity, id, service, true, "")
	return &pb.InspectContainerResponse{InspectJson: raw}, nil
}

// swarmTask inspects container id and refuses, as FailedPrecondition, one that
// is not a swarm task. verb names the action for the messages ("kill").
func (s *Server) swarmTask(ctx context.Context, id, verb string) (client.ContainerInspectResult, error) {
	res, err := s.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return res, dockerStatus(err, "inspect container")
	}
	if cfg := res.Container.Config; cfg == nil || cfg.Labels[swarmTaskLabel] == "" {
		return res, status.Errorf(codes.FailedPrecondition,
			"container %s is not a swarm task; the agent will only %s swarm task containers", id, verb)
	}
	return res, nil
}

// orID is the container's full id from its inspect, or the requested one when
// the inspect did not carry it.
func orID(full, requested string) string {
	if full != "" {
		return full
	}
	return requested
}
