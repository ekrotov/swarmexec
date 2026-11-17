// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "swarmexec/internal/pb"
)

// Tests for the pieces split out in Q-01 round 5 that the end-to-end tests
// reach only on their happy path.

// forwardAgent answers PortForward by target port: 80 echoes upper-cased
// until the client half-closes, 81 fails after ready, 1 refuses, 2 sends data
// before ready.
type forwardAgent struct {
	pb.UnimplementedAgentServer
}

func (forwardAgent) PortForward(s pb.Agent_PortForwardServer) error {
	start, err := s.Recv()
	if err != nil {
		return err
	}
	data := func(b []byte) *pb.ForwardServerMessage {
		return &pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Data{Data: b}}
	}
	fail := func(msg string) *pb.ForwardServerMessage {
		return &pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Error{Error: msg}}
	}
	switch start.GetStart().GetPort() {
	case 1:
		return s.Send(fail("connection refused"))
	case 2:
		return s.Send(data([]byte("early")))
	}
	if err := s.Send(&pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Ready{Ready: &pb.ForwardReady{}}}); err != nil {
		return err
	}
	if start.GetStart().GetPort() == 81 {
		return s.Send(fail("reset by peer"))
	}
	for {
		m, err := s.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.Send(data([]byte(strings.ToUpper(string(m.GetData()))))); err != nil {
			return err
		}
	}
}

func forwardAgentClient(t *testing.T) pb.AgentClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	pb.RegisterAgentServer(gs, forwardAgent{})
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return pb.NewAgentClient(cc)
}

func TestForwardConn(t *testing.T) {
	ac := forwardAgentClient(t)
	ctx := context.Background()

	// The happy path: bytes go both ways, and the local side closing ends
	// the bridge without an error.
	local, peer := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- forwardConn(ctx, ac, local, "c1", 80) }()
	if _, err := peer.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(peer, buf); err != nil || string(buf) != "HELLO" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	peer.Close()
	if err := <-done; err != nil {
		t.Errorf("closing the local side: %v, want nil", err)
	}

	for _, tc := range []struct {
		port uint32
		want string
	}{
		{1, "connection refused"},
		{2, "before ready"},
		{81, "reset by peer"},
	} {
		local, peer := net.Pipe()
		err := forwardConn(ctx, ac, local, "c1", tc.port)
		peer.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("port %d: err = %v, want it to contain %q", tc.port, err, tc.want)
		}
	}
}

func TestRolloutLine(t *testing.T) {
	task := func(desired, state swarm.TaskState, errMsg, msg string) swarm.Task {
		var tk swarm.Task
		tk.DesiredState, tk.Status.State, tk.Status.Err, tk.Status.Message = desired, state, errMsg, msg
		return tk
	}
	run, shut := swarm.TaskStateRunning, swarm.TaskStateShutdown
	for _, tc := range []struct {
		name     string
		tasks    []swarm.Task
		wantMsg  string
		wantDone bool
	}{
		{"nothing yet", nil, "0/0 running", false},
		{"all running", []swarm.Task{task(run, run, "", ""), task(run, run, "", "")}, "2/2 running", true},
		{"failed task's error", []swarm.Task{task(run, run, "", ""), task(run, swarm.TaskStateRejected, "port in use", "x")},
			"1/2 running — port in use", false},
		{"message when no error", []swarm.Task{task(run, swarm.TaskStateFailed, "", "exit 1")}, "0/1 running — exit 1", false},
		// A failure of a task no longer wanted says nothing once the rest run.
		{"old failure, all running", []swarm.Task{task(run, run, "", ""), task(shut, swarm.TaskStateFailed, "boom", "")},
			"1/1 running", true},
	} {
		msg, done := rolloutLine(tc.tasks)
		if msg != tc.wantMsg || done != tc.wantDone {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, msg, done, tc.wantMsg, tc.wantDone)
		}
	}
}
