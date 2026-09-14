// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
)

const (
	// statsInterval is how often the sampler takes a reading. It is also the
	// resolution of every CPU percentage, since that number is a delta between
	// two consecutive readings.
	statsInterval = 5 * time.Second

	// statsIdleAfter stops the sampling when nobody has asked for a while. A
	// fleet agent spends almost all its life with no operator looking at it, and
	// reading cgroup files for every container forever would be a permanent cost
	// for data nobody reads. The first request after an idle period arms the
	// sampler again.
	statsIdleAfter = 60 * time.Second

	// statsConcurrency bounds the per-container calls of one sampling pass. A
	// node can hold a lot of containers and each reading is an API round trip;
	// unbounded fan-out would make the sampler itself the load.
	statsConcurrency = 8

	// statsSampleTimeout bounds one container's reading, so a single wedged
	// container cannot stall the whole pass.
	statsSampleTimeout = 3 * time.Second
)

// containerSample is one container's raw reading, kept so the next pass can
// turn the CPU counters into a percentage.
type containerSample struct {
	cpuTotal    uint64 // cgroup cpuacct total, nanoseconds
	systemTotal uint64 // host-wide cpu time at the same instant
	onlineCPUs  uint32

	health    string // healthcheck verdict, "" when there is none
	memBytes  int64
	memLimit  int64
	memIsOwn  bool // the limit is the container's own, not the node's total
	cpuLimit  float64
	cpuUsable bool // a percentage could be computed (there was a previous sample)
	cpuPct    float64
}

// containerStatsCache samples container resource usage in the background so the
// Stats RPC answers from memory. Two reasons it is a cache and not a per-request
// read: a CPU percentage is a delta between two readings, so something has to
// remember the previous one; and several clients polling the same node would
// otherwise each trigger their own scan.
//
// It samples only while someone is actually asking (see statsIdleAfter), and
// drops what it holds when it goes idle rather than serving numbers from
// minutes ago.
type containerStatsCache struct {
	docker   DockerClient
	log      *slog.Logger
	interval time.Duration

	mu          sync.RWMutex
	samples     map[string]containerSample
	sampledAt   time.Time
	cpuReady    bool
	lastRequest time.Time

	// Limits are per container and never change while it lives (a redeploy makes
	// a new container with a new id), so they are inspected once on first sight
	// and pruned when the container goes away — not re-read every pass.
	limits map[string]containerLimits

	// Node capacity is the denominator for "how loaded is this node". It is read
	// once and kept; a node does not grow CPUs while the agent runs.
	nodeCPUs int64
	nodeMem  int64
}

// containerLimits is what a container is allowed to use, as the cgroup sees it.
// Zero means "no limit of its own", in which case the node's capacity is the
// real ceiling.
type containerLimits struct {
	cpuCores float64
	memBytes int64
}

func newContainerStatsCache(docker DockerClient, log *slog.Logger, interval time.Duration) *containerStatsCache {
	if interval <= 0 {
		interval = statsInterval
	}
	return &containerStatsCache{
		docker:   docker,
		log:      log,
		interval: interval,
		samples:  map[string]containerSample{},
		limits:   map[string]containerLimits{},
	}
}

// capacity returns the node's CPU count and total memory, reading it from the
// daemon the first time it is needed. A failure is not fatal: the client falls
// back to per-container limits and the next pass tries again.
func (c *containerStatsCache) capacity(ctx context.Context) (cpus, mem int64) {
	c.mu.RLock()
	cpus, mem = c.nodeCPUs, c.nodeMem
	c.mu.RUnlock()
	if cpus > 0 && mem > 0 {
		return cpus, mem
	}
	info, err := c.docker.Info(ctx)
	if err != nil {
		c.log.Debug("stats: docker info failed; node capacity unknown", "err", err)
		return cpus, mem
	}
	c.mu.Lock()
	c.nodeCPUs, c.nodeMem = int64(info.NCPU), info.MemTotal
	cpus, mem = c.nodeCPUs, c.nodeMem
	c.mu.Unlock()
	return cpus, mem
}

// limitsFor returns a container's limits, inspecting it the first time it is
// seen. live is the set of container ids in this pass; entries for containers
// that have gone are dropped so the map cannot grow without bound on a node
// that churns.
func (c *containerStatsCache) limitsFor(ctx context.Context, id string) containerLimits {
	c.mu.RLock()
	lim, ok := c.limits[id]
	c.mu.RUnlock()
	if ok {
		return lim
	}
	insp, err := c.docker.ContainerInspect(ctx, id)
	if err != nil || insp.HostConfig == nil {
		// Cache the zero value anyway: a container we cannot inspect should not be
		// re-inspected every five seconds for the rest of its life.
		c.log.Debug("stats: inspect for limits failed", "container", id, "err", err)
	} else {
		lim = containerLimits{
			cpuCores: float64(insp.HostConfig.NanoCPUs) / 1e9,
			memBytes: insp.HostConfig.Memory,
		}
	}
	c.mu.Lock()
	c.limits[id] = lim
	c.mu.Unlock()
	return lim
}

func (c *containerStatsCache) pruneLimits(live map[string]containerSample) {
	c.mu.Lock()
	for id := range c.limits {
		if _, ok := live[id]; !ok {
			delete(c.limits, id)
		}
	}
	c.mu.Unlock()
}

// get returns the current readings and marks the cache as wanted, which is what
// keeps the sampler running. Treat the map as read-only — a pass swaps it whole.
func (c *containerStatsCache) get() (samples map[string]containerSample, sampledAt time.Time, cpuReady bool) {
	c.mu.Lock()
	c.lastRequest = time.Now()
	samples, sampledAt, cpuReady = c.samples, c.sampledAt, c.cpuReady
	c.mu.Unlock()
	return samples, sampledAt, cpuReady
}

// wanted reports whether anyone has asked recently enough to keep sampling.
func (c *containerStatsCache) wanted() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.lastRequest.IsZero() && time.Since(c.lastRequest) < statsIdleAfter
}

// sleep drops the readings when the cache goes idle. Keeping them would mean the
// next reader gets numbers from before the pause with no way to tell.
func (c *containerStatsCache) sleep() {
	c.mu.Lock()
	if len(c.samples) > 0 || c.cpuReady {
		c.samples = map[string]containerSample{}
		c.cpuReady = false
		c.sampledAt = time.Time{}
	}
	c.mu.Unlock()
}

// run samples on a ticker for as long as someone is reading, until ctx ends.
func (c *containerStatsCache) run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.wanted() {
				c.sleep()
				continue
			}
			c.refresh(ctx)
		}
	}
}

// refresh takes one reading of every running container and swaps it in. CPU
// percentages are computed against the previous pass, so the first pass after
// an idle period carries memory only.
func (c *containerStatsCache) refresh(ctx context.Context) {
	list, err := c.docker.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		c.log.Warn("stats: container list failed; keeping previous readings", "err", err)
		return
	}

	c.mu.RLock()
	prev := c.samples
	c.mu.RUnlock()

	next := make(map[string]containerSample, len(list))
	var mu sync.Mutex
	anyCPU := false

	sem := make(chan struct{}, statsConcurrency)
	var wg sync.WaitGroup
	for _, ct := range list {
		id := ct.ID
		// The list entry already carries the healthcheck verdict, so health costs
		// nothing on top of the pass we are making anyway.
		health := healthFromStatus(ct.Status)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s, ok := c.sample(ctx, id)
			if !ok {
				return
			}
			s.health = health
			lim := c.limitsFor(ctx, id)
			s.cpuLimit = lim.cpuCores
			// The stats frame reports the node's total memory as the "limit" for a
			// container that has none, so the cgroup limit alone cannot say which
			// it is. The container's own configured limit can.
			if lim.memBytes > 0 {
				s.memLimit, s.memIsOwn = lim.memBytes, true
			}
			if p, had := prev[id]; had {
				if pct, ok := cpuPercent(p, s); ok {
					s.cpuPct, s.cpuUsable = pct, true
				}
			}
			mu.Lock()
			next[id] = s
			if s.cpuUsable {
				anyCPU = true
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	c.mu.Lock()
	c.samples = next
	c.sampledAt = time.Now()
	// cpuReady says the numbers in this pass include CPU. It stays false for the
	// first pass after idle, when there is nothing to compare against.
	c.cpuReady = anyCPU
	c.mu.Unlock()
	c.pruneLimits(next)
	c.capacity(ctx)
}

// sample reads one container's stats. A failure is silent-ish: a container that
// stopped between the list and the read is the normal case, not an error worth
// logging at volume.
func (c *containerStatsCache) sample(ctx context.Context, id string) (containerSample, bool) {
	cctx, cancel := context.WithTimeout(ctx, statsSampleTimeout)
	defer cancel()
	resp, err := c.docker.ContainerStatsOneShot(cctx, id)
	if err != nil {
		c.log.Debug("stats: one-shot read failed", "container", id, "err", err)
		return containerSample{}, false
	}
	defer resp.Body.Close()

	var raw container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		c.log.Debug("stats: decode failed", "container", id, "err", err)
		return containerSample{}, false
	}
	return sampleFrom(raw), true
}

// sampleFrom converts a docker stats frame into the reading we keep. Split out
// so the arithmetic — which is easy to get subtly wrong — is unit-testable
// without a docker daemon.
func sampleFrom(raw container.StatsResponse) containerSample {
	s := containerSample{
		cpuTotal:    raw.CPUStats.CPUUsage.TotalUsage,
		systemTotal: raw.CPUStats.SystemUsage,
		onlineCPUs:  raw.CPUStats.OnlineCPUs,
		memBytes:    memoryUsage(raw.MemoryStats),
		memLimit:    int64(raw.MemoryStats.Limit),
	}
	// Docker substitutes the node's total memory when a container has no limit of
	// its own. We cannot tell the two apart from this frame alone, so the caller
	// compares against the node total; see Server.Stats.
	return s
}

// memoryUsage is usage minus the page cache, which is what `docker stats`
// reports and what an operator means by "how much memory is this using". Raw
// usage counts reclaimable file cache and can sit near the limit on a container
// that is doing nothing wrong.
func memoryUsage(m container.MemoryStats) int64 {
	usage := int64(m.Usage)
	// cgroup v2 calls it inactive_file, v1 calls it cache (total_inactive_file on
	// v1 with hierarchy). Whichever is present, subtract it once.
	for _, key := range []string{"inactive_file", "total_inactive_file", "cache"} {
		if v, ok := m.Stats[key]; ok {
			if int64(v) <= usage {
				return usage - int64(v)
			}
			return 0
		}
	}
	return usage
}

// cpuPercent turns two readings into the number `docker stats` prints: the share
// of ONE cpu, so 250.0 means two and a half cores busy. It reports ok=false when
// the counters cannot produce a meaningful delta (a restarted container whose
// counters went backwards, or two readings taken at the same instant).
func cpuPercent(prev, cur containerSample) (float64, bool) {
	if cur.cpuTotal < prev.cpuTotal || cur.systemTotal < prev.systemTotal {
		return 0, false // counters reset — container restarted
	}
	cpuDelta := float64(cur.cpuTotal - prev.cpuTotal)
	sysDelta := float64(cur.systemTotal - prev.systemTotal)
	if sysDelta <= 0 || cpuDelta < 0 {
		return 0, false
	}
	cpus := float64(cur.onlineCPUs)
	if cpus <= 0 {
		cpus = 1
	}
	return cpuDelta / sysDelta * cpus * 100.0, true
}

// Health verdicts, matching docker's own vocabulary.
const (
	healthHealthy   = "healthy"
	healthUnhealthy = "unhealthy"
	healthStarting  = "starting"
)

// healthFromStatus reads the healthcheck verdict out of the status line the
// container list already returns ("Up 3 days (healthy)").
//
// The list API has no structured health field — only ContainerInspect does, and
// running one per container per pass would multiply the sampler's cost for a
// value the daemon has already formatted for us. This is the same string
// `docker ps` prints.
//
// Anything unrecognised yields "", which the views render as "no healthcheck".
// That is the honest failure mode: if the daemon ever changes the wording we
// stop claiming to know, rather than reporting a container as healthy.
func healthFromStatus(status string) string {
	open := strings.LastIndexByte(status, '(')
	if open < 0 || !strings.HasSuffix(status, ")") {
		return "" // "Up 3 days" — no healthcheck configured
	}
	switch inner := status[open+1 : len(status)-1]; inner {
	case healthHealthy, healthUnhealthy:
		return inner
	case "health: starting":
		return healthStarting
	default:
		return ""
	}
}
