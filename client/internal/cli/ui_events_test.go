// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
)

// fastTopology runs the coalescing in milliseconds. The ratio matters more than
// the absolute values: debounce well under minInterval, both well under the
// test's patience.
var fastTopology = topologyTiming{debounce: 10 * time.Millisecond, minInterval: 60 * time.Millisecond}

func svcEvent() events.Message {
	return events.Message{Type: events.ServiceEventType, Action: "update"}
}

// waitFor polls until cond holds or the deadline passes, so the tests assert on
// an outcome rather than on a sleep long enough to hide a race.
func waitFor(t *testing.T, what string, cond func() bool) {
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

// A rolling update emits an event per task state transition. Without
// coalescing, a ten-replica service would turn one deployment into dozens of
// TaskList calls — trading the poll's latency for a load spike, which is not
// the trade this feature is making.
func TestConsumeTopologyCoalescesABurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgs := make(chan events.Message)
	errs := make(chan error)
	var refreshes atomic.Int64

	go consumeTopology(ctx, msgs, errs, fastTopology, nil, func() { refreshes.Add(1) })

	for i := 0; i < 20; i++ {
		msgs <- svcEvent()
	}
	waitFor(t, "the burst to produce a refresh", func() bool { return refreshes.Load() >= 1 })

	// Nothing further may arrive from those same twenty events.
	time.Sleep(4 * fastTopology.debounce)
	if got := refreshes.Load(); got != 1 {
		t.Errorf("20 events in a burst produced %d refreshes, want 1", got)
	}
}

// …and a later, separate change must still get its own refresh. Coalescing
// that swallowed the second one would leave the tree stale until the poll —
// the failure this feature exists to remove.
func TestConsumeTopologyStillRefreshesAfterTheBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgs := make(chan events.Message)
	errs := make(chan error)
	var refreshes atomic.Int64

	go consumeTopology(ctx, msgs, errs, fastTopology, nil, func() { refreshes.Add(1) })

	msgs <- svcEvent()
	waitFor(t, "the first refresh", func() bool { return refreshes.Load() == 1 })

	msgs <- svcEvent()
	waitFor(t, "the second refresh", func() bool { return refreshes.Load() == 2 })
}

// The minimum interval DEFERS rather than drops. An event arriving immediately
// after a refresh must still produce one — late is acceptable, lost is not.
func TestConsumeTopologyDefersRatherThanDropping(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msgs := make(chan events.Message)
	errs := make(chan error)
	var refreshes atomic.Int64

	go consumeTopology(ctx, msgs, errs, fastTopology, nil, func() { refreshes.Add(1) })

	msgs <- svcEvent()
	waitFor(t, "the first refresh", func() bool { return refreshes.Load() == 1 })

	// Straight away, inside the floor.
	msgs <- svcEvent()
	start := time.Now()
	waitFor(t, "the deferred refresh", func() bool { return refreshes.Load() == 2 })

	// It must have waited out the floor rather than fired immediately.
	if waited := time.Since(start); waited < fastTopology.minInterval/2 {
		t.Errorf("second refresh came after %v, expected it to wait out the %v floor",
			waited, fastTopology.minInterval)
	}
}

// A stream that ends must be reported as having delivered, so the reconnect
// backoff can tell a transient drop from an endpoint that never works.
func TestConsumeTopologyReportsWhetherItDelivered(t *testing.T) {
	run := func(send bool) bool {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		msgs := make(chan events.Message, 1)
		errs := make(chan error, 1)
		if send {
			msgs <- svcEvent()
		}
		done := make(chan bool, 1)
		go func() { done <- consumeTopology(ctx, msgs, errs, fastTopology, nil, func() {}) }()
		// Let a queued event be consumed before the stream ends.
		time.Sleep(2 * fastTopology.debounce)
		errs <- errors.New("connection reset")
		select {
		case got := <-done:
			return got
		case <-time.After(2 * time.Second):
			t.Fatal("consumeTopology did not return after the stream ended")
			return false
		}
	}

	if !run(true) {
		t.Error("a stream that carried an event must report delivered=true")
	}
	if run(false) {
		t.Error("a stream that carried nothing must report delivered=false")
	}
}

// Switching cluster cancels that cluster's context; the watcher has to let go
// promptly, or every visited cluster leaves a goroutine holding a connection.
func TestConsumeTopologyStopsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	msgs := make(chan events.Message)
	errs := make(chan error)

	done := make(chan bool, 1)
	go func() { done <- consumeTopology(ctx, msgs, errs, fastTopology, nil, func() {}) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consumeTopology ignored its cancelled context")
	}
}

// A closed stream channel is an ordinary end, not a reason to spin.
func TestConsumeTopologyHandlesAClosedChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	msgs := make(chan events.Message)
	errs := make(chan error)

	done := make(chan bool, 1)
	go func() { done <- consumeTopology(ctx, msgs, errs, fastTopology, nil, func() {}) }()

	close(msgs)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consumeTopology did not return when the stream closed")
	}
}

// The production values have to keep the relationship the design depends on:
// coalesce quickly, then rate-limit over a longer window.
func TestTopologyTimingIsCoherent(t *testing.T) {
	if defaultTopologyTiming.debounce >= defaultTopologyTiming.minInterval {
		t.Errorf("debounce %v must be shorter than the floor %v",
			defaultTopologyTiming.debounce, defaultTopologyTiming.minInterval)
	}
	// The whole point is being faster than the poll it supplements.
	if defaultTopologyTiming.minInterval >= treeRefreshFast {
		t.Errorf("floor %v is not an improvement over the %v poll",
			defaultTopologyTiming.minInterval, treeRefreshFast)
	}
}

// The saving has to be conditional on the thing that earns it. A cluster whose
// event stream never works — an old daemon, a blocked socket — must keep the
// fast poll, or this change is a pure regression there.
func TestPollIntervalFollowsTheEventStream(t *testing.T) {
	u := &ui{}
	if got := u.pollInterval(); got != treeRefreshFast {
		t.Errorf("without events the poll must stay fast, got %v", got)
	}
	u.eventsLive.Store(true)
	if got := u.pollInterval(); got != treeRefreshSlow {
		t.Errorf("with events live the poll should slow down, got %v", got)
	}
	u.eventsLive.Store(false)
	if got := u.pollInterval(); got != treeRefreshFast {
		t.Errorf("a lost stream must restore the fast poll, got %v", got)
	}
}

// The rates only make sense in one order, and the slow one must still be a net
// under the event stream rather than a replacement for it.
func TestPollRatesAreCoherent(t *testing.T) {
	if treeRefreshSlow <= treeRefreshFast {
		t.Fatalf("slow (%v) must be slower than fast (%v)", treeRefreshSlow, treeRefreshFast)
	}
	// Liveness is re-proven on this cadence, so a silent death cannot leave the
	// poll slow for longer than one window.
	if topologyResubscribe < treeRefreshSlow {
		t.Errorf("resubscribe (%v) shorter than the slow poll (%v) would re-dial for nothing",
			topologyResubscribe, treeRefreshSlow)
	}
}
