// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"strings"

	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// ListVolumes lists the volumes on this node. Swarm volumes are node-local, so
// the cli queries every node and aggregates. With req.WithSize it also computes
// each volume's on-disk size via the docker disk-usage endpoint (du-style; can
// be slow), which is why it's opt-in.
func (s *Server) ListVolumes(ctx context.Context, req *pb.ListVolumesRequest) (*pb.ListVolumesResponse, error) {
	identity, err := s.identityFn(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
	}
	decision := s.authz.Authorize(ctx, auth.Request{Action: "volume.list", Identity: identity})
	s.audit.AuthDecision(identity, "", "", decision.Allow, decision.Reason)
	if !decision.Allow {
		s.metrics.AuthDenied()
		return nil, status.Errorf(codes.PermissionDenied, "authorization denied: %s", decision.Reason)
	}

	resp, err := s.docker.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		s.log.Error("VolumeList failed", "err", err)
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}

	// Sizes come from the background cache (no per-request du scan). ready is
	// false only briefly after startup, before the first scan completes; then a
	// volume shows SizeKnown=false and the cli renders "…" until the next list.
	var (
		sizes map[string]int64
		ready bool
	)
	if req.GetWithSize() {
		sizes, _, ready = s.sizeCache.get()
	}

	out := &pb.ListVolumesResponse{}
	for _, v := range resp.Items {
		size, known := int64(-1), false
		if ready {
			if sz, ok := sizes[v.Name]; ok {
				size, known = sz, true
			}
		}
		out.Volumes = append(out.Volumes, &pb.VolumeInfo{
			Name:       v.Name,
			Driver:     v.Driver,
			Mountpoint: v.Mountpoint,
			CreatedAt:  v.CreatedAt,
			Scope:      v.Scope,
			SizeBytes:  size,
			SizeKnown:  known,
			Labels:     v.Labels,
		})
	}
	return out, nil
}

// RemoveVolume removes a volume on this node. It is authorized and audited; an
// in-use volume yields FailedPrecondition.
func (s *Server) RemoveVolume(ctx context.Context, req *pb.RemoveVolumeRequest) (*pb.RemoveVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "RemoveVolumeRequest requires name")
	}
	identity, err := s.identityFn(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
	}
	decision := s.authz.Authorize(ctx, auth.Request{
		Action:   "volume.remove",
		Identity: identity,
		Volume:   req.GetName(),
	})
	s.audit.AuthDecision(identity, "", "", decision.Allow, decision.Reason)
	if !decision.Allow {
		s.metrics.AuthDenied()
		return nil, status.Errorf(codes.PermissionDenied, "authorization denied: %s", decision.Reason)
	}

	_, err = s.docker.VolumeRemove(ctx, req.GetName(), client.VolumeRemoveOptions{Force: req.GetForce()})
	if err != nil {
		s.audit.VolumeRemove(identity, req.GetName(), false, err.Error())
		if isVolumeInUse(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s is in use", req.GetName())
		}
		return nil, status.Errorf(codes.Internal, "remove volume %s: %v", req.GetName(), err)
	}
	s.audit.VolumeRemove(identity, req.GetName(), true, "")
	return &pb.RemoveVolumeResponse{}, nil
}

// CreateVolume creates a volume on this node. It is authorized and audited.
// Volumes are node-local, so the cli targets a specific node's agent.
func (s *Server) CreateVolume(ctx context.Context, req *pb.CreateVolumeRequest) (*pb.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "CreateVolumeRequest requires name")
	}
	identity, err := s.identityFn(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
	}
	decision := s.authz.Authorize(ctx, auth.Request{
		Action:   "volume.create",
		Identity: identity,
		Volume:   req.GetName(),
	})
	s.audit.AuthDecision(identity, "", "", decision.Allow, decision.Reason)
	if !decision.Allow {
		s.metrics.AuthDenied()
		return nil, status.Errorf(codes.PermissionDenied, "authorization denied: %s", decision.Reason)
	}

	created, err := s.docker.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:       req.GetName(),
		Driver:     req.GetDriver(),
		Labels:     req.GetLabels(),
		DriverOpts: req.GetDriverOpts(),
	})
	if err != nil {
		s.audit.VolumeCreate(identity, req.GetName(), false, err.Error())
		return nil, status.Errorf(codes.Internal, "create volume %s: %v", req.GetName(), err)
	}
	s.audit.VolumeCreate(identity, req.GetName(), true, "")
	v := created.Volume
	return &pb.CreateVolumeResponse{Volume: &pb.VolumeInfo{
		Name:       v.Name,
		Driver:     v.Driver,
		Mountpoint: v.Mountpoint,
		CreatedAt:  v.CreatedAt,
		Scope:      v.Scope,
		Labels:     v.Labels,
	}}, nil
}

// isVolumeInUse reports whether the error indicates the volume is still in use.
func isVolumeInUse(err error) bool {
	return strings.Contains(err.Error(), "in use")
}
