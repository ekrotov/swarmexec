// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/moby/moby/client"

	"swarmexec/agent/internal/auth"
	"swarmexec/agent/internal/prunecat"
	"swarmexec/internal/pb"
)

// measureReclaim measures what each category would free, node-wide. Every
// category is attempted; the error joins whatever failed.
func (s *Server) measureReclaim(ctx context.Context, until time.Duration) ([]*pb.ReclaimCategory, error) {
	var out []*pb.ReclaimCategory
	var errs []error

	// Containers: exactly the selection a prune would make, so the figure is
	// what the button frees and the held-back count is what it keeps.
	if cs, err := s.stoppedContainers(ctx, nil); err != nil {
		errs = append(errs, err)
	} else {
		prune, keep := splitByAge(cs, until, time.Now())
		c := &pb.ReclaimCategory{Category: pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS, BytesKnown: true, HeldBack: int64(len(keep))}
		for _, sc := range prune {
			c.Count++
			c.Bytes += sc.size
		}
		out = append(out, c)
	}

	// Images and build cache: one disk-usage scan. The image figures are the
	// same byte-exact arithmetic ListImages reports (imagesResponse).
	if du, err := s.docker.DiskUsage(ctx, client.DiskUsageOptions{Images: true, BuildCache: true, Verbose: true}); err != nil {
		errs = append(errs, fmt.Errorf("disk usage: %w", err))
	} else {
		imgs := imagesResponse(du.Images)
		all := &pb.ReclaimCategory{Category: pb.PruneCategory_PRUNE_CATEGORY_IMAGES, BytesKnown: true,
			Bytes: imgs.GetDanglingBytes() + imgs.GetUnusedBytes()}
		dangling := &pb.ReclaimCategory{Category: pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES, BytesKnown: true,
			Bytes: imgs.GetDanglingBytes()}
		for _, im := range imgs.GetImages() {
			if im.GetInUse() {
				continue
			}
			all.Count++
			if im.GetDangling() {
				dangling.Count++
			}
		}
		bc := du.BuildCache
		build := &pb.ReclaimCategory{Category: pb.PruneCategory_PRUNE_CATEGORY_BUILD_CACHE, BytesKnown: true,
			Bytes: max(bc.Reclaimable, 0), Count: max(bc.TotalCount-bc.ActiveCount, 0)}
		out = append(out, all, dangling, build)
	}

	// Networks: the guarded selection, so the count never includes an overlay.
	if nets, err := s.prunableNetworks(ctx, nil); err != nil {
		errs = append(errs, err)
	} else {
		out = append(out, &pb.ReclaimCategory{Category: pb.PruneCategory_PRUNE_CATEGORY_NETWORKS, Count: int64(len(nets))})
	}

	// Volumes: from the volume-size cache, whose DiskUsage scan is the
	// expensive one and already runs on its own schedule.
	sizes, refs, _, ready := s.sizeCache.Usage()
	vol := &pb.ReclaimCategory{Category: pb.PruneCategory_PRUNE_CATEGORY_VOLUMES}
	if ready {
		vol.BytesKnown = true
		for name, n := range refs {
			if n != 0 {
				continue
			}
			vol.Count++
			if sz := sizes[name]; sz >= 0 {
				vol.Bytes += sz
			} else {
				vol.BytesKnown = false // a non-local driver: its size is not ours to know
			}
		}
	}
	out = append(out, vol)

	return prunecat.SortReclaim(out), errors.Join(errs...)
}

// ReclaimStats reports what a prune would free on this node, plus its prune
// schedule and the last scheduled run.
func (s *Server) ReclaimStats(ctx context.Context, req *pb.ReclaimStatsRequest) (*pb.ReclaimStatsResponse, error) {
	if _, err := s.authorizeUnary(ctx, auth.Request{Action: "reclaim.read"}); err != nil {
		return nil, err
	}

	out := &pb.ReclaimStatsResponse{}
	if req.GetMeasureNow() {
		out.MeasureThrottled = !s.reclaim.MeasureNow(ctx)
	}
	cats, at, errMsg, until := s.reclaim.Snapshot()
	out.Categories = cats
	out.MeasureError = errMsg
	if !at.IsZero() {
		out.MeasuredAt = at.UTC().Format(time.RFC3339)
		out.ContainersUntil = until.String()
	}
	out.Schedule, out.LastRun = s.scheduler.Status(time.Now())
	return out, nil
}
