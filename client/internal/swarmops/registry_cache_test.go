// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// gateResolver answers :latest lookups once released, counting how many run
// at the same time. fail makes latestDigest an error.
type gateResolver struct {
	release chan struct{}
	cur     atomic.Int32
	peak    atomic.Int32
	calls   atomic.Int32
	fail    atomic.Bool
}

func newGateResolver() *gateResolver { return &gateResolver{release: make(chan struct{})} }

func (g *gateResolver) latestDigest(ctx context.Context, _ string) (string, error) {
	g.calls.Add(1)
	n := g.cur.Add(1)
	defer g.cur.Add(-1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if g.fail.Load() {
		return "", errors.New("registry down")
	}
	return "sha256:new", nil
}
func (g *gateResolver) versionLabel(context.Context, string, string) (string, error) { return "", nil }
func (g *gateResolver) listTags(context.Context, string) ([]string, error)           { return nil, nil }

func ref(i int) string { return fmt.Sprintf("img%d:latest@sha256:old", i) }

func newTestCache(r imageResolver) *RegistryCache {
	c := NewRegistryCache(nil)
	c.resolver = r
	return c
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (c *RegistryCache) size() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.entries) }
func (c *RegistryCache) running() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight
}

// Rendering many services at once starts at most maxFetches lookups; the rest
// are answered with "unknown" and picked up by a later render, not queued.
func TestRegistryCacheBoundsConcurrentLookups(t *testing.T) {
	g := newGateResolver()
	c := newTestCache(g)
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		c.Status(ctx, ref(i))
	}
	waitFor(t, "lookups to start", func() bool { return g.cur.Load() == int32(maxRegistryFetches) })
	if got := c.size(); got != maxRegistryFetches {
		t.Errorf("entries = %d after one render of 40 images, want %d (only started lookups get one)", got, maxRegistryFetches)
	}
	close(g.release)
	waitFor(t, "lookups to finish", func() bool { return c.running() == 0 })
	// The next renders pick up the rest, still within the bound.
	for round := 0; round < 12; round++ {
		for i := 0; i < 40; i++ {
			c.Status(ctx, ref(i))
		}
		waitFor(t, "round to finish", func() bool { return c.running() == 0 })
	}
	if p := g.peak.Load(); p > int32(maxRegistryFetches) {
		t.Errorf("peak concurrent lookups = %d, want <= %d", p, maxRegistryFetches)
	}
	if got := c.size(); got != 40 {
		t.Errorf("entries = %d, want all 40 images checked eventually", got)
	}
}

// The cache never holds more than maxEntries images; the one fetched longest
// ago goes first.
func TestRegistryCacheEvictsOldest(t *testing.T) {
	g := newGateResolver()
	close(g.release)
	c := newTestCache(g)
	c.maxEntries = 3
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		c.statusNow(ctx, ref(i), time.Second)
		time.Sleep(2 * time.Millisecond) // distinct fetch times
	}
	if got := c.size(); got != 3 {
		t.Fatalf("entries = %d, want 3", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, gone := range []int{0, 1} {
		if _, ok := c.entries[ref(gone)]; ok {
			t.Errorf("%s should have been evicted", ref(gone))
		}
	}
	for _, kept := range []int{2, 3, 4} {
		if _, ok := c.entries[ref(kept)]; !ok {
			t.Errorf("%s should have been kept", ref(kept))
		}
	}
}

// A lookup that was cancelled (a cluster switch) says nothing about the
// registry: it must not replace a good cached status with a failure.
func TestRegistryCacheCancelledFetchKeepsGoodEntry(t *testing.T) {
	g := newGateResolver()
	close(g.release)
	c := newTestCache(g)
	good := c.statusNow(context.Background(), ref(1), time.Second)
	if !good.ok {
		t.Fatalf("setup: %+v", good)
	}
	// Expire the entry, then let a refresh run under a context that dies.
	c.mu.Lock()
	c.entries[ref(1)].fetchedAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	g.release = make(chan struct{}) // block the next lookup
	ctx, cancel := context.WithCancel(context.Background())
	c.Status(ctx, ref(1))
	waitFor(t, "refresh to start", func() bool { return c.running() == 1 })
	cancel()
	waitFor(t, "refresh to end", func() bool { return c.running() == 0 })

	c.mu.Lock()
	e := c.entries[ref(1)]
	c.mu.Unlock()
	if e == nil || !e.status.ok || e.inflight {
		t.Fatalf("entry after cancelled refresh = %+v, want the good status and no in-flight marker", e)
	}
}

// A real failure does not blank a good status either; it is retried after the
// short failure TTL, not the long success one.
func TestRegistryCacheFailureKeepsGoodStatusAndRetriesSoon(t *testing.T) {
	g := newGateResolver()
	close(g.release)
	c := newTestCache(g)
	c.statusNow(context.Background(), ref(1), time.Second)
	c.mu.Lock()
	c.entries[ref(1)].fetchedAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()

	g.fail.Store(true)
	c.Status(context.Background(), ref(1))
	waitFor(t, "failing refresh to end", func() bool { return c.running() == 0 && g.calls.Load() == 2 })
	if st := c.Status(context.Background(), ref(1)); !st.ok {
		t.Fatalf("status after a failed refresh = %+v, want the earlier good one", st)
	}
	if g.calls.Load() != 2 {
		t.Errorf("a fresh failure must not be retried immediately (calls = %d)", g.calls.Load())
	}
	c.mu.Lock()
	c.entries[ref(1)].fetchedAt = c.entries[ref(1)].fetchedAt.Add(-c.failTTL - time.Second)
	c.mu.Unlock()
	c.Status(context.Background(), ref(1))
	waitFor(t, "retry after failTTL", func() bool { return g.calls.Load() == 3 })
}
