// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

const (
	// reclaimInterval is how often the agent measures what a prune would free.
	// Hourly, not on demand: the measurement lists every stopped container with
	// its size and inspects each, and runs a disk-usage scan — affordable once
	// an hour, not on every refresh of a view.
	reclaimInterval = time.Hour

	// reclaimFirstDelay leaves the agent's startup (and a rolling update of the
	// whole fleet) alone before the first measurement.
	reclaimFirstDelay = 30 * time.Second

	// reclaimMeasureNowEvery rate-limits measure_now. A client that asks more
	// often gets the figures it already has, marked throttled.
	reclaimMeasureNowEvery = 30 * time.Second

	// reclaimMeasureTimeout bounds one background measurement.
	reclaimMeasureTimeout = 3 * time.Minute
)

// reclaimCache holds the last measurement of what each prune category would
// free, so ReclaimStats answers from memory.
type reclaimCache struct {
	s          *Server
	log        *slog.Logger
	interval   time.Duration
	firstDelay time.Duration

	measureMu sync.Mutex // one measurement at a time

	mu         sync.RWMutex
	cats       []*pb.ReclaimCategory
	measuredAt time.Time
	lastErr    string
	until      time.Duration // the containers age filter the figures used
	lastForced time.Time

	kickCh chan struct{}
}

func newReclaimCache(s *Server, log *slog.Logger) *reclaimCache {
	return &reclaimCache{s: s, log: log, interval: reclaimInterval, firstDelay: reclaimFirstDelay, kickCh: make(chan struct{}, 1)}
}

// kick asks for a measurement soon, outside the rate limit: after a prune the
// cached figures are known to be wrong.
func (r *reclaimCache) kick() {
	select {
	case r.kickCh <- struct{}{}:
	default:
	}
}

// run measures shortly after start, then hourly and whenever kicked.
func (r *reclaimCache) run(ctx context.Context) {
	timer := time.NewTimer(r.firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.measureBounded(ctx, false)
			timer.Reset(r.interval)
		case <-r.kickCh:
			r.measureBounded(ctx, true)
		}
	}
}

func (r *reclaimCache) measureBounded(ctx context.Context, refreshVolumes bool) {
	mctx, cancel := context.WithTimeout(ctx, reclaimMeasureTimeout)
	defer cancel()
	r.measure(mctx, refreshVolumes)
}

// measureNow is measure_now: a fresh measurement unless one was forced within
// reclaimMeasureNowEvery, in which case it reports false and changes nothing.
func (r *reclaimCache) measureNow(ctx context.Context) bool {
	r.mu.Lock()
	if !r.lastForced.IsZero() && time.Since(r.lastForced) < reclaimMeasureNowEvery {
		r.mu.Unlock()
		return false
	}
	r.lastForced = time.Now()
	r.mu.Unlock()
	r.measure(ctx, true)
	return true
}

// measure takes one measurement and swaps it in. A category that fails keeps
// its previous figure — an old number with an error beside it beats a blank —
// and the error is reported with the response.
func (r *reclaimCache) measure(ctx context.Context, refreshVolumes bool) {
	r.measureMu.Lock()
	defer r.measureMu.Unlock()
	if refreshVolumes {
		// The volume refcounts are only as fresh as the size cache's last scan,
		// and removing a container emits no volume event.
		r.s.sizeCache.refresh(ctx)
	}
	until := r.s.scheduler.containersUntil()
	cats, err := r.s.measureReclaim(ctx, until)

	r.mu.Lock()
	defer r.mu.Unlock()
	prev := map[pb.PruneCategory]*pb.ReclaimCategory{}
	for _, c := range r.cats {
		prev[c.GetCategory()] = c
	}
	got := map[pb.PruneCategory]bool{}
	for _, c := range cats {
		got[c.GetCategory()] = true
	}
	for _, pc := range pruneOrder {
		if !got[pc.cat] && prev[pc.cat] != nil {
			cats = append(cats, prev[pc.cat])
		}
	}
	r.cats = sortCategories(cats)
	r.measuredAt = time.Now()
	r.until = until
	r.lastErr = ""
	if err != nil {
		r.lastErr = err.Error()
		r.log.Warn("reclaim measurement incomplete", "err", err)
	}
}

func sortCategories(in []*pb.ReclaimCategory) []*pb.ReclaimCategory {
	var out []*pb.ReclaimCategory
	for _, pc := range pruneOrder {
		for _, c := range in {
			if c.GetCategory() == pc.cat {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func (r *reclaimCache) snapshot() (cats []*pb.ReclaimCategory, at time.Time, errMsg string, until time.Duration) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cats, r.measuredAt, r.lastErr, r.until
}

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
	sizes, refs, _, ready := s.sizeCache.usage()
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

	return sortCategories(out), errors.Join(errs...)
}

// ReclaimStats reports what a prune would free on this node, plus its prune
// schedule and the last scheduled run.
func (s *Server) ReclaimStats(ctx context.Context, req *pb.ReclaimStatsRequest) (*pb.ReclaimStatsResponse, error) {
	identity, err := s.identityFn(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
	}
	decision := s.authz.Authorize(ctx, auth.Request{Action: "reclaim.read", Identity: identity})
	s.audit.AuthDecision(identity, "", "", decision.Allow, decision.Reason)
	if !decision.Allow {
		s.metrics.AuthDenied()
		return nil, status.Errorf(codes.PermissionDenied, "authorization denied: %s", decision.Reason)
	}

	out := &pb.ReclaimStatsResponse{}
	if req.GetMeasureNow() {
		out.MeasureThrottled = !s.reclaim.measureNow(ctx)
	}
	cats, at, errMsg, until := s.reclaim.snapshot()
	out.Categories = cats
	out.MeasureError = errMsg
	if !at.IsZero() {
		out.MeasuredAt = at.UTC().Format(time.RFC3339)
		out.ContainersUntil = until.String()
	}
	out.Schedule, out.LastRun = s.scheduler.status(time.Now())
	return out, nil
}
