// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/events"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// fakeEventStream captures what the agent sends.
type fakeEventStream struct {
	ctx context.Context
	mu  sync.Mutex
	got []*pb.ContainerEvent
}

func (s *fakeEventStream) Send(e *pb.ContainerEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
	return nil
}
func (s *fakeEventStream) events() []*pb.ContainerEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.ContainerEvent(nil), s.got...)
}
func (s *fakeEventStream) Context() context.Context     { return s.ctx }
func (s *fakeEventStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeEventStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeEventStream) SetTrailer(metadata.MD)       {}
func (s *fakeEventStream) SendMsg(any) error            { return nil }
func (s *fakeEventStream) RecvMsg(any) error            { return nil }

var _ pb.Agent_WatchContainerEventsServer = (*fakeEventStream)(nil)

func eventServer(t *testing.T, az auth.Authorizer) (*Server, *fakeDocker) {
	t.Helper()
	d := newFakeDocker()
	d.eventCh = make(chan events.Message, 8)
	d.eventErrCh = make(chan error, 1)
	srv, _ := newTestServer(d, az, Options{})
	return srv, d
}

// The point of the RPC: the manager cannot tell you a container is unhealthy,
// was OOM-killed, or exited with 137. The node can, immediately.
func TestWatchContainerEvents_ForwardsTheTypedSubset(t *testing.T) {
	srv, d := eventServer(t, auth.AllowAll{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeEventStream{ctx: ctx}

	done := make(chan error, 1)
	go func() {
		done <- srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{ContainerId: "c1"}, stream)
	}()

	d.eventCh <- events.Message{Type: events.ContainerEventType, Action: "health_status: unhealthy", TimeNano: 42}
	d.eventCh <- events.Message{
		Type: events.ContainerEventType, Action: "die", TimeNano: 43,
		Actor: events.Actor{Attributes: map[string]string{"exitCode": "137"}},
	}

	waitUntil(t, "both events forwarded", func() bool { return len(stream.events()) == 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a cancelled watch is a clean end, got %v", err)
	}

	got := stream.events()
	if got[0].Action != "health_status: unhealthy" || got[0].Health != "unhealthy" {
		t.Errorf("health event = %+v", got[0])
	}
	if got[0].TimeUnixNano != 42 {
		t.Errorf("timestamp dropped: %+v", got[0])
	}
	if got[1].Action != "die" || got[1].ExitCode != 137 {
		t.Errorf("die event = %+v", got[1])
	}
}

// Docker attaches the container's whole label set to every event. Forwarding
// that wholesale would turn this RPC into an exfiltration path for whatever a
// deployer put in a label, so only the typed fields cross the wire.
func TestWatchContainerEvents_DropsActorAttributes(t *testing.T) {
	srv, d := eventServer(t, auth.AllowAll{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeEventStream{ctx: ctx}

	go srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{ContainerId: "c1"}, stream)

	d.eventCh <- events.Message{
		Type: events.ContainerEventType, Action: "start",
		Actor: events.Actor{Attributes: map[string]string{
			"com.example.token": "hunter2",
			"image":             "internal-registry.example/private:1",
		}},
	}
	waitUntil(t, "the event to be forwarded", func() bool { return len(stream.events()) == 1 })

	ev := stream.events()[0]
	if ev.String() == "" {
		t.Fatal("empty event")
	}
	if s := ev.String(); contains(s, "hunter2") || contains(s, "internal-registry") {
		t.Errorf("event carries actor attributes: %q", s)
	}
}

// The narrowing has to happen at the daemon. An agent that subscribed to every
// container and filtered afterwards would be doing it in the one place where a
// mistake leaks another container's lifecycle.
func TestWatchContainerEvents_FiltersAtTheDaemon(t *testing.T) {
	srv, d := eventServer(t, auth.AllowAll{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{ContainerId: "c1"}, &fakeEventStream{ctx: ctx})
	waitUntil(t, "the subscription", func() bool { return len(d.eventFilters().Get("container")) > 0 })

	f := d.eventFilters()
	if got := f.Get("container"); len(got) != 1 || got[0] != "c1" {
		t.Errorf("container filter = %v, want [c1]", got)
	}
	if got := f.Get("type"); len(got) != 1 || got[0] != "container" {
		t.Errorf("type filter = %v, want [container]", got)
	}
}

func TestWatchContainerEvents_RequiresAContainerID(t *testing.T) {
	srv, _ := eventServer(t, auth.AllowAll{})
	err := srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{}, &fakeEventStream{ctx: context.Background()})
	if statusCode(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// Watching a container is watching a container: a policy that can deny exec on
// it must be able to deny this too.
func TestWatchContainerEvents_HonoursTheAuthorizer(t *testing.T) {
	var got auth.Request
	az := authFunc(func(_ context.Context, r auth.Request) auth.Decision {
		got = r
		return auth.Decision{Allow: false, Reason: "denied"}
	})
	srv, d := eventServer(t, az)

	err := srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{ContainerId: "c1"},
		&fakeEventStream{ctx: context.Background()})
	if statusCode(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if got.Action != "container.events" {
		t.Errorf("action = %q, want container.events", got.Action)
	}
	if len(d.eventFilters().Get("container")) != 0 {
		t.Error("a denied watch must not subscribe to anything")
	}
}

// A long-lived stream held open for as long as a view is open belongs under the
// same cap as exec, logs and port-forward — not around it.
func TestWatchContainerEvents_CountsAgainstTheStreamCap(t *testing.T) {
	d := newFakeDocker()
	d.eventCh = make(chan events.Message, 1)
	d.eventErrCh = make(chan error, 1)
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{MaxStreams: 1})

	release, err := srv.streams.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	err = srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{ContainerId: "c1"},
		&fakeEventStream{ctx: context.Background()})
	if statusCode(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
}

// A failed subscription must say so in-band, so the client can report why the
// live indicator stopped instead of leaving it silently frozen.
func TestWatchContainerEvents_ReportsAStreamFailure(t *testing.T) {
	srv, d := eventServer(t, auth.AllowAll{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeEventStream{ctx: ctx}

	done := make(chan error, 1)
	go func() {
		done <- srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{ContainerId: "c1"}, stream)
	}()
	waitUntil(t, "the subscription", func() bool { return len(d.eventFilters().Get("container")) > 0 })

	d.eventErrCh <- errors.New("docker went away")

	select {
	case err := <-done:
		if statusCode(err) != codes.Internal {
			t.Errorf("want Internal, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watch did not end when the event stream failed")
	}
	evs := stream.events()
	if len(evs) != 1 || evs[0].Error == "" {
		t.Errorf("the failure was not reported in-band: %+v", evs)
	}
}

func TestWatchContainerEvents_RefusedWhileDraining(t *testing.T) {
	srv, _ := eventServer(t, auth.AllowAll{})
	srv.draining.Store(true)
	err := srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{ContainerId: "c1"},
		&fakeEventStream{ctx: context.Background()})
	if statusCode(err) != codes.Unavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
}

func TestParseExitCode(t *testing.T) {
	cases := map[string]int32{"0": 0, "137": 137, "255": 255, "256": 0, "": 0, "abc": 0, "-1": 0}
	for in, want := range cases {
		if got := parseExitCode(in); got != want {
			t.Errorf("parseExitCode(%q) = %d, want %d", in, got, want)
		}
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}
