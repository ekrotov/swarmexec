// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// Images are node-local and the manager API has no view of them whatsoever —
// unlike volumes, which it at least knows are mounted. A node whose disk has
// filled with old image layers is therefore invisible from the cluster side,
// which is the gap these two RPCs close.

// ListImages reports the images on this node, what each costs on disk, and
// whether a running container is using it.
func (s *Server) ListImages(ctx context.Context, _ *pb.ListImagesRequest) (*pb.ListImagesResponse, error) {
	identity, err := s.identityFn(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
	}
	decision := s.authz.Authorize(ctx, auth.Request{Action: "image.list", Identity: identity})
	s.audit.AuthDecision(identity, "", "", decision.Allow, decision.Reason)
	if !decision.Allow {
		s.metrics.AuthDenied()
		return nil, status.Errorf(codes.PermissionDenied, "authorization denied: %s", decision.Reason)
	}

	// DiskUsage, not ImageList. Summing each image's Size double-counts every
	// layer two images share — measured against a real node it inflated 22 GB of
	// images to 33 GB. DiskUsage is what `docker system df` itself reports from:
	// LayersSize is the true total, and each image carries the SharedSize needed
	// to work out what removing it would ACTUALLY free. It also fills in
	// Containers, so no separate container list is needed.
	du, err := s.docker.DiskUsage(ctx, types.DiskUsageOptions{Types: []types.DiskUsageObject{types.ImageObject}})
	if err != nil {
		s.log.Error("DiskUsage(images) failed", "err", err)
		return nil, status.Errorf(codes.Internal, "list images: %v", err)
	}
	return imagesResponse(du), nil
}

// imagesResponse is the pure shaping half: it decides what counts as dangling,
// what counts as in use, and what the two reclaimable totals are. Split out so
// those rules are testable without a docker daemon — they are what an operator
// is about to make a destructive decision on.
//
// The size arithmetic is the subtle part. An image's Size includes every layer
// it is built from, and layers are shared, so summing sizes over-reports badly.
// What removing ONE image would free is its UNIQUE bytes, Size - SharedSize —
// the same figure `docker system df` puts in its reclaimable column. The total
// on disk comes from LayersSize, which counts each layer once.
func imagesResponse(du types.DiskUsage) *pb.ListImagesResponse {
	out := &pb.ListImagesResponse{TotalBytes: du.LayersSize}

	var inUseUnique, danglingUnique int64
	for _, img := range du.Images {
		if img == nil {
			continue
		}
		// Docker reports an untagged image either with no tags at all or with the
		// explicit "<none>:<none>" placeholder.
		tags := imageTags(*img)
		dangling := len(tags) == 0
		used := img.Containers > 0
		out.Images = append(out.Images, &pb.ImageInfo{
			Id:          img.ID,
			Tags:        tags,
			SizeBytes:   img.Size,
			CreatedUnix: img.Created,
			InUse:       used,
			Dangling:    dangling,
		})
		switch {
		case used:
			inUseUnique += uniqueSize(*img)
		case dangling:
			danglingUnique += uniqueSize(*img)
		}
	}

	// Everything not held exclusively by a running image is reclaimable. This is
	// docker's own formula for `system df`, and it is right where summing the
	// unused images' unique sizes is not: a layer shared by TWO unused images
	// belongs to neither one's unique size, yet pruning frees it. Measured on a
	// real node the naive sum under-reported by 1.3 GB of 23 GB.
	reclaimable := du.LayersSize - inUseUnique
	if reclaimable < 0 {
		reclaimable = 0
	}
	// The dangling share cannot be derived exactly the same way — that would need
	// per-layer ownership — so it stays the sum of the untagged images' unique
	// bytes, which is a LOWER bound for the same reason. The remainder is
	// attributed to the sweep, so the two always add up to the exact total.
	if danglingUnique > reclaimable {
		danglingUnique = reclaimable
	}
	out.DanglingBytes = danglingUnique
	out.UnusedBytes = reclaimable - danglingUnique
	return out
}

// uniqueSize is the bytes only this image holds — what removing it would free.
// Docker reports SharedSize as -1 when it has not computed it; treating that as
// 0 would claim the image's whole size is reclaimable, so it yields nothing
// instead. Under-promising is the right direction for a number an operator uses
// to decide what to delete.
func uniqueSize(img image.Summary) int64 {
	if img.SharedSize < 0 || img.Size < img.SharedSize {
		return 0
	}
	return img.Size - img.SharedSize
}

// imageTags returns an image's real tags, dropping docker's "<none>:<none>"
// placeholder so a dangling image reads as having no tags rather than a fake one.
func imageTags(img image.Summary) []string {
	var out []string
	for _, t := range img.RepoTags {
		if t == "" || t == "<none>:<none>" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// PruneImages reclaims image disk space on this node.
//
// The two modes are deliberately not one flag with a default: dangling-only
// removes untagged leftovers that nothing can start from, while all=true also
// removes tagged images no RUNNING container uses — which on a swarm node
// includes every service scaled to zero or between restarts, each of which then
// has to be pulled again. The audit records which one was asked for.
func (s *Server) PruneImages(ctx context.Context, req *pb.PruneImagesRequest) (*pb.PruneImagesResponse, error) {
	identity, err := s.identityFn(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
	}
	action := "image.prune"
	if req.GetAll() {
		// A separate action string so a policy can allow reclaiming dangling
		// layers without also allowing the destructive sweep.
		action = "image.prune.all"
	}
	decision := s.authz.Authorize(ctx, auth.Request{Action: action, Identity: identity})
	s.audit.AuthDecision(identity, "", "", decision.Allow, decision.Reason)
	if !decision.Allow {
		s.metrics.AuthDenied()
		return nil, status.Errorf(codes.PermissionDenied, "authorization denied: %s", decision.Reason)
	}

	report, err := s.docker.ImagesPrune(ctx, pruneFilters(req.GetAll()))
	if err != nil {
		s.audit.ImagePrune(identity, req.GetAll(), 0, 0, false, err.Error())
		s.log.Error("ImagesPrune failed", "all", req.GetAll(), "err", err)
		return nil, status.Errorf(codes.Internal, "prune images: %v", err)
	}

	out := &pb.PruneImagesResponse{ReclaimedBytes: int64(report.SpaceReclaimed)}
	for _, d := range report.ImagesDeleted {
		switch {
		case d.Deleted != "":
			out.Deleted = append(out.Deleted, d.Deleted)
		case d.Untagged != "":
			out.Deleted = append(out.Deleted, d.Untagged)
		}
	}
	s.audit.ImagePrune(identity, req.GetAll(), out.ReclaimedBytes, len(out.Deleted), true, "")
	return out, nil
}

// pruneFilters builds the docker filter for the requested mode. Docker's own
// convention is inverted and easy to get backwards: `dangling=true` prunes ONLY
// untagged images, `dangling=false` prunes every unused one. Getting this the
// wrong way round would quietly turn the safe choice into the destructive one,
// which is why it is its own function with its own test.
func pruneFilters(all bool) filters.Args {
	if all {
		return filters.NewArgs(filters.Arg("dangling", "false"))
	}
	return filters.NewArgs(filters.Arg("dangling", "true"))
}
