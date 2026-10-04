// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestVolumeSizeCache_RefreshAndGet(t *testing.T) {
	d := newFakeDocker()
	d.volumes = []volume.Volume{
		{Name: "a", UsageData: &volume.UsageData{Size: 100}},
		{Name: "b", UsageData: &volume.UsageData{Size: 200}},
	}
	c := newVolumeSizeCache(d, quietLogger(), 0)

	// Before any scan the cache is not ready (the cli then renders "…").
	if _, _, ready := c.get(); ready {
		t.Fatal("cache should not be ready before the first refresh")
	}

	c.refresh(context.Background())
	sizes, at, ready := c.get()
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
	d.diskUsageErr = errors.New("boom")
	c.refresh(context.Background())
	if s2, _, ready := c.get(); !ready || s2["a"] != 100 {
		t.Errorf("failed refresh should keep the previous cache, got ready=%v a=%d", ready, s2["a"])
	}
}

// The scan must ask for volumes in verbose mode: on API >= 1.52 a non-verbose
// DiskUsage carries totals only, and every volume would silently lose its size.
// The subscription must be narrowed to volume events at the daemon.
func TestVolumeSizeCache_AsksForVerboseVolumesAndVolumeEvents(t *testing.T) {
	d := newFakeDocker()
	c := newVolumeSizeCache(d, quietLogger(), 0)
	c.refresh(context.Background())

	d.mu.Lock()
	opts := append([]client.DiskUsageOptions(nil), d.diskUsageOpts...)
	d.mu.Unlock()
	if len(opts) != 1 {
		t.Fatalf("DiskUsage calls = %d, want 1", len(opts))
	}
	if want := (client.DiskUsageOptions{Volumes: true, Verbose: true}); opts[0] != want {
		t.Errorf("DiskUsage options = %+v, want %+v", opts[0], want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.run(ctx); close(done) }()
	waitUntil(t, "the subscription", func() bool { return len(filterValues(d.eventFilters(), "type")) > 0 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	f := d.eventFilters()
	if got := filterValues(f, "type"); len(got) != 1 || got[0] != "volume" {
		t.Errorf("type filter = %v, want [volume]", got)
	}
	if len(f) != 1 {
		t.Errorf("filters = %v, want only type=volume", f)
	}
}

func TestVolumeSizeCache_DefaultInterval(t *testing.T) {
	c := newVolumeSizeCache(newFakeDocker(), quietLogger(), 0)
	if c.interval != volumeSizeInterval {
		t.Errorf("interval = %v, want default %v", c.interval, volumeSizeInterval)
	}
}
