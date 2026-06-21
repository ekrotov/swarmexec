package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	pb "swarmexec/internal/pb"
)

type fakeStream struct {
	mu        sync.Mutex
	sent      []*pb.ClientMessage
	script    []*pb.ServerMessage
	idx       int
	recvErr   error
	closeSent bool
}

func (s *fakeStream) Send(m *pb.ClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
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
