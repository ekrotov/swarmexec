// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package reclaim is the agent's background reclaim measurement: what each
// prune category would free on this node, measured shortly after start, then
// hourly and after every prune, so ReclaimStats answers from memory.
//
// What a measurement looks at — the stopped containers a prune would take,
// the disk-usage scan, the guarded network selection, the volume refcounts —
// is the prune code's selection, and stays with it in the server; this
// package owns when to measure and what to keep when a measurement fails.
package reclaim

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"swarmexec/agent/internal/prunecat"
	"swarmexec/internal/pb"
)

const (
	// interval is how often the agent measures what a prune would free.
	// Hourly, not on demand: the measurement lists every stopped container with
	// its size and inspects each, and runs a disk-usage scan — affordable once
	// an hour, not on every refresh of a view.
	interval = time.Hour

	// firstDelay leaves the agent's startup (and a rolling update of the
	// whole fleet) alone before the first measurement.
	firstDelay = 30 * time.Second

	// measureNowEvery rate-limits measure_now. A client that asks more
	// often gets the figures it already has, marked throttled.
	measureNowEvery = 30 * time.Second

	// measureTimeout bounds one background measurement.
	measureTimeout = 3 * time.Minute
)

// MeasureFunc takes one measurement of every category, with until as the
// stopped-containers age filter. A category that could not be measured is
// left out of the result and named in the error.
type MeasureFunc func(ctx context.Context, until time.Duration) ([]*pb.ReclaimCategory, error)

// VolumeRefresher rescans the volume sizes and refcounts. A forced
// measurement calls it first: the refcounts are only as fresh as the last
// scan, and removing a container emits no volume event.
type VolumeRefresher interface {
	Refresh(ctx context.Context)
}

// UntilSource supplies the containers age filter a measurement uses — the
// prune schedule's, so the figure an operator reads is what the schedule
// will take.
type UntilSource interface {
	ContainersUntil() time.Duration
}

// Cache holds the last measurement of what each prune category would free.
type Cache struct {
	measure    MeasureFunc
	volumes    VolumeRefresher
	until      UntilSource
	log        *slog.Logger
	now        func() time.Time
	interval   time.Duration
	firstDelay time.Duration

	measureMu sync.Mutex // one measurement at a time

	mu         sync.RWMutex
	cats       []*pb.ReclaimCategory
	measuredAt time.Time
	lastErr    string
	lastUntil  time.Duration // the containers age filter the figures used
	lastForced time.Time

	kickCh chan struct{}
}

// New returns an empty cache. Run starts it.
func New(measure MeasureFunc, volumes VolumeRefresher, until UntilSource, log *slog.Logger) *Cache {
	return &Cache{
		measure: measure, volumes: volumes, until: until, log: log,
		now: time.Now, interval: interval, firstDelay: firstDelay,
		kickCh: make(chan struct{}, 1),
	}
}

// Kick asks for a measurement soon, outside the rate limit: after a prune the
// cached figures are known to be wrong.
func (r *Cache) Kick() {
	select {
	case r.kickCh <- struct{}{}:
	default:
	}
}

// Run measures shortly after start, then hourly and whenever kicked, until
// ctx ends.
func (r *Cache) Run(ctx context.Context) {
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

func (r *Cache) measureBounded(ctx context.Context, refreshVolumes bool) {
	mctx, cancel := context.WithTimeout(ctx, measureTimeout)
	defer cancel()
	r.Measure(mctx, refreshVolumes)
}

// MeasureNow is measure_now: a fresh measurement unless one was forced within
// measureNowEvery, in which case it reports false and changes nothing.
func (r *Cache) MeasureNow(ctx context.Context) bool {
	r.mu.Lock()
	if !r.lastForced.IsZero() && r.now().Sub(r.lastForced) < measureNowEvery {
		r.mu.Unlock()
		return false
	}
	r.lastForced = r.now()
	r.mu.Unlock()
	r.Measure(ctx, true)
	return true
}

// Measure takes one measurement and swaps it in. A category that fails keeps
// its previous figure — an old number with an error beside it beats a blank —
// and the error is reported with the response.
func (r *Cache) Measure(ctx context.Context, refreshVolumes bool) {
	r.measureMu.Lock()
	defer r.measureMu.Unlock()
	if refreshVolumes {
		// The volume refcounts are only as fresh as the size cache's last scan,
		// and removing a container emits no volume event.
		r.volumes.Refresh(ctx)
	}
	until := r.until.ContainersUntil()
	cats, err := r.measure(ctx, until)

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
	for _, pc := range prunecat.Order {
		if !got[pc.Cat] && prev[pc.Cat] != nil {
			cats = append(cats, prev[pc.Cat])
		}
	}
	r.cats = prunecat.SortReclaim(cats)
	r.measuredAt = r.now()
	r.lastUntil = until
	r.lastErr = ""
	if err != nil {
		r.lastErr = err.Error()
		r.log.Warn("reclaim measurement incomplete", "err", err)
	}
}

// Snapshot returns the last measurement: the figures in prunecat order, when
// they were taken (zero before the first), the error of that measurement ("" if
// none) and the containers age filter it used.
func (r *Cache) Snapshot() (cats []*pb.ReclaimCategory, at time.Time, errMsg string, until time.Duration) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cats, r.measuredAt, r.lastErr, r.lastUntil
}
