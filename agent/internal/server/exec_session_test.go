// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"swarmexec/agent/internal/audit"
	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// sessionServer is newTestServer with the audit stream captured, for the
// session-end tests: each way a session ends must still leave its record.
func sessionServer(d DockerClient, opts Options) (*Server, *testMetrics, *syncBuffer) {
	buf := &syncBuffer{}
	m := &testMetrics{}
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := trackServer(New(d, auth.AllowAll{}, audit.New(slog.New(slog.NewJSONHandler(buf, nil)), true), log, m, opts))
	s.identityFn = func(context.Context) (string, error) { return "test-user", nil }
	return s, m, buf
}

// sessionErrors is the text of every error message the session sent.
func sessionErrors(stream *fakeStream) []string {
	var out []string
	for _, m := range stream.sentMessages() {
		if p, ok := m.Payload.(*pb.ServerMessage_Error); ok {
			out = append(out, p.Error)
		}
	}
	return out
}

// sentExitCode reports whether an exit_code message was sent.
func sentExitCode(stream *fakeStream) bool {
	for _, m := range stream.sentMessages() {
		if _, ok := m.Payload.(*pb.ServerMessage_ExitCode); ok {
			return true
		}
	}
	return false
}

// checkSessionClosed asserts the bookkeeping every session end must do: one
// session_end record with the exit code, and the session metric closed.
func checkSessionClosed(t *testing.T, m *testMetrics, log *syncBuffer, exitCode float64) {
	t.Helper()
	if m.started.Load() != 1 || m.ended.Load() != 1 {
		t.Errorf("session metrics: started=%d ended=%d", m.started.Load(), m.ended.Load())
	}
	if rec := findAuditEvent(t, log.String(), "session_end"); rec["exit_code"] != exitCode {
		t.Errorf("session_end exit_code = %v, want %v", rec["exit_code"], exitCode)
	}
}

func TestExec_MaxSessionTime(t *testing.T) {
	d := newFakeDocker()
	d.exitCode = 137
	srv, m, log := sessionServer(d, Options{MaxSessionTime: 100 * time.Millisecond})
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
			t.Fatalf("a session at its time limit should return nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("max session time did not fire")
	}
	if got := sessionErrors(stream); len(got) != 1 || got[0] != "max session duration exceeded" {
		t.Errorf("errors sent = %q", got)
	}
	if sentExitCode(stream) {
		t.Error("an ended-by-limit session must not report an exit code")
	}
	checkSessionClosed(t, m, log, 137)
}

func TestExec_ClientDisconnect(t *testing.T) {
	d := newFakeDocker()
	srv, m, log := sessionServer(d, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, true))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()
	_ = d.waitAttach()
	cancel() // the client goes away

	select {
	case err := <-errCh:
		if statusCode(err) != codes.Canceled {
			t.Fatalf("want Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end on disconnect")
	}
	if got := stream.sentMessages(); len(got) != 0 {
		t.Errorf("nothing may be sent to a gone client, sent %v", got)
	}
	checkSessionClosed(t, m, log, 0)
}

func TestExec_AttachFailure(t *testing.T) {
	d := newFakeDocker()
	d.attachErr = errors.New("hijack refused")
	srv, m, log := sessionServer(d, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, true))

	if err := srv.Exec(stream); err == nil {
		t.Fatal("a failed attach must fail the RPC")
	}
	if got := sessionErrors(stream); len(got) != 1 || got[0] != "exec attach failed: hijack refused" {
		t.Errorf("errors sent = %q", got)
	}
	checkSessionClosed(t, m, log, -1)
}

func TestExec_OutputStreamError(t *testing.T) {
	d := newFakeDocker()
	d.exitCode = 0
	srv, m, log := sessionServer(d, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"ls"}, false))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()
	conn := d.waitAttach()
	// Not a stdcopy frame: stream type 9 does not exist.
	_, _ = conn.Write([]byte{9, 0, 0, 0, 0, 0, 0, 1, 'x'})
	conn.Close()

	if err := <-errCh; err != nil {
		t.Fatalf("Exec error: %v", err)
	}
	got := sessionErrors(stream)
	if len(got) != 1 || !strings.HasPrefix(got[0], "output stream error: ") {
		t.Errorf("errors sent = %q", got)
	}
	if sentExitCode(stream) {
		t.Error("a broken output stream must not report an exit code")
	}
	checkSessionClosed(t, m, log, 0)
}

// A failed initial resize is only logged: the session runs on.
func TestExec_InitialResizeFailureIsLogged(t *testing.T) {
	d := newFakeDocker()
	d.exitCode = 4
	d.resizeErr = errors.New("no such exec")
	srv, m, log := sessionServer(d, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	se := startExec("c1", []string{"/bin/sh"}, true)
	se.Width, se.Height = 120, 40
	stream.queueStart(se)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()
	conn := d.waitAttach()
	waitFor(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.resizes) == 1
	})
	conn.Close()

	if err := <-errCh; err != nil {
		t.Fatalf("Exec error: %v", err)
	}
	if got := lastExitCode(t, stream); got != 4 {
		t.Errorf("exit code = %d, want 4", got)
	}
	d.mu.Lock()
	r := d.resizes[0]
	d.mu.Unlock()
	if r.Width != 120 || r.Height != 40 {
		t.Errorf("initial resize = %+v", r)
	}
	checkSessionClosed(t, m, log, 4)
}
