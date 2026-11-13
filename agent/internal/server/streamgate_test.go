// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/events"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"swarmexec/agent/internal/auth"
	"swarmexec/agent/internal/metrics"
	"swarmexec/internal/pb"
)

// gateStream is the bare grpc.ServerStream the gate sees in these tests; the
// handler gets the typed fake, re-bound to the context the gate hands down.
type gateStream struct{ ctx context.Context }

func (g gateStream) Context() context.Context   { return g.ctx }
func (gateStream) SetHeader(metadata.MD) error  { return nil }
func (gateStream) SendHeader(metadata.MD) error { return nil }
func (gateStream) SetTrailer(metadata.MD)       {}
func (gateStream) SendMsg(any) error            { return nil }
func (gateStream) RecvMsg(any) error            { return nil }

// viaGate runs call behind the stream gate, as the gRPC server does for method.
// call gets the context the gate hands the handler (it carries the tally).
func viaGate(srv *Server, method string, ctx context.Context, call func(context.Context) error) error {
	return srv.StreamInterceptor()(srv, gateStream{ctx}, &grpc.StreamServerInfo{FullMethod: method},
		func(_ any, ss grpc.ServerStream) error { return call(ss.Context()) })
}

func gatedExec(srv *Server, st *fakeStream) error {
	return viaGate(srv, pb.Agent_Exec_FullMethodName, st.ctx, func(ctx context.Context) error {
		st.ctx = ctx
		return srv.Exec(st)
	})
}

func gatedLogs(srv *Server, req *pb.LogsRequest, st *fakeLogsStream) error {
	return viaGate(srv, pb.Agent_Logs_FullMethodName, st.ctx, func(ctx context.Context) error {
		st.ctx = ctx
		return srv.Logs(req, st)
	})
}

func gatedEvents(srv *Server, req *pb.WatchContainerEventsRequest, st *fakeEventStream) error {
	return viaGate(srv, pb.Agent_WatchContainerEvents_FullMethodName, st.ctx, func(ctx context.Context) error {
		st.ctx = ctx
		return srv.WatchContainerEvents(req, st)
	})
}

func gatedForward(srv *Server, st *fakeForwardStream) error {
	return viaGate(srv, pb.Agent_PortForward_FullMethodName, st.ctx, func(ctx context.Context) error {
		st.ctx = ctx
		return srv.PortForward(st)
	})
}

// A streaming RPC the gate does not know would bypass the drain and the cap.
// The proto is the list of record: every stream in it must be gated, and the
// metric labels must match, so a new streaming RPC cannot slip past either.
func TestStreamGate_CoversEveryStreamingRPC(t *testing.T) {
	var methods []string
	for _, sd := range pb.Agent_ServiceDesc.Streams {
		full := "/" + pb.Agent_ServiceDesc.ServiceName + "/" + sd.StreamName
		g, ok := gatedStreams[full]
		if !ok {
			t.Errorf("streaming RPC %s is not behind the stream gate", full)
			continue
		}
		if g.method != sd.StreamName {
			t.Errorf("%s: metric label %q, want %q", full, g.method, sd.StreamName)
		}
		methods = append(methods, g.method)
	}
	if len(gatedStreams) != len(pb.Agent_ServiceDesc.Streams) {
		t.Errorf("gate lists %d methods, the service has %d streams", len(gatedStreams), len(pb.Agent_ServiceDesc.Streams))
	}
	want := slices.Clone(metrics.StreamMethods)
	slices.Sort(want)
	slices.Sort(methods)
	if !slices.Equal(methods, want) {
		t.Errorf("metrics.StreamMethods = %v, gate labels = %v", want, methods)
	}
}

// Each method keeps the drain wording its clients have always seen, and the
// handler never runs.
func TestStreamGate_RefusesEveryStreamWhileDraining(t *testing.T) {
	srv, m := newTestServer(newFakeDocker(), auth.AllowAll{}, Options{})
	srv.StartDrain()
	for full, noun := range map[string]string{
		pb.Agent_Exec_FullMethodName:                 "sessions",
		pb.Agent_Logs_FullMethodName:                 "requests",
		pb.Agent_PortForward_FullMethodName:          "forwards",
		pb.Agent_WatchContainerEvents_FullMethodName: "watches",
	} {
		ran := false
		err := viaGate(srv, full, context.Background(), func(context.Context) error { ran = true; return nil })
		if statusCode(err) != codes.Unavailable {
			t.Errorf("%s: want Unavailable while draining, got %v", full, err)
		}
		if want := "agent is shutting down; not accepting new " + noun; !strings.Contains(err.Error(), want) {
			t.Errorf("%s: message %q, want %q", full, err.Error(), want)
		}
		if ran {
			t.Errorf("%s: the handler ran during a drain", full)
		}
	}
	if srv.streams.inUse() != 0 || srv.ActiveSessions() != 0 {
		t.Errorf("a refused stream holds a slot (%d) or counts in the drain (%d)", srv.streams.inUse(), srv.ActiveSessions())
	}
	for _, method := range metrics.StreamMethods {
		if started, _ := m.streams(method); started != 0 {
			t.Errorf("%s: a refused stream was counted as started", method)
		}
	}
}

// Unary RPCs — Version, the doctor's pre-auth probe, among them — are not the
// gate's business, and an unknown stream passes through untouched.
func TestStreamGate_LeavesOtherMethodsAlone(t *testing.T) {
	srv, _ := newTestServer(newFakeDocker(), auth.AllowAll{}, Options{MaxStreams: 1})
	srv.StartDrain()
	ran := false
	err := viaGate(srv, "/swarmexec.Agent/SomethingElse", context.Background(), func(context.Context) error { ran = true; return nil })
	if err != nil || !ran {
		t.Fatalf("an ungated method was blocked: ran=%v err=%v", ran, err)
	}
	if _, err := srv.Version(context.Background(), &pb.VersionRequest{}); err != nil {
		t.Fatalf("Version refused while draining: %v", err)
	}
}

// With every slot taken, each stream method is refused with ResourceExhausted
// and the refusal is counted; the slot comes back when the stream ends.
func TestStreamGate_SlotExhaustion(t *testing.T) {
	srv, m := newTestServer(newFakeDocker(), auth.AllowAll{}, Options{MaxStreams: 1})
	release, err := srv.streams.acquire()
	if err != nil {
		t.Fatal(err)
	}
	for full := range gatedStreams {
		ran := false
		err := viaGate(srv, full, context.Background(), func(context.Context) error { ran = true; return nil })
		if statusCode(err) != codes.ResourceExhausted {
			t.Errorf("%s: want ResourceExhausted at the cap, got %v", full, err)
		}
		if ran {
			t.Errorf("%s: the handler ran past the cap", full)
		}
	}
	if got := m.refusedStreams.Load(); got != int64(len(gatedStreams)) {
		t.Errorf("refusals counted = %d, want %d", got, len(gatedStreams))
	}
	release()

	// While a stream runs it holds the slot and counts in the drain.
	err = viaGate(srv, pb.Agent_Logs_FullMethodName, context.Background(), func(context.Context) error {
		if srv.streams.inUse() != 1 || srv.ActiveSessions() != 1 {
			t.Errorf("inside the handler: in use %d, active %d; want 1 and 1", srv.streams.inUse(), srv.ActiveSessions())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if srv.streams.inUse() != 0 || srv.ActiveSessions() != 0 {
		t.Errorf("after the stream: in use %d, active %d; want 0 and 0", srv.streams.inUse(), srv.ActiveSessions())
	}
}

// Logs never reported anything to the metrics. Through the gate a log stream is
// counted per method and its bytes reach bytes_out — the same number its
// logs_end audit record carries.
func TestStreamGate_LogsMetrics(t *testing.T) {
	d := newFakeDocker()
	srv, m := newTestServer(d, auth.AllowAll{}, Options{})
	log := captureAudit(srv)
	var buf bytes.Buffer
	_, _ = newStdWriter(&buf, stdcopy.Stdout).Write([]byte("hello\n"))
	_, _ = newStdWriter(&buf, stdcopy.Stderr).Write([]byte("oops\n"))
	d.logsReader = io.NopCloser(&buf)

	if err := gatedLogs(srv, &pb.LogsRequest{ContainerId: "c1"}, &fakeLogsStream{ctx: context.Background()}); err != nil {
		t.Fatal(err)
	}
	if started, ended := m.streams("Logs"); started != 1 || ended != 1 {
		t.Errorf("Logs streams started/ended = %d/%d, want 1/1", started, ended)
	}
	if got := m.out.Load(); got != int64(len("hello\noops\n")) {
		t.Errorf("bytes out = %d, want %d", got, len("hello\noops\n"))
	}
	rec := findAuditEvent(t, log.String(), "logs_end")
	if got, ok := rec["bytes_out"].(float64); !ok || int64(got) != m.out.Load() {
		t.Errorf("audit bytes_out = %v, metric = %d; they must agree", rec["bytes_out"], m.out.Load())
	}
	if started, _ := m.streams("Exec"); started != 0 {
		t.Error("a log stream was counted as an exec stream")
	}
	if m.started.Load() != 0 {
		t.Error("a log stream was counted as an exec session")
	}
}

// Event watches are counted per method too.
func TestStreamGate_EventsMetrics(t *testing.T) {
	d := newFakeDocker()
	d.eventCh = make(chan events.Message, 1)
	d.eventErrCh = make(chan error, 1)
	srv, m := newTestServer(d, auth.AllowAll{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- gatedEvents(srv, &pb.WatchContainerEventsRequest{ContainerId: "c1"}, &fakeEventStream{ctx: ctx})
	}()
	waitFor(t, func() bool {
		started, _ := m.streams("WatchContainerEvents")
		return started == 1 && srv.ActiveSessions() == 1
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("watch ended with %v", err)
	}
	if started, ended := m.streams("WatchContainerEvents"); started != 1 || ended != 1 {
		t.Errorf("WatchContainerEvents started/ended = %d/%d, want 1/1", started, ended)
	}
}

// A denied stream was admitted by the gate (it is counted as started and
// ended), but the denial itself is audited and counted by the handler exactly
// as before, and no session starts.
func TestStreamGate_DeniedStreamStillAuditedByHandler(t *testing.T) {
	srv, m := newTestServer(newFakeDocker(), denyAuth{}, Options{})
	err := gatedLogs(srv, &pb.LogsRequest{ContainerId: "c1"}, &fakeLogsStream{ctx: context.Background()})
	if statusCode(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if m.denied.Load() != 1 {
		t.Errorf("auth denials = %d, want 1", m.denied.Load())
	}
	if srv.ActiveSessions() != 0 || srv.streams.inUse() != 0 {
		t.Error("a denied stream left a slot or drain count behind")
	}
}
