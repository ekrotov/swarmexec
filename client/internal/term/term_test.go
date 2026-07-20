// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package term

import (
	"sync"
	"testing"
)

func TestRestorerRunsOnce(t *testing.T) {
	var n int
	r := newRestorer(func() { n++ })
	r.Restore()
	r.Restore()
	r.Restore()
	if n != 1 {
		t.Fatalf("restore ran %d times, want 1", n)
	}
}

func TestRestorerConcurrent(t *testing.T) {
	var n int
	var mu sync.Mutex
	r := newRestorer(func() { mu.Lock(); n++; mu.Unlock() })

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Restore() }()
	}
	wg.Wait()
	if n != 1 {
		t.Fatalf("restore ran %d times under concurrency, want 1", n)
	}
}

// TestRestoreOnPanic models the exec path: a deferred Restore must run when the
// session panics, leaving the terminal cooked (REQUIREMENTS §5).
func TestRestoreOnPanic(t *testing.T) {
	var restored bool
	r := newRestorer(func() { restored = true })

	func() {
		defer func() { _ = recover() }()
		defer r.Restore()
		panic("boom")
	}()

	if !restored {
		t.Fatal("terminal was not restored on panic")
	}
}

// TestNilRestorer ensures the non-interactive path (nil Restorer) is a no-op.
func TestNilRestorer(t *testing.T) {
	var r *Restorer
	r.Restore() // must not panic
}
