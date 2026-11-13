// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync/atomic"
	"testing"
	"time"
)

// fakeStopper's GracefulStop returns at once, or — when hang is set — only
// once Stop has been called, like a grpc.Server with a client that never
// hangs up.
type fakeStopper struct {
	hang    bool
	stopped chan struct{}
	stops   atomic.Int32
}

func newFakeStopper(hang bool) *fakeStopper {
	return &fakeStopper{hang: hang, stopped: make(chan struct{})}
}

func (f *fakeStopper) GracefulStop() {
	if f.hang {
		<-f.stopped
	}
}

func (f *fakeStopper) Stop() {
	if f.stops.Add(1) == 1 {
		close(f.stopped)
	}
}

func TestStopGracefullyFinishesInTime(t *testing.T) {
	f := newFakeStopper(false)
	if !stopGracefully(f, time.Second) {
		t.Fatal("a graceful stop that returned at once was reported as timed out")
	}
	if f.stops.Load() != 0 {
		t.Error("Stop was called although GracefulStop finished")
	}
}

// The graceful path must not wait without limit: after the timeout Stop is
// forced, and stopGracefully returns instead of hanging until SIGKILL.
func TestStopGracefullyFallsBackToStop(t *testing.T) {
	f := newFakeStopper(true)
	start := time.Now()
	if stopGracefully(f, 50*time.Millisecond) {
		t.Fatal("a hanging graceful stop was reported as finished")
	}
	if f.stops.Load() != 1 {
		t.Errorf("Stop called %d times, want 1", f.stops.Load())
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("stopGracefully took %v", el)
	}
}
