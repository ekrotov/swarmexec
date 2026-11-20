// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// A fake CopyFrom stream: it keeps what the handler sent.
type fakeCopyFromStream struct {
	ctx     context.Context
	mu      sync.Mutex
	sent    []*pb.CopyChunk
	sendErr error
}

func (s *fakeCopyFromStream) Send(c *pb.CopyChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent = append(s.sent, c)
	return nil
}
func (s *fakeCopyFromStream) Context() context.Context     { return s.ctx }
func (s *fakeCopyFromStream) SetHeader(metadata.MD) error  { return nil }
func (s *fakeCopyFromStream) SendHeader(metadata.MD) error { return nil }
func (s *fakeCopyFromStream) SetTrailer(metadata.MD)       {}
func (s *fakeCopyFromStream) SendMsg(any) error            { return nil }
func (s *fakeCopyFromStream) RecvMsg(any) error            { return nil }

func (s *fakeCopyFromStream) payload() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	for _, c := range s.sent {
		out = append(out, c.GetData()...)
	}
	return out
}

func (s *fakeCopyFromStream) stat() *pb.CopyStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		return nil
	}
	return s.sent[0].GetStat()
}

// A fake CopyTo stream: it plays the client's messages back to the handler.
type fakeCopyToStream struct {
	ctx  context.Context
	msgs []*pb.CopyToMessage
	i    int
	resp *pb.CopyToResponse
}

func (s *fakeCopyToStream) Recv() (*pb.CopyToMessage, error) {
	if s.i >= len(s.msgs) {
		return nil, io.EOF
	}
	m := s.msgs[s.i]
	s.i++
	return m, nil
}
func (s *fakeCopyToStream) SendAndClose(r *pb.CopyToResponse) error { s.resp = r; return nil }
func (s *fakeCopyToStream) Context() context.Context                { return s.ctx }
func (s *fakeCopyToStream) SetHeader(metadata.MD) error             { return nil }
func (s *fakeCopyToStream) SendHeader(metadata.MD) error            { return nil }
func (s *fakeCopyToStream) SetTrailer(metadata.MD)                  {}
func (s *fakeCopyToStream) SendMsg(any) error                       { return nil }
func (s *fakeCopyToStream) RecvMsg(any) error                       { return nil }

func copyToMsgs(dest string, maxBytes uint64, parts ...string) []*pb.CopyToMessage {
	msgs := []*pb.CopyToMessage{{Payload: &pb.CopyToMessage_Start{Start: &pb.StartCopyTo{
		ContainerId: "c1", DestPath: dest, MaxBytes: maxBytes,
	}}}}
	for _, p := range parts {
		msgs = append(msgs, &pb.CopyToMessage{Payload: &pb.CopyToMessage_Data{Data: []byte(p)}})
	}
	return msgs
}

// copyFake is a daemon with one container and one readable path.
func copyFake(archive string) *fakeDocker {
	d := newFakeDocker()
	d.inspect["c1"] = container.InspectResponse{
		ID:     "c1full",
		Config: &container.Config{Labels: map[string]string{swarmServiceLabel: "web"}},
	}
	d.copyFrom = []byte(archive)
	d.statResult = map[string]container.PathStat{
		"/var/log/app.log": {Name: "app.log", Size: int64(len(archive)), Mode: 0o644},
	}
	return d
}

func TestCopyFromSendsTheStatFirstThenThePayload(t *testing.T) {
	d := copyFake("tar-bytes-here")
	srv, log := auditedTestServer(d, auth.AllowAll{})
	st := &fakeCopyFromStream{ctx: context.Background()}

	if err := srv.CopyFrom(&pb.CopyFromRequest{ContainerId: "c1", Path: "/var/log/app.log"}, st); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if got := st.stat(); got == nil || got.GetName() != "app.log" || got.GetIsDir() {
		t.Errorf("first chunk = %v, want the stat of app.log", got)
	}
	if got := string(st.payload()); got != "tar-bytes-here" {
		t.Errorf("payload = %q, want the archive", got)
	}
	rec := findAuditEvent(t, log.String(), "copy_end")
	wantFields(t, rec, map[string]any{"direction": "from", "bytes": 14, "ok": true, "container_id": "c1"})
	start := findAuditEvent(t, log.String(), "copy_start")
	wantFields(t, start, map[string]any{"path": "/var/log/app.log", "service": "web"})
}

func TestCopyFromRefusesOverTheSizeCap(t *testing.T) {
	d := copyFake(strings.Repeat("x", 4096))
	srv, log := auditedTestServer(d, auth.AllowAll{})
	st := &fakeCopyFromStream{ctx: context.Background()}

	err := srv.CopyFrom(&pb.CopyFromRequest{ContainerId: "c1", Path: "/var/log/app.log", MaxBytes: 10}, st)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("CopyFrom err = %v, want ResourceExhausted", err)
	}
	// The failure is in the audit log too: a start without an end would read
	// as a copy that is still running.
	rec := findAuditEvent(t, log.String(), "copy_end")
	wantFields(t, rec, map[string]any{"ok": false, "direction": "from"})
}

func TestCopyFromFollowsASymlinkOnlyWhenAsked(t *testing.T) {
	d := copyFake("payload")
	d.statResult["/link"] = container.PathStat{Name: "link", Mode: os.ModeSymlink, LinkTarget: "/var/log/app.log"}
	srv, _ := auditedTestServer(d, auth.AllowAll{})

	if err := srv.CopyFrom(&pb.CopyFromRequest{ContainerId: "c1", Path: "/link", FollowLink: true},
		&fakeCopyFromStream{ctx: context.Background()}); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if len(d.copyFromOps) != 1 || d.copyFromOps[0].SourcePath != "/var/log/app.log" {
		t.Errorf("copied %v, want the link's target", d.copyFromOps)
	}

	d.copyFromOps = nil
	if err := srv.CopyFrom(&pb.CopyFromRequest{ContainerId: "c1", Path: "/link"},
		&fakeCopyFromStream{ctx: context.Background()}); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if len(d.copyFromOps) != 1 || d.copyFromOps[0].SourcePath != "/link" {
		t.Errorf("copied %v, want the link itself", d.copyFromOps)
	}
}

// A relative link target is resolved against the directory the link lives in,
// the way the kernel would, not against the root.
func TestCopyFromResolvesARelativeLinkAgainstItsDirectory(t *testing.T) {
	d := copyFake("payload")
	d.statResult["/var/log/current"] = container.PathStat{Name: "current", Mode: os.ModeSymlink, LinkTarget: "app.log"}
	srv, _ := auditedTestServer(d, auth.AllowAll{})

	if err := srv.CopyFrom(&pb.CopyFromRequest{ContainerId: "c1", Path: "/var/log/current", FollowLink: true},
		&fakeCopyFromStream{ctx: context.Background()}); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if len(d.copyFromOps) != 1 || d.copyFromOps[0].SourcePath != "/var/log/app.log" {
		t.Errorf("copied %v, want /var/log/app.log", d.copyFromOps)
	}
}

func TestCopyToExtractsAndReportsTheByteCount(t *testing.T) {
	d := copyFake("")
	srv, log := auditedTestServer(d, auth.AllowAll{})
	st := &fakeCopyToStream{ctx: context.Background(), msgs: copyToMsgs("/tmp", 0, "part-one", "part-two")}

	if err := srv.CopyTo(st); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if got := string(d.copiedTo); got != "part-onepart-two" {
		t.Errorf("daemon got %q, want both parts", got)
	}
	if st.resp.GetBytesWritten() != 16 {
		t.Errorf("bytes_written = %d, want 16", st.resp.GetBytesWritten())
	}
	if len(d.copyToOpts) != 1 || d.copyToOpts[0].DestinationPath != "/tmp" || d.copyToOpts[0].AllowOverwriteDirWithFile {
		t.Errorf("options = %+v, want /tmp and no dir-for-file overwrite", d.copyToOpts)
	}
	rec := findAuditEvent(t, log.String(), "copy_end")
	wantFields(t, rec, map[string]any{"direction": "to", "bytes": 16, "ok": true})
}

func TestCopyToRefusesOverTheSizeCap(t *testing.T) {
	d := copyFake("")
	srv, _ := auditedTestServer(d, auth.AllowAll{})
	st := &fakeCopyToStream{ctx: context.Background(), msgs: copyToMsgs("/tmp", 4, "too-long")}

	err := srv.CopyTo(st)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("CopyTo err = %v, want ResourceExhausted", err)
	}
}

func TestCopyToRefusesASecondStart(t *testing.T) {
	d := copyFake("")
	srv, _ := auditedTestServer(d, auth.AllowAll{})
	msgs := copyToMsgs("/tmp", 0, "data")
	msgs = append(msgs, msgs[0])
	st := &fakeCopyToStream{ctx: context.Background(), msgs: msgs}

	if status.Code(srv.CopyTo(st)) != codes.InvalidArgument {
		t.Errorf("a second StartCopyTo was accepted")
	}
}

func TestCopyToNeedsItsStartFirst(t *testing.T) {
	srv, _ := auditedTestServer(copyFake(""), auth.AllowAll{})
	st := &fakeCopyToStream{ctx: context.Background(), msgs: []*pb.CopyToMessage{
		{Payload: &pb.CopyToMessage_Data{Data: []byte("x")}},
	}}
	if status.Code(srv.CopyTo(st)) != codes.InvalidArgument {
		t.Errorf("payload before the StartCopyTo was accepted")
	}
}

// The two directions are two permissions: a policy that allows reading out of
// a container must be able to refuse writing into it.
func TestCopyAuthorizesTheTwoDirectionsSeparately(t *testing.T) {
	d := copyFake("archive")
	var seen []auth.Request
	srv, _ := auditedTestServer(d, denyActions{deny: map[string]bool{"copy.to": true}, seen: &seen})

	if err := srv.CopyFrom(&pb.CopyFromRequest{ContainerId: "c1", Path: "/var/log/app.log"},
		&fakeCopyFromStream{ctx: context.Background()}); err != nil {
		t.Fatalf("CopyFrom denied although only copy.to is: %v", err)
	}
	err := srv.CopyTo(&fakeCopyToStream{ctx: context.Background(), msgs: copyToMsgs("/tmp", 0, "x")})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("CopyTo err = %v, want PermissionDenied", err)
	}
	if len(seen) != 2 || seen[0].Action != "copy.from" || seen[1].Action != "copy.to" {
		t.Fatalf("authorized = %+v, want copy.from then copy.to", seen)
	}
	if seen[0].Path != "/var/log/app.log" || seen[1].Path != "/tmp" {
		t.Errorf("paths = %q, %q — the authorizer must see what is being copied", seen[0].Path, seen[1].Path)
	}
}

func TestStatPathReportsAMissingPathAsAnAnswer(t *testing.T) {
	d := copyFake("")
	d.statErr = cerrdefs.ErrNotFound
	srv, _ := auditedTestServer(d, auth.AllowAll{})

	resp, err := srv.StatPath(context.Background(), &pb.StatPathRequest{ContainerId: "c1", Path: "/nope"})
	if err != nil {
		t.Fatalf("StatPath: %v", err)
	}
	if resp.GetExists() {
		t.Errorf("exists = true for a path that is not there")
	}
}

func TestStatPathIsAuthorizedAsARead(t *testing.T) {
	var seen []auth.Request
	srv, _ := auditedTestServer(copyFake(""), denyActions{seen: &seen})

	if _, err := srv.StatPath(context.Background(), &pb.StatPathRequest{ContainerId: "c1", Path: "/tmp"}); err != nil {
		t.Fatalf("StatPath: %v", err)
	}
	if len(seen) != 1 || seen[0].Action != "copy.from" {
		t.Errorf("authorized = %+v, want copy.from", seen)
	}
}

func TestCopyRejectsEmptyArguments(t *testing.T) {
	srv, _ := auditedTestServer(copyFake(""), auth.AllowAll{})
	ctx := context.Background()

	if code := status.Code(srv.CopyFrom(&pb.CopyFromRequest{ContainerId: "c1"}, &fakeCopyFromStream{ctx: ctx})); code != codes.InvalidArgument {
		t.Errorf("CopyFrom without a path = %v, want InvalidArgument", code)
	}
	if _, err := srv.StatPath(ctx, &pb.StatPathRequest{Path: "/tmp"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("StatPath without a container = %v, want InvalidArgument", err)
	}
	st := &fakeCopyToStream{ctx: ctx, msgs: []*pb.CopyToMessage{{Payload: &pb.CopyToMessage_Start{Start: &pb.StartCopyTo{ContainerId: "c1"}}}}}
	if status.Code(srv.CopyTo(st)) != codes.InvalidArgument {
		t.Errorf("CopyTo without a destination was accepted")
	}
}

// A Docker failure keeps its gRPC shape, and the audit record says what broke.
func TestCopyFromReportsADaemonFailure(t *testing.T) {
	d := copyFake("")
	d.copyFromErr = errors.New("daemon says no")
	srv, log := auditedTestServer(d, auth.AllowAll{})

	err := srv.CopyFrom(&pb.CopyFromRequest{ContainerId: "c1", Path: "/var/log/app.log"},
		&fakeCopyFromStream{ctx: context.Background()})
	if err == nil || !strings.Contains(err.Error(), "daemon says no") {
		t.Fatalf("err = %v, want the daemon's reason", err)
	}
	rec := findAuditEvent(t, log.String(), "copy_end")
	wantFields(t, rec, map[string]any{"ok": false})
}

// The agent's own default caps a client that names no limit.
func TestCapBytesFallsBackToTheAgentDefault(t *testing.T) {
	if got := capBytes(0); got != defaultCopyMaxBytes {
		t.Errorf("capBytes(0) = %d, want the default %d", got, defaultCopyMaxBytes)
	}
	if got := capBytes(512); got != 512 {
		t.Errorf("capBytes(512) = %d, want the client's number", got)
	}
	if got := capBytes(1 << 62); got != 1<<62 {
		t.Errorf("capBytes(1<<62) = %d, want the client's number", got)
	}
}

func TestCopyToReaderSplitsAcrossMessages(t *testing.T) {
	st := &fakeCopyToStream{ctx: context.Background(), msgs: copyToMsgs("/tmp", 0, "abc", "de")}
	if _, err := st.Recv(); err != nil { // consume the start, as the handler does
		t.Fatal(err)
	}
	tally := &streamTally{}
	r := &copyToReader{stream: st, limit: 1 << 20, count: &tally.in}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, []byte("abcde")) {
		t.Errorf("read %q, want abcde", got)
	}
}
