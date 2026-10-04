// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"io"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// Logs streams a container's logs to the client (CONTRACT.md §3.1). It authorizes
// like Exec, then forwards Docker's log stream — raw for a TTY container, or
// stdcopy-demultiplexed into stdout/stderr for a non-TTY one.
func (s *Server) Logs(req *pb.LogsRequest, stream pb.Agent_LogsServer) error {
	if s.draining.Load() {
		return status.Error(codes.Unavailable, "agent is shutting down; not accepting new requests")
	}
	if req.GetContainerId() == "" {
		return status.Error(codes.InvalidArgument, "LogsRequest requires container_id")
	}
	release, err := s.streams.acquire()
	if err != nil {
		s.log.Warn("logs refused: stream limit reached", "in_use", s.streams.inUse())
		return err
	}
	defer release()
	ctx := stream.Context()

	identity, service, err := s.authorize(ctx, auth.Request{
		Action:      "logs",
		ContainerID: req.GetContainerId(),
	})
	if err != nil {
		return err
	}

	// A TTY container's log stream is raw; a non-TTY one is stdcopy-multiplexed.
	tty := false
	if res, err := s.docker.ContainerInspect(ctx, req.GetContainerId(), client.ContainerInspectOptions{}); err == nil && res.Container.Config != nil {
		tty = res.Container.Config.Tty
	}

	opts := client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     req.GetFollow(),
		Timestamps: req.GetTimestamps(),
		Tail:       "all",
	}
	if req.GetTail() > 0 {
		opts.Tail = strconv.FormatUint(uint64(req.GetTail()), 10)
	}
	if req.GetSinceSeconds() > 0 {
		opts.Since = strconv.FormatInt(time.Now().Add(-time.Duration(req.GetSinceSeconds())*time.Second).Unix(), 10)
	}

	rc, err := s.docker.ContainerLogs(ctx, req.GetContainerId(), opts)
	if err != nil {
		_ = stream.Send(&pb.LogChunk{Payload: &pb.LogChunk_Error{Error: "container logs: " + err.Error()}})
		s.log.Error("ContainerLogs failed", "container_id", req.GetContainerId(), "err", err)
		return status.Errorf(codes.Internal, "container logs: %v", err)
	}
	defer rc.Close()

	start := time.Now()
	s.audit.LogsStart(identity, req.GetContainerId(), service, req.GetFollow(), peerAddr(ctx))

	var bytesOut atomic.Int64
	out := &logWriter{stream: stream, count: &bytesOut}
	var copyErr error
	if tty {
		_, copyErr = io.Copy(out, rc)
	} else {
		errOut := &logWriter{stream: stream, stderr: true, count: &bytesOut}
		_, copyErr = stdcopy.StdCopy(out, errOut, rc)
	}

	s.audit.LogsEnd(identity, req.GetContainerId(), time.Since(start), bytesOut.Load())

	// A client cancel (ctx done) is a normal end, not an error to report.
	if copyErr != nil && !errors.Is(copyErr, io.EOF) && ctx.Err() == nil {
		_ = stream.Send(&pb.LogChunk{Payload: &pb.LogChunk_Error{Error: "log stream error: " + copyErr.Error()}})
	}
	return nil
}

// logWriter adapts an io.Writer onto the Logs stream, copying every byte slice
// out of the caller's reused buffer before it enters a protobuf message
// (CONTRACT §6 buffer-safety rule).
type logWriter struct {
	stream pb.Agent_LogsServer
	stderr bool
	count  *atomic.Int64
}

func (w *logWriter) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)

	var msg *pb.LogChunk
	if w.stderr {
		msg = &pb.LogChunk{Payload: &pb.LogChunk_Stderr{Stderr: buf}}
	} else {
		msg = &pb.LogChunk{Payload: &pb.LogChunk_Stdout{Stdout: buf}}
	}
	if err := w.stream.Send(msg); err != nil {
		return 0, err
	}
	w.count.Add(int64(len(p)))
	return len(p), nil
}
