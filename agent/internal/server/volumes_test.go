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
