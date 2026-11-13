// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package limits holds the node caps of the agent: a counting semaphore that
// refuses, rather than queues, a caller beyond its limit, and the defaults for
// the two caps the server applies (concurrent streams, live forward sidecars).
package limits

import (
	"sync/atomic"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Default caps. They exist to bound a node's blast radius, not to ration normal
// use: a person driving a terminal holds a handful of streams, and even a busy
// TUI with several forwards and a log follow stays far below these. An operator
// who genuinely needs more raises them; the point is that the answer to "how
// many can one client open" stops being "as many as it likes".
//
// Without a cap, any principal that passes transport auth could open streams in
// a tight loop, and every port-forward stream creates a real container on the
// node — so the reachable end state was PID/memory exhaustion taking down every
// Swarm workload sharing that machine, from a client that only ever spoke the
// protocol correctly.
const (
	// DefaultMaxStreams caps Exec + Logs + PortForward streams in flight at once.
	DefaultMaxStreams = 256
	// DefaultMaxForwardSidecars caps live port-forward sidecar CONTAINERS, which
	// are far more expensive than a stream and are what actually exhausts a node.
	// Lower than DefaultMaxStreams on purpose: reaching this one first means the
	// client is refused before the containers exist.
	DefaultMaxForwardSidecars = 64
)

// OrDefault maps the "0 means default, negative means off" convention onto the
// limiter's "<= 0 means off". Zero has to mean "default" rather than "no limit"
// because an Options built without thinking about limits is the common case —
// every existing caller — and that case must come out capped, not uncapped.
func OrDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

// Limiter is a counting semaphore that reports exhaustion as a gRPC status.
//
// Deliberately a counter and not a buffered channel: a client that would exceed
// the cap must be REFUSED, not queued. Queueing turns a resource limit into
// unbounded latency, and the caller — a person waiting on a shell — cannot tell
// the two apart. "No" is an answer they can act on.
type Limiter struct {
	cur  atomic.Int64
	max  int64
	what string // named in the error, so the operator knows which knob to raise
	flag string // the flag that raises it

	// refused, when set, is told about every refusal — the metric that makes a
	// node running into its cap visible before an operator reports it.
	refused func()
}

// New returns a limiter of max slots. what and flag name the resource and the
// flag that raises the cap in the refusal; refused, when non-nil, is called on
// every refusal.
func New(max int, what, flag string, refused func()) *Limiter {
	return &Limiter{max: int64(max), what: what, flag: flag, refused: refused}
}

// Acquire takes a slot, returning the release function. When the cap is
// reached it returns a ResourceExhausted error — the code a client may
// reasonably retry on later, unlike the Unavailable used for draining.
//
// Zero or negative max disables the limit, matching how the timeout options
// already spell "off".
func (l *Limiter) Acquire() (func(), error) {
	if l == nil || l.max <= 0 {
		return func() {}, nil
	}
	if n := l.cur.Add(1); n > l.max {
		l.cur.Add(-1)
		if l.refused != nil {
			l.refused()
		}
		return nil, status.Errorf(codes.ResourceExhausted,
			"too many concurrent %s on this node (limit %d); retry shortly or raise %s",
			l.what, l.max, l.flag)
	}
	var once atomic.Bool
	return func() {
		// Release must be idempotent: the forward path hands it to a channel
		// whose Close can be reached both by the deferred cleanup and by the
		// demux goroutine noticing the far end went away.
		if once.CompareAndSwap(false, true) {
			l.cur.Add(-1)
		}
	}, nil
}

// InUse reports the current count, for tests and diagnostics.
func (l *Limiter) InUse() int64 {
	if l == nil {
		return 0
	}
	return l.cur.Load()
}

// Max reports the cap; zero or negative means no limit.
func (l *Limiter) Max() int64 {
	if l == nil {
		return 0
	}
	return l.max
}
