// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "swarmexec/internal/pb"
)

type fakeStream struct {
	mu        sync.Mutex
	sent      []*pb.ClientMessage
	script    []*pb.ServerMessage
	idx       int
	recvErr   error
	sendErr   error
	closeSent bool
}

func (s *fakeStream) Send(m *pb.ClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return s.sendErr
}

func (s *fakeStream) Recv() (*pb.ServerMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx < len(s.script) {
		m := s.script[s.idx]
		s.idx++
		return m, nil
	}
	if s.recvErr != nil {
		return nil, s.recvErr
	}
	return nil, io.EOF
}

func (s *fakeStream) CloseSend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeSent = true
	return nil
}

func srvStdout(b string) *pb.ServerMessage {
	return &pb.ServerMessage{Payload: &pb.ServerMessage_Stdout{Stdout: []byte(b)}}
}
func srvExit(c int32) *pb.ServerMessage {
	return &pb.ServerMessage{Payload: &pb.ServerMessage_ExitCode{ExitCode: c}}
}
func srvErr(m string) *pb.ServerMessage {
	return &pb.ServerMessage{Payload: &pb.ServerMessage_Error{Error: m}}
}

func baseOpts(stdout, stderr *bytes.Buffer) Options {
	return Options{
		Start:  &pb.StartExec{ContainerId: "abc", Cmd: []string{"/bin/sh"}, Tty: true},
		Stdin:  strings.NewReader(""), // immediate EOF
		Stdout: stdout,
		Stderr: stderr,
	}
}

func TestRunPropagatesExitCode(t *testing.T) {
	fs := &fakeStream{script: []*pb.ServerMessage{srvStdout("hello"), srvExit(7)}}
	var out, errb bytes.Buffer

	code, err := Run(context.Background(), fs, baseOpts(&out, &errb))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 7 {
		t.Errorf("code = %d, want 7", code)
	}
	if out.String() != "hello" {
		t.Errorf("stdout = %q, want hello", out.String())
	}
}

func TestRunSurfacesStatusOnSendEOF(t *testing.T) {
	// A server-side rejection (e.g. bad shared secret) makes Send return io.EOF
	// while the real status arrives on Recv. Run should surface the status, not
	// a bare "EOF".
	fs := &fakeStream{
		sendErr: io.EOF,
		recvErr: status.Error(codes.Unauthenticated, "invalid or missing agent secret"),
	}
	var out, errb bytes.Buffer
	code, err := Run(context.Background(), fs, baseOpts(&out, &errb))
	if code != TransportFailure {
		t.Errorf("code = %d, want %d", code, TransportFailure)
	}
	if err == nil || !strings.Contains(err.Error(), "Unauthenticated") {
		t.Fatalf("error should surface the Unauthenticated status, got %v", err)
	}
}

func TestRunFirstMessageIsStartExec(t *testing.T) {
	fs := &fakeStream{script: []*pb.ServerMessage{srvExit(0)}}
	var out, errb bytes.Buffer

	if _, err := Run(context.Background(), fs, baseOpts(&out, &errb)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.sent) == 0 {
		t.Fatal("nothing sent")
	}
	if _, ok := fs.sent[0].Payload.(*pb.ClientMessage_Start); !ok {
		t.Fatalf("first message is %T, want StartExec", fs.sent[0].Payload)
	}
}

func TestRunAgentError(t *testing.T) {
	fs := &fakeStream{script: []*pb.ServerMessage{srvErr("exec denied")}}
	var out, errb bytes.Buffer

	code, err := Run(context.Background(), fs, baseOpts(&out, &errb))
	if code != TransportFailure {
		t.Errorf("code = %d, want %d", code, TransportFailure)
	}
	var ae *AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("want *AgentError, got %v", err)
	}
	if ae.Msg != "exec denied" {
		t.Errorf("msg = %q, want 'exec denied'", ae.Msg)
	}
}

func TestRunStreamClosedBeforeExit(t *testing.T) {
	fs := &fakeStream{script: []*pb.ServerMessage{srvStdout("partial")}} // then EOF, no exit
	var out, errb bytes.Buffer

	code, err := Run(context.Background(), fs, baseOpts(&out, &errb))
	if code != TransportFailure {
		t.Errorf("code = %d, want %d", code, TransportFailure)
	}
	if err == nil {
		t.Fatal("want error for stream closed before exit code")
	}
}

// TestPumpStdinCopiesAndCloses verifies the buffer-safety rule (CONTRACT.md §6)
// and half-close on EOF (CONTRACT.md §4.6).
func TestPumpStdinCopiesAndCloses(t *testing.T) {
	fs := &fakeStream{}
	snd := &sender{stream: fs}

	pumpStdin(strings.NewReader("abcdef"), snd)

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.closeSent {
		t.Error("CloseSend was not called on stdin EOF")
	}
	var got []byte
	for _, m := range fs.sent {
		if sm, ok := m.Payload.(*pb.ClientMessage_Stdin); ok {
			got = append(got, sm.Stdin...)
		}
	}
	if string(got) != "abcdef" {
		t.Errorf("stdin forwarded = %q, want abcdef", string(got))
	}
}

func TestSenderNoSendAfterClose(t *testing.T) {
	fs := &fakeStream{}
	snd := &sender{stream: fs}
	if err := snd.closeSend(); err != nil {
		t.Fatal(err)
	}
	if err := snd.send(&pb.ClientMessage{}); err == nil {
		t.Error("send after close should fail")
	}
}

// The readable message must not cost the status. This used to build the text
// with fmt.Errorf("%s: %s", …) and no %w, so every downstream status.Code saw
// Unknown — agentTooOld stopped matching, and so did the hint that explains a
// rejected shared secret. It was only caught against a real cluster running
// older agents, because the unit tests all called the classifier directly.
func TestWrapStatus_KeepsTheCodeInspectable(t *testing.T) {
	orig := status.Error(codes.Unauthenticated, "invalid or missing agent secret")
	wrapped := WrapStatus(orig)

	if got := wrapped.Error(); got != "Unauthenticated: invalid or missing agent secret" {
		t.Errorf("message = %q", got)
	}
	if got := status.Code(wrapped); got != codes.Unauthenticated {
		t.Errorf("status.Code = %v, want Unauthenticated — the code must survive wrapping", got)
	}
	// And it must still survive one more layer, which is how callers pass it on.
	outer := fmt.Errorf("open logs stream: %w", wrapped)
	if got := status.Code(outer); got != codes.Unauthenticated {
		t.Errorf("status.Code through an outer wrap = %v, want Unauthenticated", got)
	}
}

// A non-status error passes through untouched, so nothing gains a misleading
// "Unknown:" prefix.
func TestWrapStatus_LeavesPlainErrorsAlone(t *testing.T) {
	plain := errors.New("dial tcp: connection refused")
	if got := WrapStatus(plain); got != plain {
		t.Errorf("plain error was rewritten: %v", got)
	}
}
