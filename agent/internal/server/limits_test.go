// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"google.golang.org/grpc/codes"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

func TestLimiter_RefusesBeyondTheCapAndReleases(t *testing.T) {
	l := newLimiter(2, "things", "-max-things")

	r1, err := l.acquire()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := l.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.acquire(); statusCode(err) != codes.ResourceExhausted {
		t.Fatalf("third acquire: want ResourceExhausted, got %v", err)
	}
	// The error has to tell the operator which knob to turn; a bare "limit
	// reached" leaves them guessing at a value they own.
	if _, err := l.acquire(); err == nil || !strings.Contains(err.Error(), "-max-things") {
		t.Errorf("error should name the flag: %v", err)
	}

	// A released slot is reusable: the cap is "not now", not a permanent
	// degradation of the node.
	r1()
	r3, err := l.acquire()
	if err != nil {
		t.Fatalf("a released slot must be reusable: %v", err)
	}
	if got := l.inUse(); got != 2 {
		t.Fatalf("in use = %d, want 2", got)
	}

	// A rejected acquire must not leak a slot: the counter is incremented
	// speculatively and has to be given back on the failing path. Fifty
	// rejections must leave the count exactly where it was.
	for i := 0; i < 50; i++ {
		if _, err := l.acquire(); err == nil {
			t.Fatal("expected the cap to hold")
		}
	}
	if got := l.inUse(); got != 2 {
		t.Errorf("rejections leaked slots: in use = %d, want 2", got)
	}

	// Release is idempotent — the forward path can reach it twice.
	r3()
	r3()
	if got := l.inUse(); got != 1 {
		t.Errorf("double release freed a slot that was not held: in use = %d, want 1", got)
	}
	r2()
}

func TestLimiter_ZeroOrNegativeMeansUnlimited(t *testing.T) {
	for _, max := range []int{0, -1} {
		l := newLimiter(max, "things", "-max-things")
		for i := 0; i < 1000; i++ {
			if _, err := l.acquire(); err != nil {
				t.Fatalf("max=%d: acquire %d refused: %v", max, i, err)
			}
		}
	}
	// A nil limiter is also "no limit", so a zero-value Server in a test cannot
	// panic on the hot path.
	var nilLim *limiter
	if _, err := nilLim.acquire(); err != nil {
		t.Errorf("nil limiter must not refuse: %v", err)
	}
}

// An Options built without an opinion about limits — which is every existing
// caller — must come out capped rather than uncapped.
func TestNew_AppliesDefaultCaps(t *testing.T) {
	s, _ := newTestServer(newFakeDocker(), auth.AllowAll{}, Options{})
	if s.streams.max != defaultMaxStreams {
		t.Errorf("stream cap = %d, want %d", s.streams.max, defaultMaxStreams)
	}
	if s.sidecars.max != defaultMaxForwardSidecars {
		t.Errorf("sidecar cap = %d, want %d", s.sidecars.max, defaultMaxForwardSidecars)
	}
	// The sidecar budget must be the tighter of the two: reaching it first is
	// what keeps the refusal ahead of the containers being created.
	if s.sidecars.max >= s.streams.max {
		t.Errorf("sidecar cap (%d) should be below the stream cap (%d)", s.sidecars.max, s.streams.max)
	}

	off, _ := newTestServer(newFakeDocker(), auth.AllowAll{}, Options{MaxStreams: -1})
	if _, err := off.streams.acquire(); err != nil {
		t.Errorf("negative must disable the cap: %v", err)
	}
}

// Logs is the cheapest stream to open, so it is the one to prove the cap on.
func TestLogs_RefusedBeyondTheStreamCap(t *testing.T) {
	d := newFakeDocker()
	srv, m := newTestServer(d, auth.AllowAll{}, Options{MaxStreams: 1})

	// Hold the single slot.
	release, err := srv.streams.acquire()
	if err != nil {
		t.Fatal(err)
	}

	err = srv.Logs(&pb.LogsRequest{ContainerId: "abc"}, &fakeLogsStream{ctx: context.Background()})
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
	_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte("hello\n"))
	d.logsReader = io.NopCloser(&buf)
	if err := srv.Logs(&pb.LogsRequest{ContainerId: "abc"}, &fakeLogsStream{ctx: context.Background()}); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if got := srv.streams.inUse(); got != 0 {
		t.Errorf("slot not returned after the stream ended: in use = %d", got)
	}
}

// The sidecar budget is separate and must be enforced before any container is
// created — the whole point is that the node never makes them.
func TestForward_SidecarCapRefusesBeforeCreatingContainers(t *testing.T) {
	d := newFakeDocker()
	d.inspect["target"] = types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{ID: "target"},
		Config:            &container.Config{Image: "test-agent:latest"},
	}
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{ForwardImage: "test-agent:latest", MaxForwardSidecars: 1})

	held, err := srv.sidecars.acquire()
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
	if got := srv.sidecars.inUse(); got != 0 {
		t.Errorf("slots leaked by failed sidecars: in use = %d", got)
	}
}
