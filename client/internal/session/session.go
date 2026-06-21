// Package session bridges the operator's terminal to the agent's Exec stream.
// It implements the normative session lifecycle and framing from CONTRACT.md
// §4–§6 and the terminal/exit semantics from REQUIREMENTS §5–§6.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"google.golang.org/grpc/status"

	pb "swarmexec/internal/pb"
)

// TransportFailure is the exit code reserved for client/transport/agent errors,
// mirroring docker's use of 125. It is distinct from any exit code the remote
// command itself returns, so scripts can tell the two apart (REQUIREMENTS §6).
const TransportFailure = 125

// AgentError is a terminal error reported by the agent over the stream (as
// opposed to a gRPC status error or a remote command exit code).
type AgentError struct{ Msg string }

func (e *AgentError) Error() string { return "agent error: " + e.Msg }

// Options configures a single exec session.
type Options struct {
	// Start is the StartExec payload (first message). The caller fills
	// container ID, cmd, tty, env, working dir, user, and the initial width/
	// height.
	Start *pb.StartExec

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// ResizeEvents fires on terminal resize; SizeFn reads the new size. Both
	// nil for non-interactive sessions.
	ResizeEvents <-chan os.Signal
	SizeFn       func() (width, height uint32, err error)
}

// Stream is the bidirectional Exec stream (subset of pb.Agent_ExecClient).
type Stream interface {
	Send(*pb.ClientMessage) error
	Recv() (*pb.ServerMessage, error)
	CloseSend() error
}

// Run drives the session to completion and returns the process exit code. On
// transport/agent errors it returns TransportFailure plus a descriptive error.
// Run does not touch the terminal; the caller owns raw-mode setup/restore.
func Run(ctx context.Context, stream Stream, opts Options) (int, error) {
	if opts.Start == nil {
		return TransportFailure, errors.New("internal: nil StartExec")
	}

	// First message MUST carry StartExec (CONTRACT.md §4.2).
	if err := stream.Send(&pb.ClientMessage{
		Payload: &pb.ClientMessage_Start{Start: opts.Start},
	}); err != nil {
		// A server-side rejection before the handler runs (e.g. the agent's
		// shared-secret interceptor) closes the stream, so Send returns io.EOF;
		// the real status (e.g. Unauthenticated) is delivered on Recv. Surface
		// that instead of a bare "EOF".
		if errors.Is(err, io.EOF) {
			if _, rerr := stream.Recv(); rerr != nil {
				err = rerr
			}
		}
		return TransportFailure, fmt.Errorf("start exec: %w", wrapStatus(err))
	}

	snd := &sender{stream: stream}

	// stdin → agent, half-closing on EOF.
	if opts.Stdin != nil {
		go pumpStdin(opts.Stdin, snd)
	}

	// resize → agent.
	if opts.ResizeEvents != nil && opts.SizeFn != nil {
		go pumpResize(opts.ResizeEvents, opts.SizeFn, snd)
	}

	// agent → local stdout/stderr; ends on exit_code or error.
	return recvLoop(stream, opts)
}

func recvLoop(stream Stream, opts Options) (int, error) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return TransportFailure, errors.New("stream closed before exit code was received")
			}
			return TransportFailure, wrapStatus(err)
		}
		switch p := msg.Payload.(type) {
		case *pb.ServerMessage_Stdout:
			if _, err := opts.Stdout.Write(p.Stdout); err != nil {
				return TransportFailure, fmt.Errorf("write stdout: %w", err)
			}
		case *pb.ServerMessage_Stderr:
			if _, err := opts.Stderr.Write(p.Stderr); err != nil {
				return TransportFailure, fmt.Errorf("write stderr: %w", err)
			}
		case *pb.ServerMessage_ExitCode:
			return int(p.ExitCode), nil
		case *pb.ServerMessage_Error:
			return TransportFailure, &AgentError{Msg: p.Error}
		default:
			// Unknown payload — ignore for forward compatibility.
		}
	}
}

func pumpStdin(r io.Reader, snd *sender) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			// Buffer-safety rule (CONTRACT.md §6): copy before placing in a
			// protobuf message, because gRPC serializes asynchronously.
			cp := make([]byte, n)
			copy(cp, buf[:n])
			if serr := snd.send(&pb.ClientMessage{
				Payload: &pb.ClientMessage_Stdin{Stdin: cp},
			}); serr != nil {
				return
			}
		}
		if err != nil {
			// EOF or read error → half-close so the agent sees stdin EOF
			// (close-write only; output stays open — CONTRACT.md §4.6).
			_ = snd.closeSend()
			return
		}
	}
}

func pumpResize(events <-chan os.Signal, sizeFn func() (uint32, uint32, error), snd *sender) {
	for range events {
		w, h, err := sizeFn()
		if err != nil {
			continue
		}
		if serr := snd.send(&pb.ClientMessage{
			Payload: &pb.ClientMessage_Resize{Resize: &pb.Resize{Width: w, Height: h}},
		}); serr != nil {
			return
		}
	}
}

// sender serializes access to the stream's send side. gRPC streams permit
// concurrent Send and Recv (opposite directions) but not concurrent Sends, so
// the stdin and resize goroutines share this lock.
type sender struct {
	mu     sync.Mutex
	stream Stream
	closed bool
}

func (s *sender) send(m *pb.ClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("send after close")
	}
	return s.stream.Send(m)
}

func (s *sender) closeSend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.stream.CloseSend()
}

// wrapStatus turns a gRPC status error into a readable message while preserving
// the underlying error for callers that want it.
func wrapStatus(err error) error {
	if st, ok := status.FromError(err); ok {
		return fmt.Errorf("%s: %s", st.Code(), st.Message())
	}
	return err
}
