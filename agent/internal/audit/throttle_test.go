// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"encoding/json"
	"testing"
	"time"
)

// A polled allow is written at once, held back for a minute, and the next one
// says how many it stands for; a denial of the same action is never held back.
func TestAuthDecision_PolledAllowThrottle(t *testing.T) {
	l, buf := newCapture()
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	poll := AuthRecord{Identity: "a", Action: "stats.read", Allow: true, Polled: true}
	for i := 0; i < 4; i++ {
		l.AuthDecision(poll)
	}
	deny := poll
	deny.Allow = false
	l.AuthDecision(deny)
	l.AuthDecision(deny)
	now = now.Add(pollInterval)
	l.AuthDecision(poll)

	var recs []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	if len(recs) != 4 {
		t.Fatalf("%d records, want 4 (first allow, two denials, allow after the interval): %v", len(recs), recs)
	}
	if _, ok := recs[0]["suppressed"]; ok {
		t.Errorf("first record claims suppression: %v", recs[0])
	}
	if recs[3]["suppressed"] != float64(3) {
		t.Errorf("record after the interval: suppressed = %v, want 3", recs[3]["suppressed"])
	}
}
