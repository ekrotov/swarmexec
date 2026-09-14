// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// A percentage is only meaningful with its denominator. A container with its
// own limit is measured against that limit — that is what gets it throttled or
// OOM-killed — and one without is measured against the node.
func TestUsageOfPicksTheRightDenominator(t *testing.T) {
	resp := &pb.StatsResponse{CpuReady: true, NodeCpus: 8, NodeMemoryTotalBytes: 16 << 30}

	// Own limits: 1.8 of 2 allotted cores and 900 MB of a 1 GB cap — both hot,
	// even though against the node they would look harmless.
	own := usageOf(&pb.ContainerStats{
		CpuPercent: 180, CpuLimitCores: 2,
		MemoryBytes: 900 << 20, MemoryLimitBytes: 1 << 30, MemoryLimited: true,
	}, resp)
	if !own.CPULimited || own.CPURatio < 0.89 || own.CPURatio > 0.91 {
		t.Errorf("cpu ratio = %.3f (limited=%v), want ~0.90 of its own limit", own.CPURatio, own.CPULimited)
	}
	if !own.MemLimited || own.MemRatio < 0.87 || own.MemRatio > 0.89 {
		t.Errorf("mem ratio = %.3f, want ~0.88 of its own limit", own.MemRatio)
	}
	if own.level() != usageCrit {
		t.Errorf("level = %v, want crit at 90%% of its own cpu limit", own.level())
	}

	// Same absolute usage, no limits: now it is a share of the node, and modest.
	free := usageOf(&pb.ContainerStats{
		CpuPercent:  180,
		MemoryBytes: 900 << 20, MemoryLimitBytes: 16 << 30,
	}, resp)
	if free.CPULimited {
		t.Error("a container with no cpu limit must not claim one")
	}
	if free.CPURatio < 0.22 || free.CPURatio > 0.23 {
		t.Errorf("cpu ratio = %.3f, want ~0.225 of eight cores", free.CPURatio)
	}
	if free.level() != usageOK {
		t.Errorf("level = %v, want ok", free.level())
	}
}

// A CPU reading that does not exist yet must not be read as 0%.
func TestUsageIgnoresUnreadyCPU(t *testing.T) {
	resp := &pb.StatsResponse{CpuReady: false, NodeCpus: 4, NodeMemoryTotalBytes: 8 << 30}
	// 7.7 of 8 GiB — comfortably past the 90% mark on memory alone.
	u := usageOf(&pb.ContainerStats{MemoryBytes: 7700 << 20, MemoryLimitBytes: 8 << 30}, resp)

	if u.CPUReady {
		t.Error("cpu should not be marked ready")
	}
	// Memory alone still decides the level — it is valid from the first reading.
	if u.level() != usageCrit {
		t.Errorf("level = %v, want crit from memory alone", u.level())
	}
	if got := formatCPUUsage(u); got != "…" {
		t.Errorf("cpu display = %q, want a placeholder rather than a number", got)
	}
	if strings.Contains(usageBadge(u), cpuGlyph) {
		t.Error("an unmeasured cpu must not produce a cpu badge")
	}
}

func TestLevelThresholds(t *testing.T) {
	cases := []struct {
		ratio float64
		want  usageLevel
	}{
		{0, usageOK}, {0.69, usageOK},
		{0.70, usageWarn}, {0.89, usageWarn},
		{0.90, usageCrit}, {1.5, usageCrit},
	}
	for _, tc := range cases {
		if got := levelOf(tc.ratio); got != tc.want {
			t.Errorf("levelOf(%.2f) = %v, want %v", tc.ratio, got, tc.want)
		}
	}
}

// The badge is a signal, so it must be absent on a healthy row and must name
// the resource that is actually hot.
func TestUsageBadge(t *testing.T) {
	if got := usageBadge(containerUsage{CPUReady: true, CPURatio: 0.4, MemRatio: 0.5}); got != "" {
		t.Errorf("a healthy container should carry no badge, got %q", got)
	}

	hotCPU := usageBadge(containerUsage{CPUReady: true, CPURatio: 0.95, MemRatio: 0.1})
	if !strings.Contains(hotCPU, cpuGlyph) || !strings.Contains(hotCPU, "red") {
		t.Errorf("cpu at 95%% should badge red: %q", hotCPU)
	}
	if strings.Contains(hotCPU, memGlyph) {
		t.Errorf("memory is fine and must not be badged: %q", hotCPU)
	}

	warmMem := usageBadge(containerUsage{CPUReady: true, CPURatio: 0.1, MemRatio: 0.75})
	if !strings.Contains(warmMem, memGlyph) || !strings.Contains(warmMem, "orange") {
		t.Errorf("memory at 75%% should badge orange: %q", warmMem)
	}

	both := usageBadge(containerUsage{CPUReady: true, CPURatio: 0.92, MemRatio: 0.8})
	if !strings.Contains(both, cpuGlyph) || !strings.Contains(both, memGlyph) {
		t.Errorf("both resources hot should badge both: %q", both)
	}
}

func usageCand(service, id string, slot int) resolve.Candidate {
	return resolve.Candidate{Service: service, ContainerID: id, Slot: slot}
}

// A service is as stressed as its WORST replica. Averaging would hide the one
// container that is about to die, which is the whole point of the badge.
func TestServiceUsageTakesThePeak(t *testing.T) {
	cands := []resolve.Candidate{usageCand("web", "c1", 1), usageCand("web", "c2", 2), usageCand("db", "c3", 1)}
	usage := map[string]containerUsage{
		"c1": {CPUReady: true, CPURatio: 0.10, MemRatio: 0.20, MemBytes: 100},
		"c2": {CPUReady: true, CPURatio: 0.95, MemRatio: 0.30, MemBytes: 200},
		"c3": {CPUReady: true, CPURatio: 0.99, MemRatio: 0.99, MemBytes: 999},
	}

	agg, ok := serviceUsage(usage, cands, "web")
	if !ok {
		t.Fatal("web has measured containers")
	}
	if agg.CPURatio < 0.94 || agg.CPURatio > 0.96 {
		t.Errorf("cpu = %.2f, want the peak (0.95), not an average", agg.CPURatio)
	}
	if agg.MemRatio < 0.29 || agg.MemRatio > 0.31 {
		t.Errorf("mem ratio = %.2f, want the peak (0.30)", agg.MemRatio)
	}
	// Absolute memory is the one figure that genuinely adds up across replicas.
	if agg.MemBytes != 300 {
		t.Errorf("mem bytes = %d, want 300 summed over web's replicas", agg.MemBytes)
	}
	// The other service's readings must not leak in.
	if agg.CPURatio > 0.96 {
		t.Error("db's usage leaked into web's roll-up")
	}

	if _, ok := serviceUsage(usage, cands, "nope"); ok {
		t.Error("a service with no measured containers should report none")
	}
	if _, ok := serviceUsage(map[string]containerUsage{}, cands, "web"); ok {
		t.Error("no readings at all should report none")
	}
}

func TestStatsSubjectContainers(t *testing.T) {
	all := []resolve.Candidate{usageCand("web", "c1", 1), usageCand("web", "c2", 2), usageCand("db", "c3", 1)}

	if got := (statsSubject{service: "web"}).containers(all); len(got) != 2 {
		t.Errorf("service subject picked %d containers, want both replicas", len(got))
	}
	got := (statsSubject{containerID: "c2"}).containers(all)
	if len(got) != 1 || got[0].ContainerID != "c2" {
		t.Errorf("container subject = %+v, want just c2", got)
	}
	if n := len((statsSubject{}).containers(all)); n != 0 {
		t.Errorf("an empty subject measures nothing, got %d", n)
	}
}

// The basis line is what stops "91%" from being ambiguous.
func TestUsageBasisNamesTheDenominator(t *testing.T) {
	both := usageBasis(containerUsage{CPULimited: true, MemLimited: true})
	if !strings.Contains(both, "its own") || strings.Contains(both, "node") {
		t.Errorf("fully limited container: %q", both)
	}
	neither := usageBasis(containerUsage{})
	if !strings.Contains(neither, "node") || strings.Contains(neither, "its own") {
		t.Errorf("unlimited container: %q", neither)
	}
	// The mixed case must name both, or one of them is silently wrong.
	mixed := usageBasis(containerUsage{MemLimited: true})
	if !strings.Contains(mixed, "cpu") || !strings.Contains(mixed, "memory") {
		t.Errorf("mixed limits must spell out both: %q", mixed)
	}
}

func TestInspModeCycles(t *testing.T) {
	m := inspModeTable
	for _, want := range []string{"stats", "raw json", "table"} {
		m = m.next()
		if m.label() != want {
			t.Fatalf("next label = %q, want %q", m.label(), want)
		}
	}
}

// A node reading must not be invented when no agent answered.
func TestNodeUsageSectionAbsentWhenUnknown(t *testing.T) {
	n := swarmNodeInfo{Hostname: "host-a", NanoCPUs: 4e9, MemoryBytes: 8 << 30}
	if got := nodeUsageSection(n, nodeUsage{}, false); got != "" {
		t.Errorf("no agent reading should render nothing, got %q", got)
	}

	got := nodeUsageSection(n, nodeUsage{CPUReady: true, CPUPercent: 200, MemBytes: 4 << 30, NodeCPUs: 4, NodeMem: 8 << 30}, true)
	for _, want := range []string{"in use by containers", "cpu", "memory", "containers only"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage section missing %q:\n%s", want, got)
		}
	}

	// Before the second reading it must say it is measuring, not show 0%.
	warming := nodeUsageSection(n, nodeUsage{CPUReady: false, MemBytes: 1 << 30, NodeCPUs: 4, NodeMem: 8 << 30}, true)
	if !strings.Contains(warming, "measuring") {
		t.Errorf("an unready cpu reading should say so:\n%s", warming)
	}
}

// A cluster whose agents predate the Stats RPC must not cost a timeout per node
// on every refresh — measured at a full 10s per cycle before the gate existed.
func TestStatsGateBenchesFailingNodes(t *testing.T) {
	g := newStatsGate()
	if !g.allowed("node-a") {
		t.Fatal("an unseen node should be probed")
	}

	// An agent too old to know the RPC will keep saying so until it is
	// redeployed, so it waits far longer than a node that was merely away.
	g.fail("node-a", status.Error(codes.Unimplemented, "unknown method Stats"))
	g.fail("node-b", context.DeadlineExceeded)
	if g.allowed("node-a") || g.allowed("node-b") {
		t.Error("a node that just failed must not be probed again immediately")
	}

	g.mu.Lock()
	unsupported, transient := g.skip["node-a"], g.skip["node-b"]
	g.mu.Unlock()
	if !unsupported.After(transient) {
		t.Errorf("an unsupported agent should wait longer than an unreachable one: %v vs %v", unsupported, transient)
	}

	// A node that starts answering again is taken off the bench.
	g.ok("node-b")
	if !g.allowed("node-b") {
		t.Error("a node that answered should be probed again")
	}

	// The bench is time-based, not permanent.
	g.mu.Lock()
	g.skip["node-a"] = time.Now().Add(-time.Second)
	g.mu.Unlock()
	if !g.allowed("node-a") {
		t.Error("an expired bench should let the node be probed again")
	}

	// A nil gate is the no-op case, so callers need no special handling.
	var none *statsGate
	if !none.allowed("x") {
		t.Error("a nil gate should allow everything")
	}
	none.fail("x", nil)
	none.ok("x")
}

func healthCands() []resolve.Candidate {
	return []resolve.Candidate{usageCand("web", "c1", 1), usageCand("web", "c2", 2), usageCand("web", "c3", 3)}
}

// The whole point of F7: a service whose containers fail their probes must stop
// reading as a calm "3/3". The colour is what said so, and it lied.
func TestHealthColorContradictsTheCount(t *testing.T) {
	cands := healthCands()
	full := serviceColor(3, 3) // what the row used to be, unconditionally

	// Every container failing is as bad as none running.
	allBad := healthOf(map[string]containerUsage{
		"c1": {Health: healthUnhealthy}, "c2": {Health: healthUnhealthy}, "c3": {Health: healthUnhealthy},
	}, cands, "web")
	if got := healthColor(full, allBad); got != tcell.ColorRed {
		t.Errorf("all unhealthy = %v, want red", got)
	}

	// Some failing is degradation, like a missing replica.
	someBad := healthOf(map[string]containerUsage{
		"c1": {Health: healthUnhealthy}, "c2": {Health: healthHealthy}, "c3": {Health: healthHealthy},
	}, cands, "web")
	if got := healthColor(full, someBad); got != tcell.ColorOrange {
		t.Errorf("one of three unhealthy = %v, want orange", got)
	}

	// All healthy must not change what the count already said.
	allGood := healthOf(map[string]containerUsage{
		"c1": {Health: healthHealthy}, "c2": {Health: healthHealthy}, "c3": {Health: healthHealthy},
	}, cands, "web")
	if got := healthColor(full, allGood); got != full {
		t.Errorf("all healthy = %v, want the count's own colour %v", got, full)
	}

	// No healthchecks at all: we know nothing, so we must not recolour.
	none := healthOf(map[string]containerUsage{}, cands, "web")
	if got := healthColor(full, none); got != full {
		t.Errorf("no verdicts = %v, want the count's own colour", got)
	}
	if none.Checked() != 0 || none.Unknown != 3 {
		t.Errorf("unread containers should count as unknown: %+v", none)
	}

	// A degraded service must not be painted *better* by healthy containers.
	degraded := serviceColor(1, 3)
	if got := healthColor(degraded, allGood); got != degraded {
		t.Errorf("healthy probes must not upgrade a degraded row: %v", got)
	}
}

func TestHealthBadges(t *testing.T) {
	cands := healthCands()
	usage := map[string]containerUsage{
		"c1": {Health: healthUnhealthy}, "c2": {Health: healthStarting}, "c3": {Health: healthHealthy},
	}
	h := healthOf(usage, cands, "web")
	if h.Unhealthy != 1 || h.Starting != 1 || h.Healthy != 1 || h.Total() != 3 {
		t.Fatalf("counts wrong: %+v", h)
	}

	// Unhealthy outranks starting: it is the one that needs acting on.
	badge := healthBadge(h)
	if !strings.Contains(badge, "1 unhealthy") || !strings.Contains(badge, "red") {
		t.Errorf("badge = %q, want the unhealthy count in red", badge)
	}
	if strings.Contains(badge, "starting") {
		t.Errorf("badge = %q, should not also report starting", badge)
	}

	// Starting alone is worth showing, but not in red.
	starting := healthBadge(serviceHealth{Starting: 2, Healthy: 1})
	if !strings.Contains(starting, "2 starting") || strings.Contains(starting, "red") {
		t.Errorf("starting badge = %q", starting)
	}

	// A healthy service gets no badge — it must not become wallpaper.
	if got := healthBadge(serviceHealth{Healthy: 3}); got != "" {
		t.Errorf("healthy service badge = %q, want none", got)
	}
	if got := healthBadge(serviceHealth{Unknown: 3}); got != "" {
		t.Errorf("a service with no healthchecks must not be badged, got %q", got)
	}

	// Per-container markers follow the same rule.
	if got := containerHealthBadge(healthUnhealthy); !strings.Contains(got, "unhealthy") {
		t.Errorf("container badge = %q", got)
	}
	for _, quiet := range []string{healthHealthy, "", "bogus"} {
		if got := containerHealthBadge(quiet); got != "" {
			t.Errorf("containerHealthBadge(%q) = %q, want none", quiet, got)
		}
	}
}

// "No healthcheck configured" and "the probe passes" are different facts and
// must not render the same.
func TestHealthRowDistinguishesUnknownFromHealthy(t *testing.T) {
	healthy := healthRow(healthHealthy)
	none := healthRow("")
	if healthy.text == none.text {
		t.Error("a container without a healthcheck must not read as healthy")
	}
	if !strings.Contains(none.text, "no healthcheck") {
		t.Errorf("unknown row = %q", none.text)
	}
	if healthRow(healthUnhealthy).color != tcell.ColorRed {
		t.Error("a failing probe should be red")
	}
}

// A stack rolls health up like it rolls up updates and security findings. A
// stack row is what an operator scans first, so it must not read as calm while
// every service under it is failing its probes.
func TestStackHealthRollsUp(t *testing.T) {
	u := &ui{
		lastCands: []resolve.Candidate{
			usageCand("web", "c1", 1), usageCand("web", "c2", 2), usageCand("api", "c3", 1),
		},
		usage: map[string]containerUsage{
			"c1": {Health: healthUnhealthy},
			"c2": {Health: healthHealthy},
			"c3": {Health: healthUnhealthy},
		},
	}
	svcs := []resolve.Service{{Name: "web"}, {Name: "api"}}

	h := u.stackHealth(svcs)
	if h.Unhealthy != 2 || h.Healthy != 1 {
		t.Errorf("roll-up = %+v, want 2 unhealthy across both services", h)
	}
	if got := healthColor(serviceColor(3, 3), h); got != tcell.ColorOrange {
		t.Errorf("a stack with some unhealthy = %v, want orange rather than a calm full-count colour", got)
	}

	// With no readings at all the stack keeps the count's own colour.
	empty := (&ui{}).stackHealth(svcs)
	if empty.Total() != 0 {
		t.Errorf("no readings should roll up to nothing, got %+v", empty)
	}
	if got := healthColor(serviceColor(3, 3), empty); got != serviceColor(3, 3) {
		t.Error("a stack with no health readings must not be recoloured")
	}
}
