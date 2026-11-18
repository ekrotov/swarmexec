// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"swarmexec/client/internal/portfwd"
)

// The message has to name the cluster. Since forwards survive a cluster switch,
// the thing holding port 8080 is often something the operator started somewhere
// they are not currently looking — and "address already in use" never says so.
func TestPortConflictNamesTheClusterAndService(t *testing.T) {
	u := &ui{forwards: portfwd.NewRegistry()}
	e := u.forwards.Add("prod", cand("abc123456789", "api", "node-1"), 8080, 80, func() {})
	u.forwards.MarkActive(e.ID, "127.0.0.1:8080")

	msg, clash := u.portConflict(8080)
	if !clash {
		t.Fatal("an active forward on that port must be reported as a conflict")
	}
	for _, want := range []string{"8080", "api", "prod"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

// The tree row carries one marker per forward on that container. The markers
// themselves are portfwd's (Entry.Label); joining them onto a row is this
// package's, and an empty list must leave the row exactly as it was.
func TestAnnotateForwards(t *testing.T) {
	base := "abc123  node-1  up 3h"
	if got := annotateForwards(base, nil); got != base {
		t.Errorf("no forwards should leave the label untouched, got %q", got)
	}

	active := portfwd.Entry{State: portfwd.StateActive, LocalAddr: "127.0.0.1:9090", Remote: 8080}
	starting := portfwd.Entry{State: portfwd.StateStarting, Local: 6000, Remote: 5432}
	failed := portfwd.Entry{State: portfwd.StateFailed, Local: 7070, Remote: 443}

	got := annotateForwards(base, []portfwd.Entry{active, starting, failed})
	for _, want := range []string{base, "9090→8080", "…→5432", "✗→443"} {
		if !strings.Contains(got, want) {
			t.Errorf("annotation %q missing %q", got, want)
		}
	}
}
