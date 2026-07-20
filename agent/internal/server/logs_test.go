// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// fakeLogsStream captures LogChunks the server sends.
type fakeLogsStream struct {
	ctx  context.Context
	mu   sync.Mutex
	sent []*pb.LogChunk
}

func (s *fakeLogsStream) Send(c *pb.LogChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, c)
	return nil
}
func (s *fakeLogsStream) Context() context.Context     { return s.ctx }
func (s *fakeLogsStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeLogsStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeLogsStream) SetTrailer(metadata.MD)       {}
func (s *fakeLogsStream) SendMsg(any) error            { return nil }
func (s *fakeLogsStream) RecvMsg(any) error            { return nil }

func (s *fakeLogsStream) collect() (stdout, stderr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.sent {
		switch p := c.Payload.(type) {
		case *pb.LogChunk_Stdout:
			stdout += string(p.Stdout)
		case *pb.LogChunk_Stderr:
			stderr += string(p.Stderr)
		}
	}
	return
}

func TestLogs_NonTTYDemux(t *testing.T) {
	d := newFakeDocker()
	var buf bytes.Buffer
	_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte("out-line\n"))
	_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stderr).Write([]byte("err-line\n"))
	d.logsReader = io.NopCloser(&buf)

	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	stream := &fakeLogsStream{ctx: context.Background()}
	if err := srv.Logs(&pb.LogsRequest{ContainerId: "c1"}, stream); err != nil {
		t.Fatalf("Logs error: %v", err)
	}
	out, errs := stream.collect()
	if out != "out-line\n" {
		t.Errorf("stdout = %q, want out-line", out)
	}
	if errs != "err-line\n" {
		t.Errorf("stderr = %q, want err-line", errs)
	}
}

func TestLogs_TTYRaw(t *testing.T) {
	d := newFakeDocker()
	d.inspect["c1"] = types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{ID: "c1"},
		Config:            &container.Config{Tty: true},
	}
	d.logsReader = io.NopCloser(strings.NewReader("raw-tty-line\n"))

	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	stream := &fakeLogsStream{ctx: context.Background()}
	if err := srv.Logs(&pb.LogsRequest{ContainerId: "c1"}, stream); err != nil {
		t.Fatalf("Logs error: %v", err)
	}
	out, errs := stream.collect()
	if out != "raw-tty-line\n" {
		t.Errorf("stdout = %q, want raw-tty-line", out)
	}
	if errs != "" {
		t.Errorf("TTY logs must not emit stderr, got %q", errs)
	}
}

func TestLogs_AuthorizationDenied(t *testing.T) {
	d := newFakeDocker()
	srv, m := newTestServer(d, denyAuth{}, Options{})
	stream := &fakeLogsStream{ctx: context.Background()}
	err := srv.Logs(&pb.LogsRequest{ContainerId: "c1"}, stream)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if m.denied.Load() != 1 {
		t.Errorf("AuthDenied metric = %d, want 1", m.denied.Load())
	}
}

func TestLogs_RequiresContainerID(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	stream := &fakeLogsStream{ctx: context.Background()}
	if err := srv.Logs(&pb.LogsRequest{}, stream); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}
