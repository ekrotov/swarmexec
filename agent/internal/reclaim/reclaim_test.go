// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package reclaim

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"swarmexec/internal/pb"
)

var (
	catContainers = pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS
	catImages     = pb.PruneCategory_PRUNE_CATEGORY_IMAGES
	catVolumes    = pb.PruneCategory_PRUNE_CATEGORY_VOLUMES
)

// fakeMeasure returns whatever the test sets, and counts calls and the age
// filter each one used.
type fakeMeasure struct {
	mu     sync.Mutex
	cats   []*pb.ReclaimCategory
	err    error
	calls  int
	untils []time.Duration
}

func (f *fakeMeasure) measure(_ context.Context, until time.Duration) ([]*pb.ReclaimCategory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.untils = append(f.untils, until)
	return f.cats, f.err
}

func (f *fakeMeasure) set(cats []*pb.ReclaimCategory, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cats, f.err = cats, err
}

func (f *fakeMeasure) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeVolumes struct {
	mu        sync.Mutex
	refreshes int
}

func (v *fakeVolumes) Refresh(context.Context) {
	v.mu.Lock()
	v.refreshes++
	v.mu.Unlock()
}

func (v *fakeVolumes) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.refreshes
}

type fixedUntil time.Duration

func (u fixedUntil) ContainersUntil() time.Duration { return time.Duration(u) }

func newTestCache() (*Cache, *fakeMeasure, *fakeVolumes) {
	m := &fakeMeasure{}
	v := &fakeVolumes{}
	return New(m.measure, v, fixedUntil(6*time.Hour), slog.New(slog.DiscardHandler)), m, v
}

func cat(c pb.PruneCategory, bytes int64) *pb.ReclaimCategory {
	return &pb.ReclaimCategory{Category: c, Bytes: bytes, BytesKnown: true}
}

// A category that fails keeps its previous figure, and the error is reported
// beside it; a later clean measurement clears the error. The figures come back
// in prune order whatever order the measurement produced them in.
func TestMeasureKeepsPreviousFigureOnError(t *testing.T) {
	c, m, _ := newTestCache()
	m.set([]*pb.ReclaimCategory{cat(catVolumes, 3), cat(catImages, 2), cat(catContainers, 1)}, nil)
	c.Measure(context.Background(), false)

	m.set([]*pb.ReclaimCategory{cat(catContainers, 10)}, errors.New("disk usage: daemon busy"))
	c.Measure(context.Background(), false)
	cats, at, errMsg, until := c.Snapshot()
	if errMsg != "disk usage: daemon busy" || at.IsZero() || until != 6*time.Hour {
		t.Errorf("snapshot: err %q at %v until %v", errMsg, at, until)
	}
	want := []int64{10, 2, 3} // containers fresh, images and volumes kept
	if len(cats) != len(want) {
		t.Fatalf("cats = %v", cats)
	}
	for i, w := range want {
		if cats[i].GetBytes() != w {
			t.Errorf("cats[%d] = %v, want %d bytes", i, cats[i], w)
		}
	}
	if cats[0].GetCategory() != catContainers || cats[2].GetCategory() != catVolumes {
		t.Errorf("order = %v, want prune order", cats)
	}

	m.set([]*pb.ReclaimCategory{cat(catContainers, 0)}, nil)
	c.Measure(context.Background(), false)
	if _, _, errMsg, _ := c.Snapshot(); errMsg != "" {
		t.Errorf("a clean measurement must clear the error, got %q", errMsg)
	}
}

// Only a forced measurement rescans the volumes: the hourly one uses what the
// size cache already has.
func TestMeasureRefreshesVolumesOnlyWhenForced(t *testing.T) {
	c, _, v := newTestCache()
	c.Measure(context.Background(), false)
	if v.count() != 0 {
		t.Errorf("an unforced measurement refreshed the volumes")
	}
	c.Measure(context.Background(), true)
	if v.count() != 1 {
		t.Errorf("a forced measurement must refresh the volumes once, got %d", v.count())
	}
}

// measure_now is rate-limited by the clock, not by a count.
func TestMeasureNowThrottles(t *testing.T) {
	c, m, _ := newTestCache()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }

	if !c.MeasureNow(context.Background()) {
		t.Fatal("the first measure_now must measure")
	}
	now = now.Add(measureNowEvery - time.Second)
	if c.MeasureNow(context.Background()) {
		t.Error("a second measure_now inside the limit must be throttled")
	}
	now = now.Add(2 * time.Second)
	if !c.MeasureNow(context.Background()) {
		t.Error("measure_now after the limit must measure again")
	}
	if m.count() != 2 {
		t.Errorf("%d measurements, want 2", m.count())
	}
}

// Run measures after the first delay, then again when kicked — a kick is a
// forced measurement — and returns when its context ends.
func TestRunMeasuresOnTimerAndKick(t *testing.T) {
	c, m, v := newTestCache()
	c.firstDelay = time.Millisecond
	c.interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()

	waitUntil(t, "the first measurement", func() bool { return m.count() == 1 })
	if v.count() != 0 {
		t.Error("the scheduled measurement must not force a volume rescan")
	}
	c.Kick()
	c.Kick() // a second kick while one is pending coalesces
	waitUntil(t, "the kicked measurement", func() bool { return m.count() >= 2 })
	if v.count() < 1 {
		t.Error("a kicked measurement must rescan the volumes")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

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
