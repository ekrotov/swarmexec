// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/forwardmux"
)

// startForward runs one PortForward and hands back the sidecar side plus a way
// to end it, so a test can drive several forwards against one fake daemon.
func startForward(t *testing.T, srv *Server, d *fakeDocker, container string, port uint32) (*fakeForwardStream, *muxSide, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeForwardStream(ctx)
	stream.queueStart(container, port)
	done := make(chan error, 1)
	go func() { done <- srv.PortForward(stream) }()
	mux := newMuxSide(t, d.waitAttach())
	return stream, mux, func() { cancel(); <-done }
}

// The point of the whole change: a second connection to the same target must
// reuse the running sidecar instead of paying another ~300 ms container start.
func TestForwardMux_SecondConnectionReusesTheSidecar(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	_, mux, stop1 := startForward(t, srv, d, "target-abc", 8080)
	id1 := mux.acceptOpen()

	// A second forward stream to the same container and port.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	s2 := newFakeForwardStream(ctx2)
	s2.queueStart("target-abc", 8080)
	done2 := make(chan error, 1)
	go func() { done2 <- srv.PortForward(s2) }()

	// It must arrive on the SAME sidecar — a new open frame on the stream we
	// already hold, not a new attach.
	id2 := mux.acceptOpen()
	if id2 == id1 {
		t.Fatalf("both connections got id %d; ids must be distinct", id1)
	}
	if got := srv.muxes.liveSidecars(); got != 1 {
		t.Errorf("%d sidecars for one target, want 1", got)
	}
	if d.createdConfig == nil {
		t.Fatal("no sidecar was created at all")
	}

	// Both connections carry their own bytes, and nothing crosses over.
	mux.send(forwardmux.Frame{Conn: id1, Kind: forwardmux.KindData, Payload: []byte("one")})
	mux.send(forwardmux.Frame{Conn: id2, Kind: forwardmux.KindData, Payload: []byte("two")})
	waitUntil(t, "both payloads delivered", func() bool {
		return len(forwardPayload(s2.sentMessages())) > 0
	})
	if got := string(forwardPayload(s2.sentMessages())); got != "two" {
		t.Errorf("second connection received %q, want \"two\"", got)
	}

	cancel2()
	<-done2
	stop1()
}

// Different ports on the same container are different targets and must not
// share a sidecar — the sidecar dials one address.
func TestForwardMux_DifferentPortsGetTheirOwnSidecar(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	_, mux1, stop1 := startForward(t, srv, d, "target-abc", 8080)
	mux1.acceptOpen()
	_, mux2, stop2 := startForward(t, srv, d, "target-abc", 9090)
	mux2.acceptOpen()

	if got := srv.muxes.liveSidecars(); got != 2 {
		t.Errorf("%d sidecars for two ports, want 2", got)
	}
	stop1()
	stop2()
}

// A dial failure inside the sidecar must reach the client as a refusal of THAT
// connection, with the reason — not as a broken forward.
func TestForwardMux_DialFailureIsReported(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("target-abc", 8080)
	done := make(chan error, 1)
	go func() { done <- srv.PortForward(stream) }()

	mux := newMuxSide(t, d.waitAttach())
	mux.refuseOpen("connection refused")

	if err := <-done; statusCode(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
	if hasReady(stream.sentMessages()) {
		t.Error("Ready was sent although the dial failed")
	}
	if got := forwardError(stream.sentMessages()); got == "" || !contains(got, "connection refused") {
		t.Errorf("error = %q, want the refusal", got)
	}
}

// Half-closing one connection must not half-close the others. Before mux this
// was closing the stdio, which is exactly what must NOT happen now.
func TestForwardMux_CloseWriteIsPerConnection(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	s1, mux, stop1 := startForward(t, srv, d, "target-abc", 8080)
	id1 := mux.acceptOpen()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	s2 := newFakeForwardStream(ctx2)
	s2.queueStart("target-abc", 8080)
	done2 := make(chan error, 1)
	go func() { done2 <- srv.PortForward(s2) }()
	id2 := mux.acceptOpen()

	// The first client half-closes.
	s1.queueEOF()
	f := mux.recv()
	if f.Kind != forwardmux.KindCloseWrite || f.Conn != id1 {
		t.Fatalf("want close-write on conn %d, got %s on %d", id1, forwardmux.KindName(f.Kind), f.Conn)
	}

	// The second is untouched and still carries data.
	mux.send(forwardmux.Frame{Conn: id2, Kind: forwardmux.KindData, Payload: []byte("still here")})
	waitUntil(t, "the other connection still delivers", func() bool {
		return string(forwardPayload(s2.sentMessages())) == "still here"
	})

	cancel2()
	<-done2
	stop1()
}

// When the sidecar dies, every connection on it has to end — and with a reason,
// not by hanging.
func TestForwardMux_SidecarDeathEndsEveryConnection(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	_, mux, _ := startForward(t, srv, d, "target-abc", 8080)
	mux.acceptOpen()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	s2 := newFakeForwardStream(ctx2)
	s2.queueStart("target-abc", 8080)
	done2 := make(chan error, 1)
	go func() { done2 <- srv.PortForward(s2) }()
	mux.acceptOpen()

	_ = mux.conn.Close() // the container is gone

	select {
	case <-done2:
	case <-time.After(3 * time.Second):
		t.Fatal("a connection outlived its sidecar")
	}
	waitUntil(t, "the pool to forget the dead sidecar", func() bool {
		return srv.muxes.liveSidecars() == 0
	})
}

// The container budget now counts FORWARDS, not connections — which is what the
// limit was always meant to mean.
func TestForwardMux_SidecarCapCountsForwardsNotConnections(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{ForwardImage: "test-agent:latest", MaxForwardSidecars: 1})

	_, mux, stop := startForward(t, srv, d, "target-abc", 8080)
	mux.acceptOpen()

	// A second connection to the same target fits, because it shares the one
	// sidecar the budget allows.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	s2 := newFakeForwardStream(ctx2)
	s2.queueStart("target-abc", 8080)
	done2 := make(chan error, 1)
	go func() { done2 <- srv.PortForward(s2) }()
	mux.acceptOpen()

	if got := srv.sidecars.inUse(); got != 1 {
		t.Errorf("two connections used %d sidecar slots, want 1", got)
	}
	cancel2()
	<-done2
	stop()
}
