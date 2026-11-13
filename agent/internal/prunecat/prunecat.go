// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package prunecat is the agent's table of prune categories: each wire
// category with its schedule-label name and its authorization action, in the
// order a prune runs them. The Prune handler, the reclaim measurement and the
// prune scheduler all read it, so the three cannot disagree about what a
// category is called, who may prune it, or the order the figures come in.
package prunecat

import (
	"errors"
	"slices"
	"strings"

	"swarmexec/internal/pb"
	"swarmexec/internal/prunesched"
)

// Category ties a wire category to its label name and authz action.
type Category struct {
	Cat    pb.PruneCategory
	Name   string
	Action string
}

// Order is every category in the order a prune runs them, whatever order
// a request lists them in: containers first, because removing them is what
// leaves images, networks and volumes unused.
var Order = []Category{
	{pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS, prunesched.Containers, "container.prune"},
	{pb.PruneCategory_PRUNE_CATEGORY_IMAGES, prunesched.Images, "image.prune.all"},
	{pb.PruneCategory_PRUNE_CATEGORY_DANGLING_IMAGES, prunesched.DanglingImages, "image.prune"},
	{pb.PruneCategory_PRUNE_CATEGORY_BUILD_CACHE, prunesched.BuildCache, "buildcache.prune"},
	{pb.PruneCategory_PRUNE_CATEGORY_NETWORKS, prunesched.Networks, "network.prune"},
	{pb.PruneCategory_PRUNE_CATEGORY_VOLUMES, prunesched.Volumes, "volume.prune"},
}

// Info returns the table entry of a wire category.
func Info(c pb.PruneCategory) (Category, bool) {
	for _, pc := range Order {
		if pc.Cat == c {
			return pc, true
		}
	}
	return Category{}, false
}

// ByName maps a label name ("buildcache") to its wire category.
func ByName(name string) (pb.PruneCategory, bool) {
	for _, pc := range Order {
		if pc.Name == name {
			return pc.Cat, true
		}
	}
	return 0, false
}

// Names renders categories for logs.
func Names(cats []pb.PruneCategory) string {
	var names []string
	for _, c := range cats {
		if pc, ok := Info(c); ok {
			names = append(names, pc.Name)
		}
	}
	return strings.Join(names, ",")
}

// FromNames maps schedule label names to wire categories.
func FromNames(names []string) ([]pb.PruneCategory, error) {
	var out []pb.PruneCategory
	for _, n := range names {
		c, ok := ByName(n)
		if !ok {
			return nil, errors.New("unknown category " + n)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

// SortReclaim returns the reclaim figures in Order, one per category; a
// category not in Order is dropped.
func SortReclaim(in []*pb.ReclaimCategory) []*pb.ReclaimCategory {
	var out []*pb.ReclaimCategory
	for _, pc := range Order {
		for _, c := range in {
			if c.GetCategory() == pc.Cat {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
