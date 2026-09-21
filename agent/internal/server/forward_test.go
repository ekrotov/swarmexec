// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// --- fake PortForward stream ---

type fwdRecvItem struct {
	msg *pb.ForwardClientMessage
	err error
}

type fakeForwardStream struct {
	ctx    context.Context
	recvCh chan fwdRecvItem

	mu   sync.Mutex
	sent []*pb.ForwardServerMessage
}

func newFakeForwardStream(ctx context.Context) *fakeForwardStream {
	return &fakeForwardStream{ctx: ctx, recvCh: make(chan fwdRecvItem, 64)}
}

func (s *fakeForwardStream) queueStart(containerID string, port uint32) {
	s.recvCh <- fwdRecvItem{msg: &pb.ForwardClientMessage{
		Payload: &pb.ForwardClientMessage_Start{
			Start: &pb.StartForward{ContainerId: containerID, Port: port},
		},
	}}
}

func (s *fakeForwardStream) queueData(b []byte) {
	s.recvCh <- fwdRecvItem{msg: &pb.ForwardClientMessage{
		Payload: &pb.ForwardClientMessage_Data{Data: b},
	}}
}

func (s *fakeForwardStream) queueEOF() { s.recvCh <- fwdRecvItem{err: io.EOF} }

func (s *fakeForwardStream) Recv() (*pb.ForwardClientMessage, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case item := <-s.recvCh:
		return item.msg, item.err
	}
}

func (s *fakeForwardStream) Send(m *pb.ForwardServerMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

func (s *fakeForwardStream) sentMessages() []*pb.ForwardServerMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*pb.ForwardServerMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

func (s *fakeForwardStream) Context() context.Context     { return s.ctx }
func (s *fakeForwardStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeForwardStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeForwardStream) SetTrailer(metadata.MD)       {}
func (s *fakeForwardStream) SendMsg(interface{}) error    { return nil }
func (s *fakeForwardStream) RecvMsg(interface{}) error    { return nil }

var _ pb.Agent_PortForwardServer = (*fakeForwardStream)(nil)

// forwardPayload collects the data bytes the agent forwarded to the client.
func forwardPayload(msgs []*pb.ForwardServerMessage) []byte {
	var out []byte
	for _, m := range msgs {
		if d, ok := m.Payload.(*pb.ForwardServerMessage_Data); ok {
			out = append(out, d.Data...)
		}
	}
	return out
}

func forwardError(msgs []*pb.ForwardServerMessage) string {
	for _, m := range msgs {
		if e, ok := m.Payload.(*pb.ForwardServerMessage_Error); ok {
			return e.Error
		}
	}
	return ""
}

func hasReady(msgs []*pb.ForwardServerMessage) bool {
	for _, m := range msgs {
		if _, ok := m.Payload.(*pb.ForwardServerMessage_Ready); ok {
			return true
		}
	}
	return false
}

// forwardTestServer builds a server whose sidecar image is fixed, so the tests
// never depend on self-inspection.
func forwardTestServer(d DockerClient, az auth.Authorizer) (*Server, *testMetrics) {
	return newTestServer(d, az, Options{ForwardImage: "test-agent:latest"})
}

// sidecarSpeak plays the sidecar side of the attach pipe: it writes a
// stdcopy-framed control line on stderr, then optional payload on stdout.
func sidecarSpeak(t *testing.T, conn io.Writer, control string, payload []byte) {
	t.Helper()
	w := stdcopy.NewStdWriter(conn, stdcopy.Stderr)
	if _, err := w.Write([]byte(control + "\n")); err != nil {
		t.Fatalf("write control line: %v", err)
	}
	if payload != nil {
		o := stdcopy.NewStdWriter(conn, stdcopy.Stdout)
		if _, err := o.Write(payload); err != nil {
			t.Fatalf("write payload: %v", err)
		}
	}
}

// --- protocol ---

func TestPortForward_FirstMessageMustBeStartForward(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueData([]byte("premature"))

	err := srv.PortForward(stream)
	if statusCode(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v (%v)", statusCode(err), err)
	}
}

func TestPortForward_RejectsOutOfRangePort(t *testing.T) {
	for _, port := range []uint32{0, 70000} {
		d := newFakeDocker()
		srv, _ := forwardTestServer(d, auth.AllowAll{})

		ctx, cancel := context.WithCancel(context.Background())
		stream := newFakeForwardStream(ctx)
		stream.queueStart("c1", port)

		err := srv.PortForward(stream)
		if statusCode(err) != codes.InvalidArgument {
			t.Errorf("port %d: want InvalidArgument, got %v (%v)", port, statusCode(err), err)
		}
		cancel()
	}
}

func TestPortForward_AuthorizationDenied(t *testing.T) {
	d := newFakeDocker()
	srv, m := forwardTestServer(d, denyAuth{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("c1", 8080)

	err := srv.PortForward(stream)
	if statusCode(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (%v)", statusCode(err), err)
	}
	if m.denied.Load() != 1 {
		t.Errorf("AuthDenied metric = %d, want 1", m.denied.Load())
	}
	if len(d.RemovedContainers()) != 0 {
		t.Errorf("denied forward must not create a sidecar, removed %v", d.RemovedContainers())
	}
}

// The authorizer must see "portforward" and the target port, so a policy can
// deny forwarding independently of exec.
func TestPortForward_AuthorizerSeesActionAndPort(t *testing.T) {
	d := newFakeDocker()
	var got auth.Request
	az := authFunc(func(_ context.Context, r auth.Request) auth.Decision {
		got = r
		return auth.Decision{Allow: false, Reason: "recorded"}
	})
	srv, _ := forwardTestServer(d, az)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("container-xyz", 5432)
	_ = srv.PortForward(stream)

	if got.Action != "portforward" {
		t.Errorf("Action = %q, want %q", got.Action, "portforward")
	}
	if got.Port != 5432 {
		t.Errorf("Port = %d, want 5432", got.Port)
	}
	if got.ContainerID != "container-xyz" {
		t.Errorf("ContainerID = %q", got.ContainerID)
	}
}

func TestPortForward_RejectedWhileDraining(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})
	srv.draining.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("c1", 8080)

	err := srv.PortForward(stream)
	if statusCode(err) != codes.Unavailable {
		t.Fatalf("want Unavailable, got %v (%v)", statusCode(err), err)
	}
}

// --- sidecar wiring ---

func TestPortForward_SidecarJoinsTargetNetnsWithoutPrivileges(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("target-abc", 8080)

	done := make(chan error, 1)
	go func() { done <- srv.PortForward(stream) }()

	conn := d.waitAttach()
	sidecarSpeak(t, conn, "ok", nil)
	_ = conn.Close()
	<-done

	if d.createdHostConfig == nil {
		t.Fatal("no sidecar was created")
	}
	if want := container.NetworkMode("container:target-abc"); d.createdHostConfig.NetworkMode != want {
		t.Errorf("NetworkMode = %q, want %q", d.createdHostConfig.NetworkMode, want)
	}
	// Joining a netns needs no capabilities; anything else widens the blast
	// radius of a container the agent starts on every forward.
	if len(d.createdHostConfig.CapDrop) != 1 || d.createdHostConfig.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", d.createdHostConfig.CapDrop)
	}
	if !d.createdHostConfig.ReadonlyRootfs {
		t.Error("sidecar rootfs should be read-only")
	}
	if !d.createdHostConfig.AutoRemove {
		t.Error("sidecar should auto-remove")
	}
	// A TTY would apply line-discipline translation and corrupt binary payloads.
	if d.createdConfig.Tty {
		t.Error("sidecar must not allocate a TTY")
	}
	if d.createdConfig.Image != "test-agent:latest" {
		t.Errorf("Image = %q", d.createdConfig.Image)
	}
	if got := strings.Join(d.createdConfig.Cmd, " "); got != "-forward-to 127.0.0.1:8080" {
		t.Errorf("Cmd = %q", got)
	}
	if d.createdConfig.Labels[forwardLabel] != forwardValue {
		t.Errorf("missing %s=%s label: %v", forwardLabel, forwardValue, d.createdConfig.Labels)
	}
}

// Readiness is what makes a forward trustworthy: the client must not be told
// "ready" when the target port refused the connection.
func TestPortForward_DialFailureReportsErrorNotReady(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("target-abc", 9999)

	done := make(chan error, 1)
	go func() { done <- srv.PortForward(stream) }()

	conn := d.waitAttach()
	sidecarSpeak(t, conn, "error: connection refused", nil)
	_ = conn.Close()

	if err := <-done; statusCode(err) != codes.Internal {
		t.Fatalf("want Internal, got %v (%v)", statusCode(err), err)
	}
	msgs := stream.sentMessages()
	if hasReady(msgs) {
		t.Error("agent sent Ready despite a failed dial")
	}
	if got := forwardError(msgs); !strings.Contains(got, "connection refused") {
		t.Errorf("error = %q, want it to mention the refusal", got)
	}
}

func TestPortForward_SidecarExitBeforeReadinessIsAnError(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("target-abc", 8080)

	done := make(chan error, 1)
	go func() { done <- srv.PortForward(stream) }()

	// Sidecar dies without saying anything.
	conn := d.waitAttach()
	_ = conn.Close()

	if err := <-done; err == nil {
		t.Fatal("want an error when the sidecar never reports readiness")
	}
	if hasReady(stream.sentMessages()) {
		t.Error("agent sent Ready although the sidecar never reported")
	}
}

func TestPortForward_BridgesBothDirections(t *testing.T) {
	d := newFakeDocker()
	srv, m := forwardTestServer(d, auth.AllowAll{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("target-abc", 8080)

	done := make(chan error, 1)
	go func() { done <- srv.PortForward(stream) }()

	conn := d.waitAttach()

	// Container -> client, including a NUL byte: the tunnel must be binary-safe.
	payload := []byte{0x00, 0x01, 'H', 'T', 'T', 'P', 0xff}
	sidecarSpeak(t, conn, "ok", payload)

	// Client -> container.
	stream.queueData([]byte("ping"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read client->container bytes: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("container received %q, want %q", got, "ping")
	}

	stream.queueEOF()
	_ = conn.Close()
	<-done

	msgs := stream.sentMessages()
	if !hasReady(msgs) {
		t.Error("no Ready message before payload")
	}
	if fwd := forwardPayload(msgs); string(fwd) != string(payload) {
		t.Errorf("forwarded payload = %v, want %v", fwd, payload)
	}
	if m.in.Load() != 4 {
		t.Errorf("bytes in = %d, want 4", m.in.Load())
	}
	if m.out.Load() != int64(len(payload)) {
		t.Errorf("bytes out = %d, want %d", m.out.Load(), len(payload))
	}
}

// A leaked sidecar keeps the target's netns pinned, so cleanup is not optional.
func TestPortForward_RemovesSidecarOnTeardown(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("target-abc", 8080)

	done := make(chan error, 1)
	go func() { done <- srv.PortForward(stream) }()

	conn := d.waitAttach()
	sidecarSpeak(t, conn, "ok", nil)
	_ = conn.Close()
	<-done

	if got := d.RemovedContainers(); len(got) != 1 || got[0] != "sidecar-1" {
		t.Errorf("removed containers = %v, want [sidecar-1]", got)
	}
	// The node's container budget has to come back with the container. A slot
	// held past teardown is a slow leak that only shows up as an agent refusing
	// forwards after a day of normal use.
	if got := srv.sidecars.inUse(); got != 0 {
		t.Errorf("sidecar slot not released on teardown: in use = %d", got)
	}
	if got := srv.streams.inUse(); got != 0 {
		t.Errorf("stream slot not released after the RPC returned: in use = %d", got)
	}
}

func TestPortForward_SidecarCreateFailureIsReported(t *testing.T) {
	d := newFakeDocker()
	d.createContainerErr = errors.New("no such image")
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("target-abc", 8080)

	err := srv.PortForward(stream)
	if statusCode(err) != codes.Internal {
		t.Fatalf("want Internal, got %v (%v)", statusCode(err), err)
	}
	if got := forwardError(stream.sentMessages()); !strings.Contains(got, "no such image") {
		t.Errorf("error = %q, want it to surface the docker failure", got)
	}
}

// The forward must count toward the drain wait group, or a rolling agent update
// would cut live forwards mid-connection.
func TestPortForward_CountsAsActiveSession(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("target-abc", 8080)

	done := make(chan error, 1)
	go func() { done <- srv.PortForward(stream) }()

	conn := d.waitAttach()
	sidecarSpeak(t, conn, "ok", nil)

	deadline := time.Now().Add(2 * time.Second)
	for srv.active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.active.Load() != 1 {
		t.Errorf("active = %d during a forward, want 1", srv.active.Load())
	}

	_ = conn.Close()
	<-done
	if srv.active.Load() != 0 {
		t.Errorf("active = %d after teardown, want 0", srv.active.Load())
	}
}

// --- control-line parsing ---

func TestSidecarControl_ParsesReadinessLine(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantErr string
	}{
		{"ok", "ok\n", ""},
		{"error", "error: connection refused\n", "connection refused"},
		{"garbage", "what?\n", `unexpected readiness line "what?"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ready := make(chan error, 1)
			w := &sidecarControl{log: discardLogger(), id: "s1", ready: ready}
			if _, err := w.Write([]byte(tc.line)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			select {
			case err := <-ready:
				switch {
				case tc.wantErr == "" && err != nil:
					t.Errorf("want ready, got %v", err)
				case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
					t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
				}
			default:
				t.Error("no readiness reported")
			}
		})
	}
}

// Only the FIRST line is the control line; later output is diagnostics and must
// not be mistaken for a second verdict.
func TestSidecarControl_ReportsOnlyOnce(t *testing.T) {
	ready := make(chan error, 2)
	w := &sidecarControl{log: discardLogger(), id: "s1", ready: ready}
	if _, err := w.Write([]byte("ok\nerror: too late\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := <-ready; err != nil {
		t.Fatalf("first verdict = %v, want ready", err)
	}
	select {
	case extra := <-ready:
		t.Errorf("second verdict reported: %v", extra)
	default:
	}
}

// A control line split across writes must still parse.
func TestSidecarControl_HandlesSplitWrites(t *testing.T) {
	ready := make(chan error, 1)
	w := &sidecarControl{log: discardLogger(), id: "s1", ready: ready}
	for _, part := range []string{"er", "ror: conn", "ection refused\n"} {
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	err := <-ready
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("err = %v, want it to contain \"connection refused\"", err)
	}
}

// authFunc adapts a function to auth.Authorizer.
type authFunc func(context.Context, auth.Request) auth.Decision

func (f authFunc) Authorize(ctx context.Context, r auth.Request) auth.Decision { return f(ctx, r) }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ContainerInspect's Config is a pointer and the daemon does not guarantee it.
// Dereferencing it blind panicked the RPC goroutine inside a sync.Once, so the
// crash was also cached: every later forward on that agent hit the same nil.
func TestForwardImage_NilConfigIsAnErrorNotAPanic(t *testing.T) {
	d := newFakeDocker()
	host, _ := os.Hostname()
	d.inspect[host] = types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{ID: host},
		Config:            nil, // what the guard is for
	}
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{}) // no ForwardImage override

	img, err := srv.forwardImage(context.Background())
	if err == nil {
		t.Fatalf("want an error, got image %q", img)
	}
	if !strings.Contains(err.Error(), "-forward-image") {
		t.Errorf("the error should name the way out: %v", err)
	}

	// An empty image is the same failure wearing a different hat.
	d2 := newFakeDocker()
	d2.inspect[host] = types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{ID: host},
		Config:            &container.Config{Image: ""},
	}
	srv2, _ := newTestServer(d2, auth.AllowAll{}, Options{})
	if _, err := srv2.forwardImage(context.Background()); err == nil {
		t.Error("an empty image must be refused too")
	}
}
