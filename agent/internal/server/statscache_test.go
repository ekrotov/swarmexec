// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
)

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// frame builds a docker stats frame with the counters the arithmetic uses.
func frame(cpuTotal, sysTotal uint64, cpus uint32, memUsage, memLimit uint64, memStats map[string]uint64) container.StatsResponse {
	var s container.StatsResponse
	s.CPUStats.CPUUsage.TotalUsage = cpuTotal
	s.CPUStats.SystemUsage = sysTotal
	s.CPUStats.OnlineCPUs = cpus
	s.MemoryStats.Usage = memUsage
	s.MemoryStats.Limit = memLimit
	s.MemoryStats.Stats = memStats
	return s
}

// cpuPercent must produce the number `docker stats` prints: the share of ONE
// cpu, so a container busy on two of four cores reads 200%.
func TestCPUPercent(t *testing.T) {
	prev := sampleFrom(frame(1_000_000_000, 10_000_000_000, 4, 0, 0, nil))
	cur := sampleFrom(frame(3_000_000_000, 14_000_000_000, 4, 0, 0, nil))
	// cpu delta 2s, system delta 4s, 4 cpus -> 2/4*4*100 = 200%
	got, ok := cpuPercent(prev, cur)
	if !ok {
		t.Fatal("a normal delta should produce a percentage")
	}
	if got < 199.9 || got > 200.1 {
		t.Errorf("cpu = %.2f%%, want 200%%", got)
	}
}

func TestCPUPercentRejectsUnusableDeltas(t *testing.T) {
	base := sampleFrom(frame(5_000_000_000, 20_000_000_000, 2, 0, 0, nil))

	// A restarted container's counters go backwards; reporting a huge negative
	// or wrapped value would be worse than reporting nothing.
	restarted := sampleFrom(frame(1_000_000_000, 21_000_000_000, 2, 0, 0, nil))
	if _, ok := cpuPercent(base, restarted); ok {
		t.Error("counters going backwards must not yield a percentage")
	}

	// Two readings at the same instant have no system delta to divide by.
	same := sampleFrom(frame(6_000_000_000, 20_000_000_000, 2, 0, 0, nil))
	if _, ok := cpuPercent(base, same); ok {
		t.Error("a zero system delta must not yield a percentage")
	}

	// A container pinned to no online CPUs still must not divide by zero.
	p := sampleFrom(frame(1_000_000_000, 10_000_000_000, 0, 0, 0, nil))
	c := sampleFrom(frame(2_000_000_000, 12_000_000_000, 0, 0, 0, nil))
	if _, ok := cpuPercent(p, c); !ok {
		t.Error("an unknown cpu count should fall back to 1, not fail")
	}
}

// Memory must exclude the page cache, or an idle container that has read a lot
// of files looks like it is about to be OOM-killed.
func TestMemoryUsageExcludesPageCache(t *testing.T) {
	cases := []struct {
		name  string
		stats map[string]uint64
		want  int64
	}{
		{"cgroup v2", map[string]uint64{"inactive_file": 400}, 600},
		{"cgroup v1 hierarchy", map[string]uint64{"total_inactive_file": 400}, 600},
		{"cgroup v1", map[string]uint64{"cache": 400}, 600},
		{"no breakdown", nil, 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := memoryUsage(container.MemoryStats{Usage: 1000, Stats: tc.stats})
			if got != tc.want {
				t.Errorf("memoryUsage = %d, want %d", got, tc.want)
			}
		})
	}

	// A cache figure larger than usage must not underflow into a huge number.
	if got := memoryUsage(container.MemoryStats{Usage: 100, Stats: map[string]uint64{"inactive_file": 500}}); got != 0 {
		t.Errorf("memoryUsage = %d, want 0 rather than an underflow", got)
	}
}

// statsFake builds a fake docker with one container, two stats frames and a
// known node capacity.
func statsFake(limitMem int64, nanoCPUs int64) *fakeDocker {
	d := newFakeDocker()
	d.containers = []container.Summary{{ID: "c1"}}
	var insp container.InspectResponse
	insp.HostConfig = &container.HostConfig{}
	insp.HostConfig.Memory = limitMem
	insp.HostConfig.NanoCPUs = nanoCPUs
	d.inspect = map[string]container.InspectResponse{"c1": insp}
	d.statsFrames = map[string][]container.StatsResponse{"c1": {
		frame(1_000_000_000, 10_000_000_000, 4, 1000, 8<<30, map[string]uint64{"inactive_file": 400}),
		frame(3_000_000_000, 14_000_000_000, 4, 1000, 8<<30, map[string]uint64{"inactive_file": 400}),
	}}
	d.info = system.Info{NCPU: 4, MemTotal: 8 << 30}
	return d
}

// Every reading must be a single non-streaming frame without the daemon's own
// pre-sample (?stream=false&one-shot=true, the old ContainerStatsOneShot): the
// cache keeps the previous reading itself, and a pre-sample would make each
// read wait a second on the daemon.
func TestStatsCacheReadsOneShot(t *testing.T) {
	d := statsFake(0, 0)
	c := newContainerStatsCache(d, discardLog(), time.Hour)
	c.refresh(context.Background())
	c.refresh(context.Background())

	opts := d.statsCallOpts()
	if len(opts) != 2 {
		t.Fatalf("ContainerStats calls = %d, want 2", len(opts))
	}
	for i, o := range opts {
		if o != (client.ContainerStatsOptions{Stream: false, IncludePreviousSample: false}) {
			t.Errorf("call %d options = %+v, want a one-shot read", i, o)
		}
	}
}

// The first pass carries memory but no CPU — a percentage needs two readings.
// The second pass has both. Getting this wrong would show a confident 0%.
func TestStatsCacheNeedsTwoPassesForCPU(t *testing.T) {
	c := newContainerStatsCache(statsFake(0, 0), discardLog(), time.Hour)
	ctx := context.Background()

	c.refresh(ctx)
	samples, _, cpuReady := c.get()
	if cpuReady {
		t.Error("cpu cannot be ready after a single reading")
	}
	if got := samples["c1"].memBytes; got != 600 {
		t.Errorf("memory = %d, want 600 from the very first reading", got)
	}

	c.refresh(ctx)
	samples, sampledAt, cpuReady := c.get()
	if !cpuReady {
		t.Fatal("cpu should be ready once two readings exist")
	}
	if pct := samples["c1"].cpuPct; pct < 199.9 || pct > 200.1 {
		t.Errorf("cpu = %.2f%%, want 200%%", pct)
	}
	if sampledAt.IsZero() {
		t.Error("sampledAt should be set")
	}
}

// A container with no limit of its own must not be reported as if the node's
// total memory were its budget.
func TestStatsCacheLimitOwnership(t *testing.T) {
	ctx := context.Background()

	unlimited := newContainerStatsCache(statsFake(0, 0), discardLog(), time.Hour)
	unlimited.refresh(ctx)
	if s := unlimited.samples["c1"]; s.memIsOwn {
		t.Error("a container without its own memory limit must not claim one")
	}

	limited := newContainerStatsCache(statsFake(512<<20, 2_000_000_000), discardLog(), time.Hour)
	limited.refresh(ctx)
	s := limited.samples["c1"]
	if !s.memIsOwn || s.memLimit != 512<<20 {
		t.Errorf("own memory limit not picked up: own=%v limit=%d", s.memIsOwn, s.memLimit)
	}
	if s.cpuLimit < 1.99 || s.cpuLimit > 2.01 {
		t.Errorf("cpu limit = %v cores, want 2", s.cpuLimit)
	}
}

// Limits are inspected once per container, not on every pass — the whole point
// of caching them.
func TestStatsCacheInspectsLimitsOnce(t *testing.T) {
	d := statsFake(512<<20, 0)
	c := newContainerStatsCache(d, discardLog(), time.Hour)
	ctx := context.Background()
	c.refresh(ctx)
	c.refresh(ctx)
	c.refresh(ctx)
	if n := d.inspectCalls(); n != 1 {
		t.Errorf("%d inspect calls, want 1 — limits do not change while a container lives", n)
	}
}

// The sampler must stop working when nobody is reading, and must drop what it
// holds rather than serving numbers from before the pause.
func TestStatsCacheIdlesAndForgets(t *testing.T) {
	c := newContainerStatsCache(statsFake(0, 0), discardLog(), time.Hour)
	ctx := context.Background()

	if c.wanted() {
		t.Error("a cache nobody has asked for should not be sampling")
	}
	c.get() // a reader arms it
	if !c.wanted() {
		t.Error("a recent reader should keep it sampling")
	}

	c.refresh(ctx)
	c.refresh(ctx)
	if _, _, ready := c.get(); !ready {
		t.Fatal("expected readings before going idle")
	}

	// Rewind the last request past the idle window.
	c.mu.Lock()
	c.lastRequest = time.Now().Add(-2 * statsIdleAfter)
	c.mu.Unlock()
	if c.wanted() {
		t.Error("a cache nobody has read for a while should stop sampling")
	}
	c.sleep()
	samples, sampledAt, ready := c.get()
	if len(samples) != 0 || ready || !sampledAt.IsZero() {
		t.Errorf("going idle must drop the readings, got %d samples ready=%v", len(samples), ready)
	}
}

// A container that disappears must not leave its limits behind forever.
func TestStatsCachePrunesLimits(t *testing.T) {
	d := statsFake(512<<20, 0)
	c := newContainerStatsCache(d, discardLog(), time.Hour)
	c.refresh(context.Background())
	if len(c.limits) != 1 {
		t.Fatalf("expected the container's limits to be cached, got %d", len(c.limits))
	}

	d.mu.Lock()
	d.containers = nil
	d.mu.Unlock()
	c.refresh(context.Background())
	if len(c.limits) != 0 {
		t.Errorf("limits of a departed container should be pruned, got %d", len(c.limits))
	}
}

// A docker that cannot list containers must keep the previous readings rather
// than blanking the view on one transient error.
func TestStatsCacheKeepsReadingsOnListError(t *testing.T) {
	d := statsFake(0, 0)
	c := newContainerStatsCache(d, discardLog(), time.Hour)
	ctx := context.Background()
	c.refresh(ctx)
	c.refresh(ctx)

	d.mu.Lock()
	d.listErr = context.DeadlineExceeded
	d.mu.Unlock()
	c.refresh(ctx)

	samples, _, ready := c.get()
	if len(samples) != 1 || !ready {
		t.Errorf("a failed list should keep the previous readings, got %d samples ready=%v", len(samples), ready)
	}
}

// The health verdict is read out of the status line the container list already
// returns, so it costs nothing. That makes the parser the whole risk.
func TestHealthFromStatus(t *testing.T) {
	cases := []struct {
		status, want string
	}{
		{"Up 3 days (healthy)", healthHealthy},
		{"Up 2 minutes (unhealthy)", healthUnhealthy},
		{"Up 5 seconds (health: starting)", healthStarting},
		{"Up 3 days", ""}, // no healthcheck configured
		{"Exited (0) 2 days ago", ""},
		{"", ""},
		// Anything we do not recognise must NOT become a verdict. Reporting a
		// container as healthy because the daemon reworded its status would be
		// worse than admitting we do not know.
		{"Up 3 days (paused)", ""},
		{"Up 3 days (something new)", ""},
		{"Up 3 days (", ""},
		{"Up 3 days )healthy(", ""},
	}
	for _, tc := range cases {
		if got := healthFromStatus(tc.status); got != tc.want {
			t.Errorf("healthFromStatus(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

// Health rides along with the usage readings, from the list call the sampler
// already makes — no extra inspect per container.
func TestStatsCacheCarriesHealth(t *testing.T) {
	d := statsFake(0, 0)
	d.containers[0].Status = "Up 3 days (unhealthy)"
	c := newContainerStatsCache(d, discardLog(), time.Hour)
	c.refresh(context.Background())
	if got := c.samples["c1"].health; got != healthUnhealthy {
		t.Errorf("health = %q, want unhealthy", got)
	}
	if n := d.inspectCalls(); n != 1 {
		t.Errorf("%d inspect calls — health must not add one per pass (only the one-off limits read)", n)
	}
}
