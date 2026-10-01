// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/audit"
	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// testMetrics counts Metrics calls.
type testMetrics struct {
	started, ended, denied atomic.Int64
	in, out                atomic.Int64
	refusedStreams         atomic.Int64
	refusedSidecars        atomic.Int64
}

func (m *testMetrics) SessionStarted()             { m.started.Add(1) }
func (m *testMetrics) SessionEnded()               { m.ended.Add(1) }
func (m *testMetrics) AuthDenied()                 { m.denied.Add(1) }
func (m *testMetrics) BytesTransferred(i, o int64) { m.in.Add(i); m.out.Add(o) }
func (m *testMetrics) LimitRefused(limit string) {
	switch limit {
	case "streams":
		m.refusedStreams.Add(1)
	case "forward_sidecars":
		m.refusedSidecars.Add(1)
	}
}

// denyAuth always denies.
type denyAuth struct{}

func (denyAuth) Authorize(context.Context, auth.Request) auth.Decision {
	return auth.Decision{Allow: false, Reason: "test deny"}
}

func newTestServer(d DockerClient, az auth.Authorizer, opts Options) (*Server, *testMetrics) {
	m := &testMetrics{}
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	al := audit.New(slog.New(slog.NewJSONHandler(io.Discard, nil)), true)
	s := New(d, az, al, log, m, opts)
	s.identityFn = func(context.Context) (string, error) { return "test-user", nil }
	return s, m
}

// auditedTestServer is newTestServer with the audit stream captured, for the
// tests that assert an action leaves a record — the claim being tested is about
// the audit log itself, so discarding it would test nothing.
func auditedTestServer(d DockerClient, az auth.Authorizer) (*Server, *syncBuffer) {
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := New(d, az, audit.New(slog.New(slog.NewJSONHandler(buf, nil)), true), log, &testMetrics{}, Options{})
	s.identityFn = func(context.Context) (string, error) { return "test-user", nil }
	return s, buf
}

// syncBuffer is a bytes.Buffer safe for a handler writing from another
// goroutine; slog gives no ordering guarantee about who calls Write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// findAuditEvent returns the first audit record with the given event name,
// failing the test when there is none.
func findAuditEvent(t *testing.T, out, event string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["event"] == event {
			return rec
		}
	}
	t.Fatalf("no %q audit record in:\n%s", event, out)
	return nil
}

func startExec(containerID string, cmd []string, tty bool) *pb.StartExec {
	return &pb.StartExec{ContainerId: containerID, Cmd: cmd, Tty: tty}
}

// statusCode extracts the gRPC code from an error.
func statusCode(err error) codes.Code {
	return status.Code(err)
}

// --- §9.7: first message must be StartExec ---

func TestExec_FirstMessageMustBeStartExec(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStdin([]byte("oops")) // stdin before StartExec is a violation

	err := srv.Exec(stream)
	if statusCode(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v (%v)", statusCode(err), err)
	}
}

func TestExec_EmptyStartIsInvalid(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(&pb.StartExec{}) // no container_id / cmd

	if err := srv.Exec(stream); statusCode(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

// --- §9.7: authorization allow/deny ---

func TestExec_AuthorizationDenied(t *testing.T) {
	d := newFakeDocker()
	srv, m := newTestServer(d, denyAuth{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, true))

	err := srv.Exec(stream)
	if statusCode(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (%v)", statusCode(err), err)
	}
	if m.denied.Load() != 1 {
		t.Fatalf("want AuthDenied metric=1, got %d", m.denied.Load())
	}
	if d.containerConn != nil {
		t.Fatal("exec must not be attached when authorization is denied")
	}
}

func TestExec_AuthorizationAllowedRunsSession(t *testing.T) {
	d := newFakeDocker()
	d.exitCode = 7
	srv, m := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, true))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()

	conn := d.waitAttach()
	conn.Close() // process exits / output ends

	if err := <-errCh; err != nil {
		t.Fatalf("Exec returned error: %v", err)
	}
	if m.started.Load() != 1 || m.ended.Load() != 1 {
		t.Fatalf("session metrics: started=%d ended=%d", m.started.Load(), m.ended.Load())
	}
	if got := lastExitCode(t, stream); got != 7 {
		t.Fatalf("want exit code 7, got %d", got)
	}
}

// --- §9.7: stdcopy demux routing (non-TTY) ---

func TestExec_NonTTYDemuxRouting(t *testing.T) {
	d := newFakeDocker()
	d.exitCode = 0
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"ls"}, false)) // tty=false

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()

	conn := d.waitAttach()
	// Write framed stdout and stderr from the container side.
	if _, err := stdcopy.NewStdWriter(conn, stdcopy.Stdout).Write([]byte("hello-out")); err != nil {
		t.Fatal(err)
	}
	if _, err := stdcopy.NewStdWriter(conn, stdcopy.Stderr).Write([]byte("hello-err")); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	if err := <-errCh; err != nil {
		t.Fatalf("Exec error: %v", err)
	}

	var gotOut, gotErr string
	for _, m := range stream.sentMessages() {
		switch p := m.Payload.(type) {
		case *pb.ServerMessage_Stdout:
			gotOut += string(p.Stdout)
		case *pb.ServerMessage_Stderr:
			gotErr += string(p.Stderr)
		}
	}
	if gotOut != "hello-out" {
		t.Errorf("stdout routing: want %q, got %q", "hello-out", gotOut)
	}
	if gotErr != "hello-err" {
		t.Errorf("stderr routing: want %q, got %q", "hello-err", gotErr)
	}
}

func TestExec_TTYForwardsRawAsStdout(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, true))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()

	conn := d.waitAttach()
	conn.Write([]byte("raw-tty-bytes"))
	conn.Close()
	<-errCh

	var out string
	for _, m := range stream.sentMessages() {
		if p, ok := m.Payload.(*pb.ServerMessage_Stdout); ok {
			out += string(p.Stdout)
		}
		if _, ok := m.Payload.(*pb.ServerMessage_Stderr); ok {
			t.Fatal("TTY mode must never emit stderr")
		}
	}
	if out != "raw-tty-bytes" {
		t.Fatalf("want raw-tty-bytes, got %q", out)
	}
}

// --- §9.7: buffer-copy safety ---

func TestMsgWriter_CopiesBuffer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	var count atomic.Int64
	w := &msgWriter{stream: stream, count: &count}

	buf := []byte("hello")
	if _, err := w.Write(buf); err != nil {
		t.Fatal(err)
	}
	// Mutate the source buffer after Write, simulating a reused read buffer.
	buf[0] = 'X'

	sent := stream.sentMessages()
	if len(sent) != 1 {
		t.Fatalf("want 1 message, got %d", len(sent))
	}
	got := string(sent[0].Payload.(*pb.ServerMessage_Stdout).Stdout)
	if got != "hello" {
		t.Fatalf("buffer not copied: want %q, got %q", "hello", got)
	}
	if count.Load() != 5 {
		t.Fatalf("want byte count 5, got %d", count.Load())
	}
}

// --- stdin + resize bridging ---

func TestExec_StdinAndResizeForwarded(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(&pb.StartExec{ContainerId: "c1", Cmd: []string{"/bin/sh"}, Tty: true, Width: 80, Height: 24})

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()

	conn := d.waitAttach()

	// Read stdin the agent forwards from the container side.
	stream.queueResize(120, 40)
	stream.queueStdin([]byte("echo hi\n"))

	readBuf := make([]byte, 64)
	n, _ := conn.Read(readBuf)
	if string(readBuf[:n]) != "echo hi\n" {
		t.Fatalf("stdin not forwarded: got %q", string(readBuf[:n]))
	}

	conn.Close()
	<-errCh

	// Initial size + the resize event should both have been applied.
	resizes := d.Resizes()
	if len(resizes) < 2 {
		t.Fatalf("want >=2 resizes (initial + event), got %d: %+v", len(resizes), resizes)
	}
	if resizes[0] != (container.ResizeOptions{Height: 24, Width: 80}) {
		t.Errorf("initial resize wrong: %+v", resizes[0])
	}
	last := resizes[len(resizes)-1]
	if last != (container.ResizeOptions{Height: 40, Width: 120}) {
		t.Errorf("resize event wrong: %+v", last)
	}
}

func TestExec_ClientHalfCloseClosesStdin(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"cat"}, false))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()

	conn := d.waitAttach()
	stream.queueEOF() // client CloseSend -> EOF to process stdin

	// The container side should observe EOF on read (close-write happened).
	done := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 8))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("want EOF on container read after half-close, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for stdin EOF after half-close")
	}
	conn.Close()
	<-errCh
}

// --- §9.7: graceful shutdown ---

func TestExec_RejectedWhileDraining(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	srv.StartDrain()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, true))

	if err := srv.Exec(stream); statusCode(err) != codes.Unavailable {
		t.Fatalf("want Unavailable while draining, got %v", err)
	}
}

func TestServer_WaitForSessionsDrains(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, true))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()
	conn := d.waitAttach()

	// Session is live now.
	waitFor(t, func() bool { return srv.ActiveSessions() == 1 })

	// Not drained yet.
	dctx, dcancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer dcancel()
	if srv.WaitForSessions(dctx) {
		t.Fatal("WaitForSessions returned true while a session is active")
	}

	// End the session; drain should now complete.
	conn.Close()
	<-errCh
	dctx2, dcancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer dcancel2()
	if !srv.WaitForSessions(dctx2) {
		t.Fatal("WaitForSessions did not drain after session ended")
	}
	if srv.ActiveSessions() != 0 {
		t.Fatalf("active sessions should be 0, got %d", srv.ActiveSessions())
	}
}

// --- idle timeout ---

func TestExec_IdleTimeout(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{IdleTimeout: 1 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, true))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()
	_ = d.waitAttach()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("idle-timed-out session should return nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle timeout did not fire")
	}

	var sawErr bool
	for _, m := range stream.sentMessages() {
		if _, ok := m.Payload.(*pb.ServerMessage_Error); ok {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected an error message on idle timeout")
	}
}

// --- ListContainers ---

func TestListContainers_Filter(t *testing.T) {
	d := newFakeDocker()
	d.containers = []types.Container{
		{ID: "a1", Names: []string{"/web.1"}, Labels: map[string]string{swarmServiceLabel: "web"}},
		{ID: "b2", Names: []string{"/db.1"}, Labels: map[string]string{swarmServiceLabel: "db"}},
		{ID: "c3", Names: []string{"/standalone"}},
	}
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})

	all, err := srv.ListContainers(context.Background(), &pb.ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Containers) != 3 {
		t.Fatalf("want 3 containers, got %d", len(all.Containers))
	}

	filtered, err := srv.ListContainers(context.Background(), &pb.ListRequest{ServiceFilter: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Containers) != 1 || filtered.Containers[0].Service != "web" {
		t.Fatalf("filter web: %+v", filtered.Containers)
	}

	// Filter also matches container name substring.
	byName, _ := srv.ListContainers(context.Background(), &pb.ListRequest{ServiceFilter: "standalone"})
	if len(byName.Containers) != 1 || byName.Containers[0].Name != "standalone" {
		t.Fatalf("filter by name: %+v", byName.Containers)
	}
}

func TestListContainers_Error(t *testing.T) {
	d := newFakeDocker()
	d.listErr = errors.New("docker down")
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	if _, err := srv.ListContainers(context.Background(), &pb.ListRequest{}); statusCode(err) != codes.Internal {
		t.Fatalf("want Internal, got %v", err)
	}
}

// Discovery must be refusable. It used to be the one RPC the Authorizer never
// saw, so a policy could restrict exec and logs while still handing out every
// container id and service name needed to aim them.
func TestListContainers_HonoursTheAuthorizer(t *testing.T) {
	d := newFakeDocker()
	d.containers = []types.Container{{ID: "a1", Names: []string{"/web.1"}}}
	srv, m := newTestServer(d, denyAuth{}, Options{})

	if _, err := srv.ListContainers(context.Background(), &pb.ListRequest{}); statusCode(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if m.denied.Load() != 1 {
		t.Errorf("denial not counted: %d", m.denied.Load())
	}
	// A refused enumeration must not reach Docker at all — otherwise the answer
	// was computed and merely withheld.
	if d.listCalls.Load() != 0 {
		t.Errorf("Docker was queried despite the denial (%d calls)", d.listCalls.Load())
	}
}

// The action name is what a policy is written against, so it is part of the
// contract, not an implementation detail.
func TestListContainers_AuthorizerSeesTheAction(t *testing.T) {
	d := newFakeDocker()
	var got auth.Request
	az := authFunc(func(_ context.Context, r auth.Request) auth.Decision {
		got = r
		return auth.Decision{Allow: true}
	})
	srv, _ := newTestServer(d, az, Options{})

	if _, err := srv.ListContainers(context.Background(), &pb.ListRequest{}); err != nil {
		t.Fatal(err)
	}
	if got.Action != "container.list" {
		t.Errorf("action = %q, want container.list", got.Action)
	}
	if got.Identity != "test-user" {
		t.Errorf("identity = %q, want test-user", got.Identity)
	}
}

// The audit gap was the part that bit today, with AllowAll in place: mapping
// every container on every node left no record whatsoever.
func TestListContainers_IsAudited(t *testing.T) {
	d := newFakeDocker()
	d.containers = []types.Container{
		{ID: "a1", Names: []string{"/web.1"}, Labels: map[string]string{swarmServiceLabel: "web"}},
		{ID: "b2", Names: []string{"/db.1"}, Labels: map[string]string{swarmServiceLabel: "db"}},
	}
	srv, log := auditedTestServer(d, auth.AllowAll{})

	if _, err := srv.ListContainers(context.Background(), &pb.ListRequest{ServiceFilter: "web"}); err != nil {
		t.Fatal(err)
	}

	rec := findAuditEvent(t, log.String(), "container_list")
	if rec["identity"] != "test-user" {
		t.Errorf("identity = %v", rec["identity"])
	}
	if rec["filter"] != "web" {
		t.Errorf("filter = %v, want web", rec["filter"])
	}
	if rec["matched"] != float64(1) {
		t.Errorf("matched = %v, want 1", rec["matched"])
	}
	// The listing itself must never be in the record: one refresh cycle would
	// otherwise write the node's whole topology into the audit log.
	if strings.Contains(log.String(), "a1") {
		t.Error("audit record leaks container ids")
	}
}

// helpers

func lastExitCode(t *testing.T, stream *fakeStream) int32 {
	t.Helper()
	for _, m := range stream.sentMessages() {
		if p, ok := m.Payload.(*pb.ServerMessage_ExitCode); ok {
			return p.ExitCode
		}
	}
	t.Fatal("no exit_code message sent")
	return 0
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
