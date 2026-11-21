// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package clockskew turns one Version round trip into a statement about a
// node's clock.
//
// The problem it exists to solve is that a single request cannot measure a
// remote clock exactly. The agent reads its clock somewhere between the moment
// we sent the request and the moment we read the reply, and we do not know
// where in that window. So an answer that looks half a round trip away from
// ours is not evidence of anything — it is what a correct clock looks like
// over a slow link. This package keeps that uncertainty instead of rounding it
// away, and reports drift only when it exceeds what the round trip alone could
// explain.
package clockskew

import (
	"fmt"
	"time"
)

// Warn is the offset from which a node's clock is worth a line in `doctor`.
//
// It is a constant and not a flag on purpose. Below two seconds nothing in a
// swarm misbehaves: certificate lifetimes are measured in hours and log
// correlation in seconds. Above it, timestamps from two nodes stop lining up
// and every later diagnosis built on them is quietly wrong. A threshold that
// can be argued about becomes a flag, a flag becomes documentation, and
// `doctor` stops being a command with one answer.
const Warn = 2 * time.Second

// Reading is what one round trip says about a node's clock.
type Reading struct {
	// Known is false when the agent sent no time: one too old to carry the
	// field, which is not the same as a clock that agrees.
	Known bool
	// Offset is the node's clock minus ours, measured against the midpoint of
	// the round trip — the best single number the exchange supports.
	Offset time.Duration
	// Slack is half the round trip: how much of Offset the network alone could
	// account for. An Offset within Slack says nothing at all.
	Slack time.Duration
}

// Measure turns the agent's reported time into a Reading. sent and received
// bracket the call on this machine's clock; now is VersionResponse.now.
//
// An unparseable time is treated exactly like a missing one. The field is
// either an RFC3339 instant or it is noise, and a client that guessed at the
// difference would be inventing the measurement it is supposed to take.
func Measure(sent, received time.Time, now string) Reading {
	if now == "" {
		return Reading{}
	}
	t, err := time.Parse(time.RFC3339Nano, now)
	if err != nil {
		return Reading{}
	}
	trip := received.Sub(sent)
	if trip < 0 {
		trip = 0 // a clock that stepped mid-call; the midpoint is still the best guess
	}
	return Reading{
		Known:  true,
		Offset: t.Sub(sent.Add(trip / 2)),
		Slack:  trip / 2,
	}
}

// Beyond reports whether the drift exceeds limit by more than the round trip
// can explain. A Reading that is not Known is never beyond anything: silence
// about an old agent beats a number nobody measured.
func (r Reading) Beyond(limit time.Duration) bool {
	return r.Known && abs(r.Offset)-r.Slack > limit
}

// String is the drift as a report reads it: a signed, rounded offset, where
// the sign says which way — "+3s" is a node running ahead of this machine.
// An unknown reading is the empty string, so a caller can leave the cell
// blank rather than print a zero it did not measure.
func (r Reading) String() string {
	if !r.Known {
		return ""
	}
	d := r.Offset.Round(time.Millisecond)
	if abs(d) >= time.Second {
		d = d.Round(100 * time.Millisecond)
	}
	if d >= 0 {
		return "+" + d.String()
	}
	return d.String()
}

// Summary is the sentence for a node whose clock is beyond Warn, naming the
// direction, because "ahead" and "behind" lead to different causes: a node
// ahead usually has no NTP at all, a node behind has one that stopped.
func (r Reading) Summary() string {
	if !r.Known {
		return ""
	}
	dir := "ahead of"
	if r.Offset < 0 {
		dir = "behind"
	}
	return fmt.Sprintf("clock %s %s this machine", r.String(), dir)
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
