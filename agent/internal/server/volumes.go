package server

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// ListVolumes lists the volumes on this node. Swarm volumes are node-local, so
// the cli queries every node and aggregates.
func (s *Server) ListVolumes(ctx context.Context, _ *pb.ListVolumesRequest) (*pb.ListVolumesResponse, error) {
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

	resp, err := s.docker.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		s.log.Error("VolumeList failed", "err", err)
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}
	out := &pb.ListVolumesResponse{}
	for _, v := range resp.Volumes {
		if v == nil {
			continue
		}
		out.Volumes = append(out.Volumes, &pb.VolumeInfo{
			Name:       v.Name,
			Driver:     v.Driver,
			Mountpoint: v.Mountpoint,
			CreatedAt:  v.CreatedAt,
			Scope:      v.Scope,
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

	err = s.docker.VolumeRemove(ctx, req.GetName(), req.GetForce())
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

// isVolumeInUse reports whether the error indicates the volume is still in use.
func isVolumeInUse(err error) bool {
	return strings.Contains(err.Error(), "in use")
}
