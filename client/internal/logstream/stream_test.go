// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logstream

import "testing"

func TestFollowTargetFromTarget(t *testing.T) {
	cases := []struct {
		in      string
		service string
		slot    int
	}{
		{"web", "web", 0},
		{"web.3", "web", 3},
		{" web.3 ", "web", 3},
		{"a.b.2", "a.b", 2},                 // only the trailing numeric segment is a slot
		{"1a2b3c4d5e6f", "1a2b3c4d5e6f", 0}, // a container id: no slot, resolves to nothing later
	}
	for _, c := range cases {
		ft := FollowTargetOf(c.in)
		if ft.Service != c.service || ft.Slot != c.slot {
			t.Errorf("followTargetFromTarget(%q) = {%q,%d}, want {%q,%d}", c.in, ft.Service, ft.Slot, c.service, c.slot)
		}
	}
}
