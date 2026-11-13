// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	cerrdefs "github.com/containerd/errdefs"
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
	if _, err := s.authorizeUnary(ctx, auth.Request{Action: "volume.list"}); err != nil {
		return nil, err
	}

	resp, err := s.docker.VolumeList(ctx, client.VolumeListOptions{})
	if err != nil {
		s.log.Error("VolumeList failed", "err", err)
		return nil, dockerStatus(err, "list volumes")
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
// in-use volume yields FailedPrecondition (CONTRACT.md §3.8).
func (s *Server) RemoveVolume(ctx context.Context, req *pb.RemoveVolumeRequest) (*pb.RemoveVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "RemoveVolumeRequest requires name")
	}
	identity, err := s.authorizeUnary(ctx, auth.Request{
		Action: "volume.remove",
		Volume: req.GetName(),
	})
	if err != nil {
		return nil, err
	}

	_, err = s.docker.VolumeRemove(ctx, req.GetName(), client.VolumeRemoveOptions{Force: req.GetForce()})
	if err != nil {
		s.audit.VolumeRemove(identity, req.GetName(), false, err.Error())
		// The Engine answers an in-use volume with 409 Conflict, which
		// dockerStatus maps to FailedPrecondition too; the short message is
		// kept because it is what clients have always shown for this case.
		if cerrdefs.IsConflict(err) {
			return nil, status.Errorf(codes.FailedPrecondition, "volume %s is in use", req.GetName())
		}
		return nil, dockerStatus(err, "remove volume "+req.GetName())
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
	identity, err := s.authorizeUnary(ctx, auth.Request{
		Action: "volume.create",
		Volume: req.GetName(),
	})
	if err != nil {
		return nil, err
	}

	created, err := s.docker.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name:       req.GetName(),
		Driver:     req.GetDriver(),
		Labels:     req.GetLabels(),
		DriverOpts: req.GetDriverOpts(),
	})
	if err != nil {
		s.audit.VolumeCreate(identity, req.GetName(), false, err.Error())
		return nil, dockerStatus(err, "create volume "+req.GetName())
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
