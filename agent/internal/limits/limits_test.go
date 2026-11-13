// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package limits

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLimiter_RefusesBeyondTheCapAndReleases(t *testing.T) {
	l := New(2, "things", "-max-things", nil)

	r1, err := l.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := l.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("third acquire: want ResourceExhausted, got %v", err)
	}
	// The error has to tell the operator which knob to turn; a bare "limit
	// reached" leaves them guessing at a value they own.
	if _, err := l.Acquire(); err == nil || !strings.Contains(err.Error(), "-max-things") {
		t.Errorf("error should name the flag: %v", err)
	}

	// A released slot is reusable: the cap is "not now", not a permanent
	// degradation of the node.
	r1()
	r3, err := l.Acquire()
	if err != nil {
		t.Fatalf("a released slot must be reusable: %v", err)
	}
	if got := l.InUse(); got != 2 {
		t.Fatalf("in use = %d, want 2", got)
	}

	// A rejected acquire must not leak a slot: the counter is incremented
	// speculatively and has to be given back on the failing path. Fifty
	// rejections must leave the count exactly where it was.
	for i := 0; i < 50; i++ {
		if _, err := l.Acquire(); err == nil {
			t.Fatal("expected the cap to hold")
		}
	}
	if got := l.InUse(); got != 2 {
		t.Errorf("rejections leaked slots: in use = %d, want 2", got)
	}

	// Release is idempotent — the forward path can reach it twice.
	r3()
	r3()
	if got := l.InUse(); got != 1 {
		t.Errorf("double release freed a slot that was not held: in use = %d, want 1", got)
	}
	r2()
}

func TestLimiter_ZeroOrNegativeMeansUnlimited(t *testing.T) {
	for _, max := range []int{0, -1} {
		l := New(max, "things", "-max-things", nil)
		for i := 0; i < 1000; i++ {
			if _, err := l.Acquire(); err != nil {
				t.Fatalf("max=%d: acquire %d refused: %v", max, i, err)
			}
		}
	}
	// A nil limiter is also "no limit", so a zero-value Server in a test cannot
	// panic on the hot path.
	var nilLim *Limiter
	if _, err := nilLim.Acquire(); err != nil {
		t.Errorf("nil limiter must not refuse: %v", err)
	}
}

// An Options built without an opinion about limits — which is every existing

// Every refusal is reported, and only refusals: the metric behind it must not
// count admitted callers or released slots.
func TestLimiter_ReportsEachRefusal(t *testing.T) {
	refused := 0
	l := New(1, "things", "-max-things", func() { refused++ })
	release, err := l.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := l.Acquire(); err == nil {
			t.Fatal("expected the cap to hold")
		}
	}
	release()
	if refused != 3 {
		t.Errorf("refused called %d times, want 3", refused)
	}
	if l.Max() != 1 {
		t.Errorf("Max = %d, want 1", l.Max())
	}
}

func TestOrDefault(t *testing.T) {
	for _, c := range []struct{ v, want int }{{0, 7}, {-1, -1}, {3, 3}} {
		if got := OrDefault(c.v, 7); got != c.want {
			t.Errorf("OrDefault(%d, 7) = %d, want %d", c.v, got, c.want)
		}
	}
}
