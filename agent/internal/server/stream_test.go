// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"sync"

	"google.golang.org/grpc/metadata"

	"swarmexec/internal/pb"
)

// recvItem is one queued client->server event for the fake stream.
type recvItem struct {
	msg *pb.ClientMessage
	err error
}

// fakeStream implements pb.Agent_ExecServer for tests, backed by channels.
type fakeStream struct {
	ctx    context.Context
	recvCh chan recvItem

	mu   sync.Mutex
	sent []*pb.ServerMessage
}

func newFakeStream(ctx context.Context) *fakeStream {
	return &fakeStream{ctx: ctx, recvCh: make(chan recvItem, 64)}
}

// queueStart enqueues the initial StartExec message.
func (s *fakeStream) queueStart(se *pb.StartExec) {
	s.recvCh <- recvItem{msg: &pb.ClientMessage{Payload: &pb.ClientMessage_Start{Start: se}}}
}

func (s *fakeStream) queueStdin(b []byte) {
	s.recvCh <- recvItem{msg: &pb.ClientMessage{Payload: &pb.ClientMessage_Stdin{Stdin: b}}}
}

func (s *fakeStream) queueResize(w, h uint32) {
	s.recvCh <- recvItem{msg: &pb.ClientMessage{Payload: &pb.ClientMessage_Resize{Resize: &pb.Resize{Width: w, Height: h}}}}
}

// queueEOF signals client half-close (CloseSend).
func (s *fakeStream) queueEOF() { s.recvCh <- recvItem{err: io.EOF} }

// queueErr signals a client disconnect / stream error.
func (s *fakeStream) queueErr(err error) { s.recvCh <- recvItem{err: err} }

func (s *fakeStream) Recv() (*pb.ClientMessage, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case item := <-s.recvCh:
		return item.msg, item.err
	}
}

func (s *fakeStream) Send(m *pb.ServerMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

// sentMessages returns a snapshot of everything sent so far.
func (s *fakeStream) sentMessages() []*pb.ServerMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*pb.ServerMessage, len(s.sent))
	copy(out, s.sent)
	return out
}

// grpc.ServerStream surface (unused by the agent logic).
func (s *fakeStream) Context() context.Context     { return s.ctx }
func (s *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeStream) SetTrailer(metadata.MD)       {}
func (s *fakeStream) SendMsg(interface{}) error    { return nil }
func (s *fakeStream) RecvMsg(interface{}) error    { return nil }

var _ pb.Agent_ExecServer = (*fakeStream)(nil)
