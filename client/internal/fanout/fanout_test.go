// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package fanout

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"swarmexec/client/internal/resolve"
)

func TestEachVisitsEveryItemWithinTheLimit(t *testing.T) {
	items := make([]int, 50)
	for i := range items {
		items[i] = i * 10
	}
	out := make([]int, len(items))
	var running, peak atomic.Int32
	err := Each(context.Background(), items, 4, func(_ context.Context, i int, it int) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		out[i] = it + 1
		running.Add(-1)
	})
	if err != nil {
		t.Fatalf("Each: %v", err)
	}
	for i, v := range out {
		if v != items[i]+1 {
			t.Fatalf("item %d: got %d, want %d", i, v, items[i]+1)
		}
	}
	if p := peak.Load(); p > 4 {
		t.Fatalf("ran %d at once, limit is 4", p)
	}
}

// The point of taking a context: once it ends, nothing new is started and Each
// comes back as soon as the calls in flight — which see the same context —
// have returned, instead of working through the rest of the swarm.
func TestEachStopsStartingWorkOnceTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	items := make([]int, 40)
	var started atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- Each(ctx, items, 2, func(ctx context.Context, i int, _ int) {
			if started.Add(1) == 2 {
				cancel() // both slots taken: end the context from inside
			}
			<-ctx.Done() // an RPC honouring its context
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Each returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Each did not return after the context ended")
	}
	if n := started.Load(); n != 2 {
		t.Fatalf("started %d items, want only the 2 already running", n)
	}
}

func TestEachWithAnEndedContextStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := Nodes(ctx, []resolve.Node{{Name: "n1"}}, func(context.Context, int, resolve.Node) { called = true })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("err=%v called=%v, want context.Canceled and no call", err, called)
	}
}

// Every item started means every slot was written: the result is complete and
// must not be thrown away because the context ended during the last call.
func TestEachReportsSuccessOnceEverythingStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Each(ctx, []int{1, 2}, 0, func(_ context.Context, i int, _ int) {
		if i == 1 {
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("Each: %v, want nil after starting every item", err)
	}
}
