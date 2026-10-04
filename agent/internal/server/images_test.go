// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"

	"swarmexec/agent/internal/auth"

	"swarmexec/internal/pb"
)

// img builds an image summary as DiskUsage returns one: size is the image's
// total including shared layers, shared is how much of that it holds in common
// with others, and containers>0 means something is running from it.
func img(id string, size, shared int64, containers int64, tags ...string) image.Summary {
	return image.Summary{ID: id, Size: size, SharedSize: shared, Containers: containers, RepoTags: tags}
}

// du builds the images half of a DiskUsage result: total is the layer store's
// size with each layer counted once (TotalSize; LayersSize before API 1.52).
func du(total int64, images ...image.Summary) client.ImagesDiskUsage {
	return client.ImagesDiskUsage{TotalSize: total, Items: images}
}

// Docker's prune filter reads backwards from how it sounds: dangling=TRUE is
// the SAFE mode (untagged only) and dangling=FALSE is the sweep. Getting this
// the wrong way round would turn the safe button into the destructive one, so
// it is pinned here.
func TestPruneFiltersDirection(t *testing.T) {
	safe := pruneFilters(false)
	if got := filterValues(safe, "dangling"); len(got) != 1 || got[0] != "true" {
		t.Errorf("safe prune filter = %v, want dangling=true (untagged only)", got)
	}
	all := pruneFilters(true)
	if got := filterValues(all, "dangling"); len(got) != 1 || got[0] != "false" {
		t.Errorf("sweeping prune filter = %v, want dangling=false (every unused image)", got)
	}
}

// The two reclaimable totals are what an operator chooses between, so each has
// to count the right images and nothing else.
func TestImagesResponseTotals(t *testing.T) {
	// Shared layers are the trap: these five images sum to 450 bytes of Size but
	// only occupy 300 on disk, because most of that is the same base layers.
	resp := imagesResponse(du(300,
		img("sha256:used", 100, 60, 1, "app:1.2"),     // tagged, running   -> neither
		img("sha256:old", 200, 150, 0, "app:1.1"),     // tagged, unused    -> unused, 50 unique
		img("sha256:dang", 50, 20, 0),                 // untagged, unused  -> dangling, 30 unique
		img("sha256:dangused", 70, 10, 2),             // untagged, running -> neither
		img("sha256:none", 30, 5, 0, "<none>:<none>"), // placeholder tag   -> dangling, 25 unique
	))

	// The total must be what the layer store actually holds, NOT the sum of the
	// image sizes — that double-counts every shared layer.
	if resp.TotalBytes != 300 {
		t.Errorf("total = %d, want 300 (LayersSize, each layer counted once)", resp.TotalBytes)
	}
	// In-use images hold 40 + 60 = 100 bytes exclusively, so everything else —
	// 200 — is reclaimable. That is docker's formula, and it is larger than the
	// naive sum of the unused images' unique bytes (105), because it also counts
	// layers two UNUSED images share, which a prune does free.
	if got := resp.DanglingBytes + resp.UnusedBytes; got != 200 {
		t.Errorf("reclaimable = %d, want 200 (everything not held by a running image)", got)
	}
	// The untagged images' own unique bytes: 30 + 25. A lower bound by design.
	if resp.DanglingBytes != 55 {
		t.Errorf("dangling = %d, want 55 (the untagged images' unique bytes)", resp.DanglingBytes)
	}
	if resp.UnusedBytes != 145 {
		t.Errorf("unused = %d, want the remainder so the two add up exactly", resp.UnusedBytes)
	}

	byID := map[string]*pb.ImageInfo{}
	for _, i := range resp.Images {
		byID[i.GetId()] = i
	}
	// Docker's "<none>:<none>" placeholder is not a tag.
	if got := byID["sha256:none"]; len(got.GetTags()) != 0 || !got.GetDangling() {
		t.Errorf("placeholder-tagged image = %+v, want no tags and dangling", got)
	}
	// A dangling image can still be running — a container started before the tag
	// moved on — and then it is not reclaimable.
	if got := byID["sha256:dangused"]; !got.GetDangling() || !got.GetInUse() {
		t.Errorf("dangling-but-running image = %+v, want both flags", got)
	}
}

// If we cannot find out what is running, everything is reported as in use: it
// is better to offer no reclaimable space than to tell an operator an image is
// free when a container is running from it.
// Docker reports SharedSize as -1 when it has not worked it out. Treating that
// as zero would claim the image's whole size is reclaimable — an over-promise
// on a number someone deletes things by.
func TestImagesResponseHandlesUnknownSharedSize(t *testing.T) {
	// Nothing is running here, so the whole layer store is reclaimable whatever
	// the per-image numbers say — but none of it can be attributed to the
	// untagged bucket, whose figure comes from unique sizes we do not have.
	resp := imagesResponse(du(500, img("sha256:a", 100, -1, 0, "app:1"), img("sha256:b", 50, -1, 0)))
	if resp.DanglingBytes != 0 {
		t.Errorf("unknown shared size should promise nothing untagged, got %d", resp.DanglingBytes)
	}
	if resp.UnusedBytes != 500 {
		t.Errorf("unused = %d, want the whole layer store when nothing runs", resp.UnusedBytes)
	}
	// No per-image items (Items are values now, so there is no nil entry to
	// trip on) must still report the total and no images.
	if got := imagesResponse(du(10)); got.TotalBytes != 10 || len(got.Images) != 0 {
		t.Errorf("empty image list mishandled: %+v", got)
	}
	// Nonsense (shared larger than total) also yields nothing rather than a
	// negative that would subtract from the reclaimable figure.
	odd := imagesResponse(du(100, img("sha256:c", 10, 99, 0)))
	if odd.DanglingBytes != 0 {
		t.Errorf("shared > size should yield 0, got %d", odd.DanglingBytes)
	}

	// An in-use image holding more unique bytes than the store reports must not
	// produce a negative reclaimable figure.
	neg := imagesResponse(du(10, img("sha256:d", 100, 0, 1, "app:1")))
	if neg.DanglingBytes != 0 || neg.UnusedBytes != 0 {
		t.Errorf("reclaimable must not go negative: %+v", neg)
	}
}

func imageTestServer(d *fakeDocker) *Server {
	s, _ := newTestServer(d, auth.AllowAll{}, Options{})
	return s
}

// The safe mode must reach docker as the safe filter, and the sweep must be
// authorized under its own action so a policy can allow one without the other.
func TestPruneImagesModes(t *testing.T) {
	d := newFakeDocker()
	d.pruneReport = image.PruneReport{SpaceReclaimed: 1234, ImagesDeleted: []image.DeleteResponse{
		{Deleted: "sha256:gone"}, {Untagged: "app:old"},
	}}
	s := imageTestServer(d)
	ctx := context.Background()

	resp, err := s.PruneImages(ctx, &pb.PruneImagesRequest{})
	if err != nil {
		t.Fatalf("safe prune: %v", err)
	}
	if resp.GetReclaimedBytes() != 1234 || len(resp.GetDeleted()) != 2 {
		t.Errorf("response = %+v, want the reclaimed bytes and both removals", resp)
	}
	if got := filterValues(d.pruneFilters[0], "dangling"); len(got) != 1 || got[0] != "true" {
		t.Errorf("default prune sent dangling=%v, want the safe mode", got)
	}

	if _, err := s.PruneImages(ctx, &pb.PruneImagesRequest{All: true}); err != nil {
		t.Fatalf("sweeping prune: %v", err)
	}
	if got := filterValues(d.pruneFilters[1], "dangling"); len(got) != 1 || got[0] != "false" {
		t.Errorf("all-prune sent dangling=%v, want the sweep", got)
	}
}

// The destructive RPC must be refused when the policy says no, and must not
// reach docker at all.
func TestPruneImagesRequiresAuthorization(t *testing.T) {
	d := newFakeDocker()
	s, _ := newTestServer(d, denyAuth{}, Options{})
	if _, err := s.PruneImages(context.Background(), &pb.PruneImagesRequest{All: true}); err == nil {
		t.Fatal("a denied prune must return an error")
	}
	if len(d.pruneFilters) != 0 {
		t.Error("a denied prune must not reach docker")
	}
}

func TestListImagesMarksRunningImages(t *testing.T) {
	d := newFakeDocker()
	d.imageDiskUsage = du(100,
		img("sha256:a", 10, 0, 1, "app:1"), // running
		img("sha256:b", 20, 0, 0, "app:2"), // nothing runs it
	)
	s := imageTestServer(d)

	resp, err := s.ListImages(context.Background(), &pb.ListImagesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetTotalBytes() != 100 {
		t.Errorf("total = %d, want the layer-store size", resp.GetTotalBytes())
	}
	// The running image holds 10 bytes exclusively, so 90 is reclaimable.
	if got := resp.GetDanglingBytes() + resp.GetUnusedBytes(); got != 90 {
		t.Errorf("reclaimable = %d, want everything the running image does not hold", got)
	}
	var running, idle int
	for _, i := range resp.GetImages() {
		if i.GetInUse() {
			running++
		} else {
			idle++
		}
	}
	if running != 1 || idle != 1 {
		t.Errorf("in-use split = %d running / %d idle, want 1 each", running, idle)
	}
}
