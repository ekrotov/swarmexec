// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// inspectTimeout bounds the best-effort exec inspect performed after output
// ends, using a context independent of the (possibly cancelled) stream.
const inspectTimeout = 5 * time.Second

// Exec runs the bidirectional interactive exec bridge defined in CONTRACT.md §4.
// The drain check, the stream slot and the drain accounting happen before it
// runs, in the stream gate (streamgate.go).
func (s *Server) Exec(stream pb.Agent_ExecServer) error {
	// (1) The first message MUST be StartExec (CONTRACT §4.2).
	first, err := stream.Recv()
	if err != nil {
		if err == io.EOF {
			return status.Error(codes.InvalidArgument, "stream closed before StartExec")
		}
		return err
	}
	start, ok := first.Payload.(*pb.ClientMessage_Start)
	if !ok || start.Start == nil {
		return status.Error(codes.InvalidArgument, "first message must be StartExec")
	}
	se := start.Start
	if se.GetContainerId() == "" || len(se.GetCmd()) == 0 {
		return status.Error(codes.InvalidArgument, "StartExec requires container_id and cmd")
	}

	// (2) Identity, service and authorization — the shared per-RPC gate. Deny
	// before creating the exec (REQUIREMENTS §5).
	a, err := s.authorize(stream.Context(), auth.Request{
		Action:      "exec",
		ContainerID: se.GetContainerId(),
		Cmd:         se.GetCmd(),
		User:        se.GetUser(),
		TTY:         se.GetTty(),
	})
	if err != nil {
		return err
	}

	return s.runSession(stream, se, a.Identity, a.Service)
}

// runSession creates the exec, attaches, and bridges both directions until the
// process exits, the client disconnects, or a timeout fires.
func (s *Server) runSession(stream pb.Agent_ExecServer, se *pb.StartExec, identity, service string) error {
	s.metrics.SessionStarted()
	start := time.Now()

	clientAddr := peerAddr(stream.Context())
	s.audit.SessionStart(identity, se.GetContainerId(), service, se.GetCmd(), se.GetTty(), clientAddr)

	// (5) Create the exec.
	execCfg := execCreateOptions(se)
	idResp, err := s.docker.ExecCreate(stream.Context(), se.GetContainerId(), execCfg)
	if err != nil {
		return s.failSession(stream, identity, se.GetContainerId(), start, "exec create failed", err)
	}
	execID := idResp.ID

	attach, err := s.docker.ExecAttach(stream.Context(), execID, client.ExecAttachOptions{
		TTY:         se.GetTty(),
		ConsoleSize: execCfg.ConsoleSize,
	})
	if err != nil {
		return s.failSession(stream, identity, se.GetContainerId(), start, "exec attach failed", err)
	}

	// Single place that closes the hijacked connection; safe to call repeatedly.
	var closeOnce sync.Once
	closeAttach := func() { closeOnce.Do(attach.Close) }
	defer closeAttach()

	// Session context: cancelled on normal end, client disconnect, or timeout.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	if s.opts.MaxSessionTime > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, s.opts.MaxSessionTime)
		defer stop()
	}

	s.initialResize(ctx, execID, se)

	tally := tallyFrom(stream.Context())
	bytesIn, bytesOut := &tally.in, &tally.out
	var idleHit, maxHit atomic.Bool

	// Activity tracker for the idle timeout (no-op when disabled).
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	touch := func() { lastActivity.Store(time.Now().UnixNano()) }

	// Closing the hijacked conn unblocks the output reader on cancellation
	// (client disconnect, timeout) so io.Copy/StdCopy returns promptly.
	go func() {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			maxHit.Store(true)
		}
		closeAttach()
	}()

	if s.opts.IdleTimeout > 0 {
		go s.idleMonitor(ctx, &lastActivity, &idleHit, cancel)
	}

	// Client -> container pump (stdin + resize). Runs until the client
	// half-closes, disconnects, or the session context is cancelled.
	go s.pumpInput(ctx, stream, &attach.HijackedResponse, execID, bytesIn, touch, cancel)

	// Container -> client copy. Blocks until output ends or the conn is closed.
	copyErr := copyOutput(stream, se.GetTty(), attach.Reader, bytesOut, touch)

	// Stop the input pump and monitors now that output has ended.
	cancel()

	exitCode := s.inspectExit(execID)
	dur := time.Since(start)
	bIn, bOut := bytesIn.Load(), bytesOut.Load()

	clientGone := stream.Context().Err() != nil
	if msg := sessionEndMessage(idleHit.Load(), maxHit.Load(), clientGone, copyErr, exitCode); msg != nil {
		_ = stream.Send(msg)
	}

	s.audit.SessionEnd(identity, se.GetContainerId(), exitCode, dur, bIn, bOut)
	s.metrics.SessionEnded()

	if clientGone && !idleHit.Load() && !maxHit.Load() {
		return status.FromContextError(stream.Context().Err()).Err()
	}
	return nil
}

// execCreateOptions is the exec runSession creates for se: attached on all
// three streams, with the console size when a TTY with a size is asked for.
func execCreateOptions(se *pb.StartExec) client.ExecCreateOptions {
	execCfg := client.ExecCreateOptions{
		User:         se.GetUser(),
		TTY:          se.GetTty(),
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          se.GetEnv(),
		WorkingDir:   se.GetWorkingDir(),
		Cmd:          se.GetCmd(),
	}
	if se.GetTty() && se.GetWidth() > 0 && se.GetHeight() > 0 {
		execCfg.ConsoleSize = client.ConsoleSize{Height: uint(se.GetHeight()), Width: uint(se.GetWidth())}
	}
	return execCfg
}

// initialResize applies the initial TTY size explicitly after attach
// (REQUIREMENTS §3.1). A failure is only logged.
func (s *Server) initialResize(ctx context.Context, execID string, se *pb.StartExec) {
	if se.GetTty() && se.GetWidth() > 0 && se.GetHeight() > 0 {
		if _, err := s.docker.ExecResize(ctx, execID, client.ExecResizeOptions{
			Height: uint(se.GetHeight()), Width: uint(se.GetWidth()),
		}); err != nil {
			s.log.Warn("initial exec resize failed", "exec_id", execID, "err", err)
		}
	}
}

// copyOutput copies the exec's output to the client until it ends or the
// connection is closed: raw as stdout for a TTY, demultiplexed otherwise.
func copyOutput(stream pb.Agent_ExecServer, tty bool, r io.Reader, bytesOut *atomic.Int64, touch func()) error {
	out := &msgWriter{stream: stream, count: bytesOut, touch: touch}
	if tty {
		_, err := io.Copy(out, r)
		return err
	}
	errOut := &msgWriter{stream: stream, stderr: true, count: bytesOut, touch: touch}
	_, err := stdcopy.StdCopy(out, errOut, r)
	return err
}

// sessionEndMessage is the last message of a session, by why it ended: a
// limit, an output error, or the normal exit code. Nil when the client is
// gone — the stream is gone, nothing to send.
func sessionEndMessage(idleHit, maxHit, clientGone bool, copyErr error, exitCode int) *pb.ServerMessage {
	switch {
	case idleHit:
		return errorMsg("session idle timeout exceeded")
	case maxHit:
		return errorMsg("max session duration exceeded")
	case clientGone:
		return nil
	case copyErr != nil && !errors.Is(copyErr, io.EOF):
		return errorMsg("output stream error: " + copyErr.Error())
	}
	// (7) Normal exit: one exit_code message, then end the stream.
	return &pb.ServerMessage{Payload: &pb.ServerMessage_ExitCode{ExitCode: int32(exitCode)}}
}

// pumpInput forwards client stdin to the exec and applies resize events. On
// client half-close it half-closes the exec stdin (EOF to the process) while
// leaving the output direction open. On disconnect/error it cancels the session.
func (s *Server) pumpInput(ctx context.Context, stream pb.Agent_ExecServer, attach *client.HijackedResponse, execID string, bytesIn *atomic.Int64, touch func(), cancel context.CancelFunc) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				// (6) Client CloseSend -> EOF to the process stdin only.
				if cwErr := attach.CloseWrite(); cwErr != nil {
					s.log.Debug("exec stdin close-write failed", "exec_id", execID, "err", cwErr)
				}
				return
			}
			// Client disconnected or stream errored: tear the session down.
			cancel()
			return
		}
		switch p := msg.Payload.(type) {
		case *pb.ClientMessage_Stdin:
			touch()
			if _, werr := attach.Conn.Write(p.Stdin); werr != nil {
				s.log.Debug("stdin write failed", "exec_id", execID, "err", werr)
				cancel()
				return
			}
			bytesIn.Add(int64(len(p.Stdin)))
		case *pb.ClientMessage_Resize:
			touch()
			if p.Resize.GetWidth() > 0 && p.Resize.GetHeight() > 0 {
				if _, rerr := s.docker.ExecResize(ctx, execID, client.ExecResizeOptions{
					Height: uint(p.Resize.GetHeight()), Width: uint(p.Resize.GetWidth()),
				}); rerr != nil {
					s.log.Debug("exec resize failed", "exec_id", execID, "err", rerr)
				}
			}
		case *pb.ClientMessage_Start:
			// A second StartExec mid-stream is a protocol violation; ignore it.
			s.log.Warn("ignoring unexpected StartExec mid-session", "exec_id", execID)
		}
	}
}

// idleMonitor cancels the session when no activity occurs within IdleTimeout.
func (s *Server) idleMonitor(ctx context.Context, last *atomic.Int64, idleHit *atomic.Bool, cancel context.CancelFunc) {
	// Check at a fraction of the timeout so detection latency stays bounded.
	interval := s.opts.IdleTimeout / 4
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			idle := time.Since(time.Unix(0, last.Load()))
			if idle >= s.opts.IdleTimeout {
				idleHit.Store(true)
				cancel()
				return
			}
		}
	}
}

// inspectExit returns the process exit code, best-effort, using an independent
// context so it still works after the stream context is cancelled.
func (s *Server) inspectExit(execID string) int {
	ictx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()
	info, err := s.docker.ExecInspect(ictx, execID, client.ExecInspectOptions{})
	if err != nil {
		s.log.Warn("exec inspect failed; exit code unknown", "exec_id", execID, "err", err)
		return -1
	}
	if info.Running {
		// Output ended but the daemon still reports the process as running;
		// treat as unknown rather than reporting a bogus 0.
		s.log.Warn("exec still running at output end", "exec_id", execID)
		return -1
	}
	return info.ExitCode
}

// failSession records and returns a session-creation failure, emitting a
// terminal error message to the client and a session-end audit record. The
// in-band message and the status carry the same text; the status code is
// dockerStatus's classification of err (a stopped container is
// FailedPrecondition, a vanished one NotFound), not a blanket Internal.
func (s *Server) failSession(stream pb.Agent_ExecServer, identity, containerID string, start time.Time, verb string, err error) error {
	msg := verb + ": " + err.Error()
	_ = stream.Send(errorMsg(msg))
	s.audit.SessionEnd(identity, containerID, -1, time.Since(start), 0, 0)
	s.metrics.SessionEnded()
	s.log.Error("session setup failed", "container_id", containerID, "reason", msg)
	return dockerStatus(err, verb)
}

// errorMsg builds a terminal error ServerMessage.
func errorMsg(text string) *pb.ServerMessage {
	return &pb.ServerMessage{Payload: &pb.ServerMessage_Error{Error: text}}
}

// msgWriter adapts an io.Writer onto the server stream, copying every byte slice
// out of the caller's (reused) buffer before it enters a protobuf message
// (CONTRACT §6 buffer-safety rule).
type msgWriter struct {
	stream pb.Agent_ExecServer
	stderr bool
	count  *atomic.Int64
	touch  func()
}

func (w *msgWriter) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)

	var msg *pb.ServerMessage
	if w.stderr {
		msg = &pb.ServerMessage{Payload: &pb.ServerMessage_Stderr{Stderr: buf}}
	} else {
		msg = &pb.ServerMessage{Payload: &pb.ServerMessage_Stdout{Stdout: buf}}
	}
	if err := w.stream.Send(msg); err != nil {
		return 0, err
	}
	if w.touch != nil {
		w.touch()
	}
	w.count.Add(int64(len(p)))
	return len(p), nil
}
