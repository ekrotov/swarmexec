// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package prunecat

import (
	"slices"
	"testing"

	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// The table must hold every wire category and every schedule-label name, each
// exactly once and in the same order prunesched lists the names. A category
// added to the proto or to the label vocabulary without a row here would be
// unprunable on the agent — refused as "unknown" — with nothing else noticing.
func TestOrderCoversEveryCategoryOnce(t *testing.T) {
	var names []string
	seen := map[pb.PruneCategory]bool{}
	for _, pc := range Order {
		if seen[pc.Cat] {
			t.Errorf("%v listed twice", pc.Cat)
		}
		seen[pc.Cat] = true
		names = append(names, pc.Name)
		if pc.Action == "" {
			t.Errorf("%v has no authz action", pc.Cat)
		}
	}
	for v := range pb.PruneCategory_name {
		c := pb.PruneCategory(v)
		if c != pb.PruneCategory_PRUNE_CATEGORY_UNSPECIFIED && !seen[c] {
			t.Errorf("wire category %v missing from Order", c)
		}
	}
	if !slices.Equal(names, prunesched.Categories) {
		t.Errorf("names = %v, want prunesched.Categories %v", names, prunesched.Categories)
	}
}

func TestFromNamesDedupesAndRejectsUnknown(t *testing.T) {
	got, err := FromNames([]string{prunesched.Networks, prunesched.Containers, prunesched.Networks})
	if err != nil {
		t.Fatal(err)
	}
	want := []pb.PruneCategory{pb.PruneCategory_PRUNE_CATEGORY_NETWORKS, pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS}
	if !slices.Equal(got, want) {
		t.Errorf("FromNames = %v, want %v", got, want)
	}
	if Names(got) != "networks,containers" {
		t.Errorf("Names = %q", Names(got))
	}
	if _, err := FromNames([]string{"everything"}); err == nil {
		t.Error("an unknown name must be an error")
	}
	if _, ok := Info(pb.PruneCategory_PRUNE_CATEGORY_UNSPECIFIED); ok {
		t.Error("UNSPECIFIED is not a category")
	}
}

func TestSortReclaimFollowsOrder(t *testing.T) {
	in := []*pb.ReclaimCategory{
		{Category: pb.PruneCategory_PRUNE_CATEGORY_VOLUMES},
		{Category: pb.PruneCategory_PRUNE_CATEGORY_UNSPECIFIED},
		{Category: pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS},
	}
	out := SortReclaim(in)
	if len(out) != 2 || out[0].GetCategory() != pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS ||
		out[1].GetCategory() != pb.PruneCategory_PRUNE_CATEGORY_VOLUMES {
		t.Errorf("SortReclaim = %v", out)
	}
}
