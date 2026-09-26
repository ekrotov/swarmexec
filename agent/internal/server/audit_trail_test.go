// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/pkg/stdcopy"

	"swarmexec/agent/internal/audit"
	"swarmexec/agent/internal/auth"
	"swarmexec/internal/forwardmux"
	"swarmexec/internal/pb"
)

// The audit log is a security promise — "every exec, log stream, forward and
// deletion leaves a record" — and for a long time exactly one of its records
// (container_list) was asserted anywhere. These tests drive each RPC through
// its normal path and read the trail back, so a refactor that drops or
// mislabels a record fails here instead of in an incident review.

// captureAudit points srv's audit trail at a buffer. It works on any test
// server, so each RPC keeps its own setup helper.
func captureAudit(srv *Server) *syncBuffer {
	buf := &syncBuffer{}
	srv.audit = audit.New(slog.New(slog.NewJSONHandler(buf, nil)), true)
	return buf
}

// auditRecords returns every record of the given event, in order.
func auditRecords(out, event string) []map[string]any {
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["event"] == event {
			recs = append(recs, rec)
		}
	}
	return recs
}

// wantFields fails for every field of rec that differs from want. JSON numbers
// arrive as float64, so numeric wants are written that way.
func wantFields(t *testing.T, rec map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		got, _ := json.Marshal(rec[k])
		exp, _ := json.Marshal(v)
		if !bytes.Equal(got, exp) {
			t.Errorf("%v.%s = %s, want %s", rec["event"], k, got, exp)
		}
	}
}

func TestAuditTrail_Exec(t *testing.T) {
	d := newFakeDocker()
	d.exitCode = 7
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	log := captureAudit(srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeStream(ctx)
	stream.queueStart(startExec("c1", []string{"/bin/sh", "-l"}, true))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Exec(stream) }()
	d.waitAttach().Close()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}

	out := log.String()
	wantFields(t, findAuditEvent(t, out, "auth_decision"), map[string]any{
		"identity": "test-user", "container_id": "c1", "allow": true,
	})
	wantFields(t, findAuditEvent(t, out, "session_start"), map[string]any{
		"identity": "test-user", "container_id": "c1", "cmd": []string{"/bin/sh", "-l"}, "tty": true,
	})
	wantFields(t, findAuditEvent(t, out, "session_end"), map[string]any{
		"identity": "test-user", "container_id": "c1", "exit_code": 7.0,
	})
}

// A refusal is the record that matters most, and the session it refused must
// leave no trace of having started.
func TestAuditTrail_ExecDenied(t *testing.T) {
	d := newFakeDocker()
	srv, _ := newTestServer(d, denyAuth{}, Options{})
	log := captureAudit(srv)
	stream := newFakeStream(context.Background())
	stream.queueStart(startExec("c1", []string{"/bin/sh"}, true))

	if err := srv.Exec(stream); err == nil {
		t.Fatal("a denied exec must fail")
	}
	out := log.String()
	wantFields(t, findAuditEvent(t, out, "auth_decision"), map[string]any{
		"identity": "test-user", "container_id": "c1", "allow": false, "reason": "test deny",
	})
	if recs := auditRecords(out, "session_start"); len(recs) != 0 {
		t.Errorf("a denied exec was recorded as started: %v", recs)
	}
}

func TestAuditTrail_Logs(t *testing.T) {
	d := newFakeDocker()
	var buf bytes.Buffer
	_, _ = stdcopy.NewStdWriter(&buf, stdcopy.Stdout).Write([]byte("out-line\n"))
	d.logsReader = io.NopCloser(&buf)
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	log := captureAudit(srv)

	if err := srv.Logs(&pb.LogsRequest{ContainerId: "c1", Follow: true}, &fakeLogsStream{ctx: context.Background()}); err != nil {
		t.Fatal(err)
	}
	out := log.String()
	wantFields(t, findAuditEvent(t, out, "logs_start"), map[string]any{
		"identity": "test-user", "container_id": "c1", "follow": true,
	})
	wantFields(t, findAuditEvent(t, out, "logs_end"), map[string]any{
		"identity": "test-user", "container_id": "c1", "bytes_out": float64(len("out-line\n")),
	})
}

func TestAuditTrail_Forward(t *testing.T) {
	d := newFakeDocker()
	srv, _ := forwardTestServer(d, auth.AllowAll{})
	log := captureAudit(srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeForwardStream(ctx)
	stream.queueStart("target-abc", 8080)

	done := make(chan error, 1)
	go func() { done <- srv.PortForward(stream) }()
	mux := newMuxSide(t, d.waitAttach())
	id := mux.acceptOpen()
	mux.send(forwardmux.Frame{Conn: id, Kind: forwardmux.KindData, Payload: []byte("pong!")})
	stream.queueData([]byte("ping"))
	mux.recv()
	waitUntil(t, "the payload to reach the client", func() bool {
		return len(forwardPayload(stream.sentMessages())) == len("pong!")
	})
	mux.send(forwardmux.Frame{Conn: id, Kind: forwardmux.KindClose})
	<-done

	out := log.String()
	wantFields(t, findAuditEvent(t, out, "forward_start"), map[string]any{
		"identity": "test-user", "container_id": "target-abc", "port": 8080.0,
	})
	wantFields(t, findAuditEvent(t, out, "forward_end"), map[string]any{
		"identity": "test-user", "container_id": "target-abc", "port": 8080.0,
		"bytes_in": 4.0, "bytes_out": 5.0,
	})
}

func TestAuditTrail_EventWatch(t *testing.T) {
	srv, d := eventServer(t, auth.AllowAll{})
	log := captureAudit(srv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &fakeEventStream{ctx: ctx}

	done := make(chan error, 1)
	go func() {
		done <- srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{ContainerId: "c1"}, stream)
	}()
	d.eventCh <- events.Message{Type: events.ContainerEventType, Action: "start", TimeNano: 1}
	d.eventCh <- events.Message{Type: events.ContainerEventType, Action: "die", TimeNano: 2}
	waitUntil(t, "both events forwarded", func() bool { return len(stream.events()) == 2 })
	cancel()
	<-done

	out := log.String()
	wantFields(t, findAuditEvent(t, out, "event_watch_start"), map[string]any{
		"identity": "test-user", "container_id": "c1",
	})
	// The count, never the content: nothing of the events themselves.
	wantFields(t, findAuditEvent(t, out, "event_watch_end"), map[string]any{
		"identity": "test-user", "container_id": "c1", "events": 2.0,
	})
}

func TestAuditTrail_Volumes(t *testing.T) {
	d := newFakeDocker()
	d.volumeRemErr = errors.New("volume is in use - [abc]")
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	log := captureAudit(srv)
	ctx := context.Background()

	if _, err := srv.CreateVolume(ctx, &pb.CreateVolumeRequest{Name: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RemoveVolume(ctx, &pb.RemoveVolumeRequest{Name: "v2"}); err == nil {
		t.Fatal("remove should fail with the volume in use")
	}

	out := log.String()
	wantFields(t, findAuditEvent(t, out, "volume_create"), map[string]any{
		"identity": "test-user", "volume": "v1", "ok": true,
	})
	// A failed deletion is still an attempt, and is recorded with its reason.
	rm := findAuditEvent(t, out, "volume_remove")
	wantFields(t, rm, map[string]any{"identity": "test-user", "volume": "v2", "ok": false})
	if !strings.Contains(rm["error"].(string), "in use") {
		t.Errorf("volume_remove.error = %v, want the daemon's reason", rm["error"])
	}
	if n := len(auditRecords(out, "auth_decision")); n != 2 {
		t.Errorf("auth_decision records = %d, want one per call", n)
	}
}

func TestAuditTrail_ImagePrune(t *testing.T) {
	d := newFakeDocker()
	d.pruneReport = image.PruneReport{SpaceReclaimed: 1234, ImagesDeleted: []image.DeleteResponse{
		{Deleted: "sha256:gone"}, {Untagged: "app:old"},
	}}
	srv := imageTestServer(d)
	log := captureAudit(srv)

	if _, err := srv.PruneImages(context.Background(), &pb.PruneImagesRequest{All: true}); err != nil {
		t.Fatal(err)
	}
	wantFields(t, findAuditEvent(t, log.String(), "image_prune"), map[string]any{
		"identity": "test-user", "all": true, "reclaimed_bytes": 1234.0, "deleted": 2.0, "ok": true,
	})
}
