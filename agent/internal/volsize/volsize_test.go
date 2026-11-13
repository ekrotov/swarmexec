// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package volsize

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

// fakeDocker is the two calls the cache makes. DiskUsage mirrors the real
// client on API >= 1.52: per-volume Items only when Verbose is set, so a cache
// that forgot Verbose would see no sizes here either.
type fakeDocker struct {
	mu            sync.Mutex
	volumes       []volume.Volume
	diskUsageErr  error
	diskUsageOpts []client.DiskUsageOptions
	eventOpts     []client.EventsListOptions
	// eventCh, when set, is handed to the next subscription and then cleared,
	// so a test can close it and watch the cache subscribe again.
	eventCh chan events.Message
}

func (f *fakeDocker) DiskUsage(_ context.Context, opts client.DiskUsageOptions) (client.DiskUsageResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diskUsageOpts = append(f.diskUsageOpts, opts)
	if f.diskUsageErr != nil {
		return client.DiskUsageResult{}, f.diskUsageErr
	}
	var r client.DiskUsageResult
	if opts.Volumes {
		r.Volumes.TotalCount = int64(len(f.volumes))
		if opts.Verbose {
			r.Volumes.Items = f.volumes
		}
	}
	return r, nil
}

func (f *fakeDocker) Events(_ context.Context, opts client.EventsListOptions) client.EventsResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventOpts = append(f.eventOpts, opts)
	ch := f.eventCh
	f.eventCh = nil
	if ch == nil {
		ch = make(chan events.Message)
	}
	return client.EventsResult{Messages: ch, Err: make(chan error)}
}

func (f *fakeDocker) subscriptions() []client.EventsListOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.EventsListOptions(nil), f.eventOpts...)
}

func quietLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// runUntilCancelled runs the cache in the background; the returned func
// cancels it and fails the test if Run does not return.
func runUntilCancelled(t *testing.T, c *Cache) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
}

func TestVolumeSizeCache_RefreshAndGet(t *testing.T) {
	d := &fakeDocker{volumes: []volume.Volume{
		{Name: "a", UsageData: &volume.UsageData{Size: 100}},
		{Name: "b", UsageData: &volume.UsageData{Size: 200}},
	}}
	c := New(d, quietLogger(), 0)

	// Before any scan the cache is not ready (the cli then renders "…").
	if _, _, ready := c.Get(); ready {
		t.Fatal("cache should not be ready before the first refresh")
	}

	c.Refresh(context.Background())
	sizes, at, ready := c.Get()
	if !ready {
		t.Fatal("cache should be ready after refresh")
	}
	if at.IsZero() {
		t.Error("computedAt should be set after refresh")
	}
	if sizes["a"] != 100 || sizes["b"] != 200 {
		t.Errorf("sizes = %v, want a=100 b=200", sizes)
	}

	// A failing scan keeps the previous cache rather than blanking it.
	d.mu.Lock()
	d.diskUsageErr = errors.New("boom")
	d.mu.Unlock()
	c.Refresh(context.Background())
	if s2, _, ready := c.Get(); !ready || s2["a"] != 100 {
		t.Errorf("failed refresh should keep the previous cache, got ready=%v a=%d", ready, s2["a"])
	}
}

// The scan must ask for volumes in verbose mode: on API >= 1.52 a non-verbose
// DiskUsage carries totals only, and every volume would silently lose its size.
// The subscription must be narrowed to volume events at the daemon.
func TestVolumeSizeCache_AsksForVerboseVolumesAndVolumeEvents(t *testing.T) {
	d := &fakeDocker{}
	c := New(d, quietLogger(), 0)
	c.Refresh(context.Background())

	d.mu.Lock()
	opts := append([]client.DiskUsageOptions(nil), d.diskUsageOpts...)
	d.mu.Unlock()
	if len(opts) != 1 {
		t.Fatalf("DiskUsage calls = %d, want 1", len(opts))
	}
	if want := (client.DiskUsageOptions{Volumes: true, Verbose: true}); opts[0] != want {
		t.Errorf("DiskUsage options = %+v, want %+v", opts[0], want)
	}

	stop := runUntilCancelled(t, c)
	waitUntil(t, "the subscription", func() bool { return len(d.subscriptions()) > 0 })
	stop()
	f := d.subscriptions()[0].Filters
	if len(f) != 1 || len(f["type"]) != 1 || !f["type"]["volume"] {
		t.Errorf("filters = %v, want only type=volume", f)
	}
}

func TestVolumeSizeCache_DefaultInterval(t *testing.T) {
	c := New(&fakeDocker{}, quietLogger(), 0)
	if c.interval != DefaultInterval {
		t.Errorf("interval = %v, want default %v", c.interval, DefaultInterval)
	}
}

// Usage carries the reference counts beside the sizes: a volume nothing
// references is what a volume prune removes, and the reclaim figure is built
// from exactly that. A volume without usage data is left out of both.
func TestVolumeSizeCache_UsageCarriesRefCounts(t *testing.T) {
	d := &fakeDocker{volumes: []volume.Volume{
		{Name: "free", UsageData: &volume.UsageData{Size: 10, RefCount: 0}},
		{Name: "busy", UsageData: &volume.UsageData{Size: 20, RefCount: 2}},
		{Name: "nfs", UsageData: &volume.UsageData{Size: -1, RefCount: 0}},
		{Name: "unknown"},
	}}
	c := New(d, quietLogger(), 0)
	if _, _, _, ready := c.Usage(); ready {
		t.Fatal("Usage must report not-ready before the first scan")
	}
	c.Refresh(context.Background())
	sizes, refs, _, ready := c.Usage()
	if !ready {
		t.Fatal("Usage must be ready after a scan")
	}
	if refs["free"] != 0 || refs["busy"] != 2 || sizes["nfs"] != -1 {
		t.Errorf("sizes = %v, refs = %v", sizes, refs)
	}
	if _, ok := sizes["unknown"]; ok {
		t.Error("a volume without usage data must not get a size")
	}
}

// A daemon that ends the event stream (a restart, a dropped socket) must not
// leave the cache deaf: it subscribes again at once.
func TestVolumeSizeCache_ResubscribesWhenTheStreamCloses(t *testing.T) {
	first := make(chan events.Message)
	d := &fakeDocker{eventCh: first}
	c := New(d, quietLogger(), time.Hour)
	stop := runUntilCancelled(t, c)
	defer stop()

	waitUntil(t, "the first subscription", func() bool { return len(d.subscriptions()) == 1 })
	close(first)
	waitUntil(t, "a second subscription", func() bool { return len(d.subscriptions()) == 2 })
}
