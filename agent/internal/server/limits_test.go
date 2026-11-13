// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"google.golang.org/grpc/codes"

	"swarmexec/agent/internal/auth"
	"swarmexec/agent/internal/limits"
	"swarmexec/internal/pb"
)

// caller — must come out capped rather than uncapped.
func TestNew_AppliesDefaultCaps(t *testing.T) {
	s, _ := newTestServer(newFakeDocker(), auth.AllowAll{}, Options{})
	if s.streams.Max() != limits.DefaultMaxStreams {
		t.Errorf("stream cap = %d, want %d", s.streams.Max(), limits.DefaultMaxStreams)
	}
	if s.sidecars.Max() != limits.DefaultMaxForwardSidecars {
		t.Errorf("sidecar cap = %d, want %d", s.sidecars.Max(), limits.DefaultMaxForwardSidecars)
	}
	// The sidecar budget must be the tighter of the two: reaching it first is
	// what keeps the refusal ahead of the containers being created.
	if s.sidecars.Max() >= s.streams.Max() {
		t.Errorf("sidecar cap (%d) should be below the stream cap (%d)", s.sidecars.Max(), s.streams.Max())
	}

	off, _ := newTestServer(newFakeDocker(), auth.AllowAll{}, Options{MaxStreams: -1})
	if _, err := off.streams.Acquire(); err != nil {
		t.Errorf("negative must disable the cap: %v", err)
	}
}

// Logs is the cheapest stream to open, so it is the one to prove the cap on.
func TestLogs_RefusedBeyondTheStreamCap(t *testing.T) {
	d := newFakeDocker()
	srv, m := newTestServer(d, auth.AllowAll{}, Options{MaxStreams: 1})

	// Hold the single slot.
	release, err := srv.streams.Acquire()
	if err != nil {
		t.Fatal(err)
	}

	err = gatedLogs(srv, &pb.LogsRequest{ContainerId: "abc"}, &fakeLogsStream{ctx: context.Background()})
	if statusCode(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
	// The refusal is counted, so a node running into its cap shows on a
	// dashboard before anyone files a ticket.
	if got := m.refusedStreams.Load(); got != 1 {
		t.Errorf("stream refusals counted = %d, want 1", got)
	}

	// Releasing it makes the agent usable again — the cap must be a queue-free
	// "not now", not a permanent degradation.
	release()
	var buf bytes.Buffer
	_, _ = newStdWriter(&buf, stdcopy.Stdout).Write([]byte("hello\n"))
	d.logsReader = io.NopCloser(&buf)
	if err := gatedLogs(srv, &pb.LogsRequest{ContainerId: "abc"}, &fakeLogsStream{ctx: context.Background()}); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if got := srv.streams.InUse(); got != 0 {
		t.Errorf("slot not returned after the stream ended: in use = %d", got)
	}
}

// The sidecar budget is separate and must be enforced before any container is
// created — the whole point is that the node never makes them.
func TestForward_SidecarCapRefusesBeforeCreatingContainers(t *testing.T) {
	d := newFakeDocker()
	d.inspect["target"] = container.InspectResponse{
		ID:     "target",
		Config: &container.Config{Image: "test-agent:latest"},
	}
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{ForwardImage: "test-agent:latest", MaxForwardSidecars: 1})

	held, err := srv.sidecars.Acquire()
	if err != nil {
		t.Fatal(err)
	}

	_, err = srv.openSidecarChannel(context.Background(), "target", 80)
	if statusCode(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
	if d.createdConfig != nil {
		t.Error("a refused forward must not have created a container")
	}
	held()
}

// A sidecar that fails to start must hand its slot back, or a node degrades
// permanently after a handful of failures.
func TestForward_FailedSidecarReleasesItsSlot(t *testing.T) {
	d := newFakeDocker()
	d.createContainerErr = errors.New("no space left on device")
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{ForwardImage: "test-agent:latest", MaxForwardSidecars: 2})

	for i := 0; i < 5; i++ {
		if _, err := srv.openSidecarChannel(context.Background(), "target", 80); err == nil {
			t.Fatal("expected the create to fail")
		}
	}
	if got := srv.sidecars.InUse(); got != 0 {
		t.Errorf("slots leaked by failed sidecars: in use = %d", got)
	}
}
