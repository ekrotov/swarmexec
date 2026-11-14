// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmfake"
	pb "swarmexec/internal/pb"
)

// logAgent answers Logs over real gRPC and TLS: two stdout lines and one
// stderr line per container, then the end of the stream — a container that
// exited.
type logAgent struct {
	pb.UnimplementedAgentServer

	mu   sync.Mutex
	reqs []*pb.LogsRequest
}

func (a *logAgent) Logs(req *pb.LogsRequest, stream pb.Agent_LogsServer) error {
	a.mu.Lock()
	a.reqs = append(a.reqs, req)
	a.mu.Unlock()
	id := req.GetContainerId()
	for _, c := range []*pb.LogChunk{
		{Payload: &pb.LogChunk_Stdout{Stdout: []byte("hello from " + id + "\n")}},
		{Payload: &pb.LogChunk_Stderr{Stderr: []byte("oops in " + id + "\n")}},
		{Payload: &pb.LogChunk_Stdout{Stdout: []byte("bye from " + id + "\n")}},
	} {
		if err := stream.Send(c); err != nil {
			return err
		}
	}
	return nil
}

func (a *logAgent) requests() []*pb.LogsRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*pb.LogsRequest(nil), a.reqs...)
}

// testRun starts the fake agent and an empty fake manager, and returns a Run
// wired to both, writing into out/errOut.
func testRun(t *testing.T, p Params, out, errOut io.Writer) (Run, *logAgent) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{swarmfake.SelfSignedPair(t)}})))
	agent := &logAgent{}
	pb.RegisterAgentServer(gs, agent)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	api := httptest.NewServer(&swarmfake.Swarm{})
	t.Cleanup(api.Close)
	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dcli.Close() })

	addr := lis.Addr().String()
	ac := agentcall.New(config.Config{
		Port:     7000,
		Insecure: true,
		ProxyDialer: func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
	})
	t.Cleanup(ac.Close)
	if p.ConnectTimeout == 0 {
		p.ConnectTimeout = 3 * time.Second
	}
	return Run{Agents: ac, Resolver: resolve.New(dcli, resolve.AddrHostname), Params: p, Stdout: out, Stderr: errOut}, agent
}

func ep(container string) resolve.Endpoint {
	return resolve.Endpoint{DialHost: "node-a", ContainerID: container, NodeName: "node-a"}
}

// One container's stream lands on the two writers it came from, and the
// request carries the run's parameters.
func TestStreamSplitsStdoutAndStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	run, agent := testRun(t, Params{Tail: 50, Timestamps: true, Since: 2 * time.Minute}, &out, &errOut)

	if err := Stream(context.Background(), run, ep("c1")); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "hello from c1\nbye from c1\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := errOut.String(); got != "oops in c1\n" {
		t.Errorf("stderr = %q", got)
	}
	reqs := agent.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d Logs calls, want 1", len(reqs))
	}
	if r := reqs[0]; r.GetContainerId() != "c1" || r.GetTail() != 50 || !r.GetTimestamps() || r.GetSinceSeconds() != 120 || r.GetFollow() {
		t.Errorf("request = %+v, want c1, tail 50, timestamps, since 120s, no follow", r)
	}
}

// Following a target whose service is gone ends cleanly, and says so, once
// the container's stream ends.
func TestFollowStopsWhenTheTargetIsGone(t *testing.T) {
	var out bytes.Buffer
	run, agent := testRun(t, Params{Follow: true, Tail: 10}, &out, io.Discard)

	var notes []string
	err := Follow(context.Background(), run, resolve.FollowTarget{Service: "web", Slot: 1}, ep("c1"),
		func(msg string) { notes = append(notes, msg) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hello from c1") {
		t.Errorf("stdout = %q, want the container's lines", out.String())
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "no running container left") {
		t.Errorf("notes = %q, want one saying the target is gone", notes)
	}
	if n := len(agent.requests()); n != 1 {
		t.Errorf("got %d Logs calls, want 1 (nothing to reconnect to)", n)
	}
}

// Several sources meet in the shared sinks one whole line at a time, each
// line carrying its source's prefix, through the filter chain wrap builds.
func TestStreamManyPrefixesEverySource(t *testing.T) {
	var out, errOut bytes.Buffer
	run, _ := testRun(t, Params{Tail: 10}, &out, &errOut)
	sources := []Source{
		{Label: "web.1", Endpoint: ep("c1")},
		{Label: "web.2", Endpoint: ep("c2")},
	}
	var wrapped int
	var mu sync.Mutex
	wrap := func(dst io.Writer) (io.Writer, func()) {
		mu.Lock()
		wrapped++
		mu.Unlock()
		return dst, func() {}
	}

	if err := StreamMany(context.Background(), run, sources, false, wrap); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"web.1", "hello from c1", "web.2", "bye from c2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.Contains(line, "web.1") && !strings.Contains(line, "web.2") {
			t.Errorf("line without a source prefix: %q", line)
		}
	}
	if !strings.Contains(errOut.String(), "oops in c2") {
		t.Errorf("stderr = %q, want the second source's stderr", errOut.String())
	}
	if wrapped != 4 {
		t.Errorf("wrap called %d times, want once per source and stream (4)", wrapped)
	}
}

// A source whose agent cannot be reached is reported, and the error counts it.
func TestStreamManyReportsAFailedSource(t *testing.T) {
	var out, errOut bytes.Buffer
	run, _ := testRun(t, Params{ConnectTimeout: 200 * time.Millisecond}, &out, &errOut)
	run.Agents = agentcall.New(config.Config{
		Port:     7000,
		Insecure: true,
		ProxyDialer: func(context.Context, string) (net.Conn, error) {
			return nil, io.ErrUnexpectedEOF
		},
	})
	t.Cleanup(run.Agents.Close)

	err := StreamMany(context.Background(), run, []Source{{Label: "web.1", Endpoint: ep("c1")}}, false,
		func(dst io.Writer) (io.Writer, func()) { return dst, func() {} })
	if err == nil || !strings.Contains(err.Error(), "1 of 1 log streams failed") {
		t.Fatalf("err = %v, want the failed source counted", err)
	}
	if !strings.Contains(errOut.String(), "stream ended") {
		t.Errorf("stderr = %q, want the failure in the source's own lines", errOut.String())
	}
}
