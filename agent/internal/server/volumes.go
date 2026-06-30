package server

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/volume"
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

	resp, err := s.docker.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		s.log.Error("VolumeList failed", "err", err)
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}

	// sizes maps volume name -> on-disk bytes (-1 = unavailable), only when asked.
	var sizes map[string]int64
	if req.GetWithSize() {
		sizes = s.volumeSizes(ctx)
	}

	out := &pb.ListVolumesResponse{}
	for _, v := range resp.Volumes {
		if v == nil {
			continue
		}
		size := int64(-1)
		if sizes != nil {
			if sz, ok := sizes[v.Name]; ok {
				size = sz
			}
		}
		out.Volumes = append(out.Volumes, &pb.VolumeInfo{
			Name:       v.Name,
			Driver:     v.Driver,
			Mountpoint: v.Mountpoint,
			CreatedAt:  v.CreatedAt,
			Scope:      v.Scope,
			SizeBytes:  size,
			SizeKnown:  sizes != nil,
		})
	}
	return out, nil
}

// volumeSizes returns volume name -> on-disk size in bytes via the docker
// disk-usage endpoint, scoped to volumes so images/containers/build-cache are
// not walked (that walk dominates the cost on busy nodes). A failure is
// non-fatal: it returns nil so the listing still works (sizes show as "-").
//
// NB: the daemon computes volume sizes regardless of the type filter. The
// earlier "no sizes with the filter" symptom was actually the scan being
// cancelled when the idle ssh tunnel dropped mid-call; gRPC keepalive fixes
// that, so scoping to volumes is safe and much faster than a full system df.
func (s *Server) volumeSizes(ctx context.Context) map[string]int64 {
	du, err := s.docker.DiskUsage(ctx, types.DiskUsageOptions{Types: []types.DiskUsageObject{types.VolumeObject}})
	if err != nil {
		s.log.Warn("DiskUsage failed; volume sizes unavailable", "err", err)
		return nil
	}
	sizes := make(map[string]int64, len(du.Volumes))
	for _, v := range du.Volumes {
		if v == nil || v.UsageData == nil {
			continue
		}
		sizes[v.Name] = v.UsageData.Size // docker reports -1 for non-local drivers
	}
	return sizes
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
