// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errhttp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// The table of CONTRACT.md §3.8. Each class is built the way the moby client
// builds it — from the Engine's HTTP status (errhttp.ToNative) — or as the
// Engine's own typed error, wrapped once more as handlers do.
func TestDockerStatus(t *testing.T) {
	engine := func(code int, msg string) error {
		return fmt.Errorf("Error response from daemon: %s: %w", msg, errhttp.ToNative(code))
	}
	for _, tc := range []struct {
		name string
		err  error
		want codes.Code
	}{
		{"404 no such container", engine(404, "No such container: abc"), codes.NotFound},
		{"typed NotFound", cerrdefs.ErrNotFound.WithMessage("no such volume"), codes.NotFound},
		{"409 conflict (volume in use, container not running)", engine(409, "volume is in use"), codes.FailedPrecondition},
		{"412 precondition", engine(412, "x"), codes.FailedPrecondition},
		{"304 not modified", engine(304, "already stopped"), codes.FailedPrecondition},
		{"already exists", cerrdefs.ErrAlreadyExists.WithMessage("volume exists"), codes.AlreadyExists},
		{"400 invalid parameter", engine(400, "invalid reference format"), codes.InvalidArgument},
		{"401 to the agent is not the client's Unauthenticated", engine(401, "unauthorized"), codes.PermissionDenied},
		{"403 forbidden", engine(403, "forbidden"), codes.PermissionDenied},
		{"429 too many requests", engine(429, "slow down"), codes.ResourceExhausted},
		{"503 daemon unavailable", engine(503, "shutting down"), codes.Unavailable},
		{"501 is not the client's Unimplemented", engine(501, "not implemented"), codes.Internal},
		{"500", engine(500, "boom"), codes.Internal},
		{"plain error", errors.New("something odd"), codes.Internal},
		{"context cancelled", fmt.Errorf("read: %w", context.Canceled), codes.Canceled},
		{"deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), codes.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := dockerStatus(tc.err, "do thing")
			if c := status.Code(got); c != tc.want {
				t.Fatalf("code = %v, want %v (%v)", c, tc.want, got)
			}
			if want := "do thing: " + tc.err.Error(); status.Convert(got).Message() != want {
				t.Fatalf("message = %q, want %q", status.Convert(got).Message(), want)
			}
		})
	}
}

func TestDockerStatus_KeepsAChosenCodeAndNil(t *testing.T) {
	if dockerStatus(nil, "x") != nil {
		t.Fatal("nil must stay nil")
	}
	capped := status.Error(codes.ResourceExhausted, "too many sidecars")
	if got := dockerStatus(capped, "forward"); got != capped {
		t.Fatalf("a status the setup chose was rewritten: %v", got)
	}
	if got := dockerStatus(errors.New("x"), ""); status.Convert(got).Message() != "x" {
		t.Fatalf("empty verb must not prefix: %v", got)
	}
}

// Exec into a stopped container is the operator's state problem, not a broken
// agent: FailedPrecondition, with the same in-band text as before.
func TestExec_CreateFailureIsClassified(t *testing.T) {
	d := newFakeDocker()
	d.createErr = cerrdefs.ErrConflict.WithMessage("container abc is not running")
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, false))

	err := srv.Exec(stream)
	if statusCode(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	var inband string
	for _, m := range stream.sentMessages() {
		if e, ok := m.Payload.(*pb.ServerMessage_Error); ok {
			inband = e.Error
		}
	}
	if want := "exec create failed: container abc is not running"; inband != want {
		t.Fatalf("in-band error = %q, want %q", inband, want)
	}
}

func TestListContainers_DaemonUnavailable(t *testing.T) {
	d := newFakeDocker()
	d.listErr = fmt.Errorf("list: %w", errhttp.ToNative(503))
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	if _, err := srv.ListContainers(context.Background(), &pb.ListRequest{}); statusCode(err) != codes.Unavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
}

func TestRemoveVolume_NotFound(t *testing.T) {
	d := newFakeDocker()
	d.volumeRemErr = cerrdefs.ErrNotFound.WithMessage("get v1: no such volume")
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	_, err := srv.RemoveVolume(context.Background(), &pb.RemoveVolumeRequest{Name: "v1"})
	if statusCode(err) != codes.NotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
}
