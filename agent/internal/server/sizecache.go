// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
)

// volumeSizeInterval is the periodic full-rescan interval. Volume sizes grow as
// data is written without emitting any docker event, so a timer catches drift
// between the reactive (create/destroy) refreshes.
const volumeSizeInterval = 10 * time.Minute

// volumeSizeCache holds each volume's on-disk size so ListVolumes(with_size)
// answers from memory instead of running a du-style DiskUsage scan per request
// (that scan is what made the SIZE column slow). It is refreshed once at
// startup, reactively on docker volume create/destroy events, and periodically.
type volumeSizeCache struct {
	docker   DockerClient
	log      *slog.Logger
	interval time.Duration

	mu         sync.RWMutex
	sizes      map[string]int64
	computedAt time.Time
	ready      bool
}

func newVolumeSizeCache(docker DockerClient, log *slog.Logger, interval time.Duration) *volumeSizeCache {
	if interval <= 0 {
		interval = volumeSizeInterval
	}
	return &volumeSizeCache{docker: docker, log: log, interval: interval, sizes: map[string]int64{}}
}

// get returns the cached sizes (treat as read-only; refresh swaps the whole
// map), when they were computed, and whether the cache has ever been populated.
func (c *volumeSizeCache) get() (sizes map[string]int64, computedAt time.Time, ready bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sizes, c.computedAt, c.ready
}

// run scans once immediately, then refreshes on volume create/destroy events
// (debounced so a burst like a prune triggers a single rescan) and on the
// periodic ticker, until ctx is cancelled.
func (c *volumeSizeCache) run(ctx context.Context) {
	c.refresh(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	// Debounce reactive refreshes: an event arms a short timer that fires a
	// single refresh, coalescing bursts.
	fire := make(chan struct{}, 1)
	var debounce *time.Timer
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	arm := func() {
		if debounce != nil {
			debounce.Stop()
		}
		debounce = time.AfterFunc(2*time.Second, func() {
			select {
			case fire <- struct{}{}:
			default:
			}
		})
	}

	evCh, errCh := c.subscribe(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		case <-fire:
			c.refresh(ctx)
		case ev, ok := <-evCh:
			if !ok {
				evCh, errCh = c.subscribe(ctx)
				continue
			}
			if ev.Action == "create" || ev.Action == "destroy" {
				arm()
			}
		case err, ok := <-errCh:
			if ok && err != nil {
				c.log.Warn("volume event stream error; re-subscribing", "err", err)
			}
			// Pause briefly, then re-subscribe. The ticker keeps sizes fresh in
			// the meantime, so a flaky event stream never stalls the cache.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			evCh, errCh = c.subscribe(ctx)
		}
	}
}

func (c *volumeSizeCache) subscribe(ctx context.Context) (<-chan events.Message, <-chan error) {
	return c.docker.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(filters.Arg("type", "volume")),
	})
}

// refresh runs one DiskUsage scan and swaps in the new sizes. A failure is
// non-fatal: the previous cache is kept so a transient error does not blank the
// SIZE column.
func (c *volumeSizeCache) refresh(ctx context.Context) {
	du, err := c.docker.DiskUsage(ctx, types.DiskUsageOptions{Types: []types.DiskUsageObject{types.VolumeObject}})
	if err != nil {
		c.log.Warn("volume size scan failed; keeping previous cache", "err", err)
		return
	}
	sizes := make(map[string]int64, len(du.Volumes))
	for _, v := range du.Volumes {
		if v == nil || v.UsageData == nil {
			continue
		}
		sizes[v.Name] = v.UsageData.Size // docker reports -1 for non-local drivers
	}
	c.mu.Lock()
	c.sizes = sizes
	c.computedAt = time.Now()
	c.ready = true
	c.mu.Unlock()
	c.log.Debug("volume sizes refreshed", "count", len(sizes))
}
