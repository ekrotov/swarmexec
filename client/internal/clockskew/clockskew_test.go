// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package clockskew

import (
	"strings"
	"testing"
	"time"
)

var base = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) string { return base.Add(d).Format(time.RFC3339Nano) }

// An agent that answers with our own time, over an instant round trip, is the
// zero case: known, no offset, no slack.
func TestMeasureAgreeingClock(t *testing.T) {
	r := Measure(base, base, at(0))
	if !r.Known || r.Offset != 0 || r.Slack != 0 {
		t.Fatalf("got %+v", r)
	}
	if r.Beyond(Warn) {
		t.Error("an agreeing clock is beyond the threshold")
	}
	if r.String() != "+0s" {
		t.Errorf("String() = %q", r.String())
	}
}

// The offset is measured against the MIDPOINT of the round trip, not its
// start: a reply that took a second to arrive is not a second of drift.
func TestMeasureUsesTheMidpoint(t *testing.T) {
	// Sent at 0, answered at +1s, agent read its clock at +500ms — exactly
	// what a correct clock on a 1s link looks like.
	r := Measure(base, base.Add(time.Second), at(500*time.Millisecond))
	if r.Offset != 0 {
		t.Errorf("Offset = %v, want 0", r.Offset)
	}
	if r.Slack != 500*time.Millisecond {
		t.Errorf("Slack = %v, want 500ms", r.Slack)
	}
}

// Slack is subtracted before the comparison, so drift the round trip alone
// could explain is never reported.
func TestBeyondSubtractsTheRoundTrip(t *testing.T) {
	// A 6 s round trip (slack 3 s) and an apparent 4 s offset: 4-3 = 1 s,
	// under the 2 s threshold, so this says nothing.
	noisy := Measure(base, base.Add(6*time.Second), at(7*time.Second))
	if noisy.Offset != 4*time.Second {
		t.Fatalf("Offset = %v", noisy.Offset)
	}
	if noisy.Beyond(Warn) {
		t.Error("drift inside the round trip was reported")
	}
	// The same apparent offset on a fast link is real.
	clear := Measure(base, base.Add(10*time.Millisecond), at(4*time.Second))
	if !clear.Beyond(Warn) {
		t.Error("real drift on a fast link was not reported")
	}
}

// An agent too old to send a time, or one that sends nonsense, is unknown —
// never "no drift". This is the whole reason Known exists.
func TestMeasureUnknown(t *testing.T) {
	for _, now := range []string{"", "not a time", "12:00:00"} {
		r := Measure(base, base, now)
		if r.Known {
			t.Errorf("Measure(%q) claimed to know something", now)
		}
		if r.Beyond(0) {
			t.Errorf("Measure(%q) is beyond a zero threshold", now)
		}
		if r.String() != "" || r.Summary() != "" {
			t.Errorf("Measure(%q) rendered %q / %q, want empty", now, r.String(), r.Summary())
		}
	}
}

// The sign says which way, because the two directions have different causes.
func TestStringAndSummaryCarryTheDirection(t *testing.T) {
	ahead := Measure(base, base, at(3*time.Second))
	if got := ahead.String(); got != "+3s" {
		t.Errorf("ahead String() = %q", got)
	}
	if s := ahead.Summary(); !strings.Contains(s, "ahead of") {
		t.Errorf("ahead Summary() = %q", s)
	}
	behind := Measure(base, base, at(-3*time.Second))
	if got := behind.String(); got != "-3s" {
		t.Errorf("behind String() = %q", got)
	}
	if s := behind.Summary(); !strings.Contains(s, "behind") {
		t.Errorf("behind Summary() = %q", s)
	}
}

// A reply stamped before the request was sent means a clock stepped during the
// call. The midpoint is still the best estimate available; it must not panic
// or produce a negative slack.
func TestMeasureNegativeRoundTrip(t *testing.T) {
	r := Measure(base.Add(time.Second), base, at(time.Second))
	if !r.Known || r.Slack < 0 {
		t.Fatalf("got %+v", r)
	}
}
