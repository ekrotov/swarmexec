// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

func TestListVolumes(t *testing.T) {
	d := newFakeDocker()
	d.volumes = []*volume.Volume{
		{Name: "v1", Driver: "local", Mountpoint: "/m1", Scope: "local"},
		{Name: "v2", Driver: "local"},
		nil, // tolerated
	}
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	resp, err := srv.ListVolumes(context.Background(), &pb.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Volumes) != 2 {
		t.Fatalf("want 2 volumes, got %d", len(resp.Volumes))
	}
	if resp.Volumes[0].Name != "v1" || resp.Volumes[0].Mountpoint != "/m1" {
		t.Errorf("first volume wrong: %+v", resp.Volumes[0])
	}
}

func TestListVolumes_WithSize(t *testing.T) {
	d := newFakeDocker()
	d.volumes = []*volume.Volume{
		{Name: "v1", Driver: "local", UsageData: &volume.UsageData{Size: 4096, RefCount: 1}},
		{Name: "v2", Driver: "other", UsageData: &volume.UsageData{Size: -1, RefCount: -1}},
	}
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})

	// Without with_size, the agent must not compute sizes: SizeBytes stays -1.
	resp, err := srv.ListVolumes(context.Background(), &pb.ListVolumesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Volumes[0].SizeBytes != -1 || resp.Volumes[0].SizeKnown {
		t.Errorf("without with_size: got size=%d known=%v, want -1/false", resp.Volumes[0].SizeBytes, resp.Volumes[0].SizeKnown)
	}

	// With with_size, sizes come from the background cache (-1 for non-local) and
	// size_known is set so the client can tell "0 bytes" from "not reported".
	// Warm the cache directly instead of starting the background loop.
	srv.sizeCache.refresh(context.Background())
	resp, err = srv.ListVolumes(context.Background(), &pb.ListVolumesRequest{WithSize: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, v := range resp.Volumes {
		got[v.Name] = v.SizeBytes
		if !v.SizeKnown {
			t.Errorf("%s: size_known = false, want true", v.Name)
		}
	}
	if got["v1"] != 4096 {
		t.Errorf("v1 size = %d, want 4096", got["v1"])
	}
	if got["v2"] != -1 {
		t.Errorf("v2 size = %d, want -1 (n/a)", got["v2"])
	}
}

func TestListVolumes_Denied(t *testing.T) {
	srv, m := newTestServer(newFakeDocker(), denyAuth{}, Options{})
	if _, err := srv.ListVolumes(context.Background(), &pb.ListVolumesRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if m.denied.Load() != 1 {
		t.Errorf("AuthDenied = %d, want 1", m.denied.Load())
	}
}

func TestRemoveVolume(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	if _, err := srv.RemoveVolume(context.Background(), &pb.RemoveVolumeRequest{Name: "v1"}); err != nil {
		t.Fatal(err)
	}
	if len(d.removedVols) != 1 || d.removedVols[0] != "v1" {
		t.Errorf("removedVols = %v, want [v1]", d.removedVols)
	}
}

func TestRemoveVolume_RequiresName(t *testing.T) {
	srv, _ := newTestServer(newFakeDocker(), auth.AllowAll{}, Options{})
	if _, err := srv.RemoveVolume(context.Background(), &pb.RemoveVolumeRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestRemoveVolume_InUse(t *testing.T) {
	d := newFakeDocker()
	d.volumeRemErr = errors.New("Error response from daemon: remove v1: volume is in use - [abc]")
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	_, err := srv.RemoveVolume(context.Background(), &pb.RemoveVolumeRequest{Name: "v1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for in-use, got %v", err)
	}
}

func TestRemoveVolume_Denied(t *testing.T) {
	srv, m := newTestServer(newFakeDocker(), denyAuth{}, Options{})
	if _, err := srv.RemoveVolume(context.Background(), &pb.RemoveVolumeRequest{Name: "v1"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if m.denied.Load() != 1 {
		t.Errorf("AuthDenied = %d, want 1", m.denied.Load())
	}
}
