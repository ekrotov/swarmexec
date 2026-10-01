// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"swarmexec/client/internal/resolve"
)

// A stack view follows every container of the stack's services — taken from
// the last fetch, so a folded service counts as much as an open one — and
// nothing from another stack or from the unstacked bucket.
func TestStackLogMembers(t *testing.T) {
	u := &ui{clusterState: &clusterState{
		lastSvcs: []resolve.Service{
			{Name: "shop_web", Stack: "shop"}, {Name: "shop_db", Stack: "shop"},
			{Name: "other_api", Stack: "other"}, {Name: "loose"},
		},
		lastCands: []resolve.Candidate{
			{Service: "shop_web", Slot: 2, ContainerID: "w2"},
			{Service: "other_api", Slot: 1, ContainerID: "o1"},
			{Service: "shop_db", Slot: 1, ContainerID: "d1"},
			{Service: "shop_web", Slot: 1, ContainerID: "w1"},
			{Service: "loose", Slot: 1, ContainerID: "l1"},
		},
	}}
	var got []string
	for _, c := range u.stackLogMembers("shop") {
		got = append(got, sourceFromCandidate(c).label)
	}
	if strings.Join(got, " ") != "shop_db.1 shop_web.1 shop_web.2" {
		t.Errorf("members = %v", got)
	}
	if n := len(u.stackLogMembers(noStackLabel)); n != 1 {
		t.Errorf("the unstacked bucket resolves to its own services (%d), but the view refuses it before streaming", n)
	}
}
