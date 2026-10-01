// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
	"time"

	"swarmexec/client/internal/resolve"
)

func TestSparklineScalesFixedAndDrawsGapsBlank(t *testing.T) {
	s := []usageSample{{cpu: 0}, {cpu: 0.5}, {gap: true}, {cpu: 1}, {cpu: 2}, {cpu: -1}}
	got := sparkline(s, 10, func(x usageSample) float64 { return x.cpu })
	if got != "▁▅ ██▁" {
		t.Errorf("sparkline = %q (fixed 0..1 scale, clamped, gap blank)", got)
	}
	// Only the newest samples fit.
	if got := sparkline(s, 2, func(x usageSample) float64 { return x.cpu }); got != "█▁" {
		t.Errorf("narrow sparkline = %q", got)
	}
}

// A hole in the readings is drawn as a hole: a node unreachable for minutes is
// not a container idling at zero.
func TestRingMarksAGapAndKeepsOnlyTheWindow(t *testing.T) {
	t0 := time.Now()
	r := &usageRing{}
	r.add(usageSample{at: t0, cpu: 0.2})
	r.add(usageSample{at: t0.Add(10 * time.Second), cpu: 0.3})
	r.add(usageSample{at: t0.Add(10*time.Second + usageGapAfter + time.Second), cpu: 0.9})
	if len(r.samples) != 4 || !r.samples[2].gap {
		t.Fatalf("want a gap marker before the late reading, got %+v", r.samples)
	}
	r.add(usageSample{at: t0.Add(usageHistoryWindow + time.Hour), cpu: 0.1})
	for _, s := range r.samples {
		if t0.Add(usageHistoryWindow+time.Hour).Sub(s.at) > usageHistoryWindow {
			t.Fatalf("sample older than the window kept: %+v", s)
		}
	}
	for i := 0; i < usageHistoryCap*2; i++ {
		r.add(usageSample{at: t0.Add(2*time.Hour + time.Duration(i)*time.Second)})
	}
	if len(r.samples) > usageHistoryCap {
		t.Errorf("ring grew to %d, cap %d", len(r.samples), usageHistoryCap)
	}
}

func TestRecordUsageSkipsFirstReadingsAndForgetsTheGone(t *testing.T) {
	now := time.Now()
	hist := map[string]*usageRing{
		"gone": {samples: []usageSample{{at: now.Add(-usageHistoryWindow - time.Minute)}}},
	}
	recordUsage(hist, now, map[string]containerUsage{
		"a":     {CPUReady: true, CPURatio: 0.4, MemRatio: 0.1},
		"fresh": {CPUReady: false, MemRatio: 0.2}, // one reading: no cpu yet, not 0%
	})
	if _, ok := hist["fresh"]; ok {
		t.Error("a first reading has no cpu and must not enter the history as 0%")
	}
	if _, ok := hist["gone"]; ok {
		t.Error("a container silent for a whole window must be forgotten")
	}
	if r := hist["a"]; r == nil || len(r.samples) != 1 || r.samples[0].cpu != 0.4 {
		t.Errorf("a = %+v", hist["a"])
	}
}

func TestHistoryRowsLabelAndAlign(t *testing.T) {
	t0 := time.Now()
	ring := func() *usageRing {
		return &usageRing{samples: []usageSample{{at: t0, cpu: 0.1, mem: 0.5}, {at: t0.Add(5 * time.Minute), cpu: 0.9, mem: 0.5}}}
	}
	rows := historyRows([]resolve.Candidate{
		{Service: "web", Slot: 1, ContainerID: "c1"},
		{Service: "web", Slot: 12, ContainerID: "c12"},
		{Service: "web", Slot: 2, ContainerID: "none"}, // no history yet: no row
	}, map[string]*usageRing{"c1": ring(), "c12": ring()})
	if len(rows) != 2 {
		t.Fatalf("rows = %q", rows)
	}
	if !strings.HasPrefix(rows[0], "  web.1   cpu ▂▇") || !strings.Contains(rows[0], "last 5 min") {
		t.Errorf("row = %q", rows[0])
	}
	if strings.Index(rows[0], "cpu") != strings.Index(rows[1], "cpu") {
		t.Errorf("rows not aligned:\n%s\n%s", rows[0], rows[1])
	}
}
