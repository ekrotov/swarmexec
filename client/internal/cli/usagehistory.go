// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"time"
)

// The live usage numbers answer "now"; in an incident the question is "since
// when". Standing at 90% and having just shot up to 90% are different
// diagnoses, and the readings that tell them apart arrive with every refresh
// anyway — they used to be thrown away. A short ring per container keeps the
// last minutes, in memory only: no persistence, no export, nothing that would
// make this a monitoring system (the agent has a Prometheus endpoint for that).
//
// A gap is a gap. The agent's sampler sleeps when nobody asks, a cluster that
// is not on screen is not polled, and a node can be unreachable — none of that
// is a reading of zero, and drawing it as one would show a service idling at
// the very moment nobody was looking.

const (
	usageHistoryWindow = 15 * time.Minute
	usageHistoryCap    = 120 // ≥ the window at the fast 10 s poll, with room
	// usageGapAfter is how long between two readings counts as a hole rather
	// than a slow poll: three times the slowest regular interval.
	usageGapAfter = 3 * treeRefreshSlow
)

type usageSample struct {
	at       time.Time
	cpu, mem float64 // ratios 0..1 of what the container may use
	gap      bool    // no reading between the previous sample and this one
}

type usageRing struct{ samples []usageSample }

func (r *usageRing) add(s usageSample) {
	if n := len(r.samples); n > 0 && s.at.Sub(r.samples[n-1].at) > usageGapAfter {
		r.samples = append(r.samples, usageSample{at: s.at, gap: true})
	}
	r.samples = append(r.samples, s)
	cut := 0
	for cut < len(r.samples) && (s.at.Sub(r.samples[cut].at) > usageHistoryWindow || len(r.samples)-cut > usageHistoryCap) {
		cut++
	}
	r.samples = r.samples[cut:]
}

// recordUsage appends this pass's readings and forgets containers that have
// not reported for a whole window (they are gone, or their node is).
func recordUsage(hist map[string]*usageRing, now time.Time, usage map[string]containerUsage) {
	for id, u := range usage {
		if !u.CPUReady {
			continue // a first reading carries no cpu yet; not a 0%
		}
		r := hist[id]
		if r == nil {
			r = &usageRing{}
			hist[id] = r
		}
		r.add(usageSample{at: now, cpu: u.CPURatio, mem: u.MemRatio})
	}
	for id, r := range hist {
		if n := len(r.samples); n == 0 || now.Sub(r.samples[n-1].at) > usageHistoryWindow {
			delete(hist, id)
		}
	}
}

var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// sparkline draws the last width samples on a fixed 0..1 scale — fixed so two
// containers' lines compare, and so a flat 3% does not render as a wall. A gap
// is a blank, never a low bar.
func sparkline(samples []usageSample, width int, val func(usageSample) float64) string {
	if len(samples) > width {
		samples = samples[len(samples)-width:]
	}
	var b strings.Builder
	for _, s := range samples {
		if s.gap {
			b.WriteRune(' ')
			continue
		}
		v := val(s)
		switch {
		case v < 0:
			v = 0
		case v > 1:
			v = 1
		}
		b.WriteRune(sparkBlocks[int(v*float64(len(sparkBlocks)-1)+0.5)])
	}
	return b.String()
}

// spanOf is how much time the drawn samples cover, for the label.
func spanOf(samples []usageSample, width int) time.Duration {
	if len(samples) > width {
		samples = samples[len(samples)-width:]
	}
	if len(samples) < 2 {
		return 0
	}
	return samples[len(samples)-1].at.Sub(samples[0].at)
}
