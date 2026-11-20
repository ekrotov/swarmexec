// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/agenttest"
	"swarmexec/client/internal/cptar"
	"swarmexec/client/internal/resolve"
)

// The copy calls against the real agent over real gRPC and real TLS: the wire
// shape (stat first, then payload; the start message before any payload) is
// the half that unit tests on either side cannot check.

func copyAgent(t *testing.T) (*agenttest.Agent, *Client, resolve.Endpoint) {
	t.Helper()
	d := agenttest.NewDocker().
		AddContainer(agenttest.Container{ID: "c1", Name: "web.1", Service: "web"}).
		SetFile("c1", "/var/log/app.log", []byte("the log line")).
		SetDir("c1", "/etc/nginx", map[string][]byte{"nginx.conf": []byte("listen 80;")}).
		SetSymlink("c1", "/var/log/current", "/var/log/app.log")
	a, ac, n := realAgent(t, d)
	return a, ac, resolve.Endpoint{DialHost: n.DialHost, ContainerID: "c1"}
}

func TestRealAgent_CopyFromGivesTheStatAndTheArchive(t *testing.T) {
	a, ac, ep := copyAgent(t)
	var buf bytes.Buffer

	stat, n, err := ac.CopyFrom(context.Background(), ep, testTimeout,
		CopyFromOptions{Path: "/var/log/app.log"}, &buf, nil)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if stat.GetName() != "app.log" || stat.GetIsDir() {
		t.Errorf("stat = %v, want the file app.log", stat)
	}
	if n == 0 || int64(buf.Len()) != n {
		t.Errorf("n = %d, buffered = %d — the count must be what was written", n, buf.Len())
	}
	if got := tarEntry(t, buf.Bytes(), "app.log"); got != "the log line" {
		t.Errorf("archive held %q, want the file's content", got)
	}
	if recs := a.AuditEvents("copy_start"); len(recs) != 1 || recs[0]["path"] != "/var/log/app.log" ||
		recs[0]["direction"] != "from" || recs[0]["service"] != "web" {
		t.Errorf("audit copy_start = %v", recs)
	}
}

func TestRealAgent_CopyFromFollowsALinkWhenAsked(t *testing.T) {
	_, ac, ep := copyAgent(t)
	var buf bytes.Buffer

	stat, _, err := ac.CopyFrom(context.Background(), ep, testTimeout,
		CopyFromOptions{Path: "/var/log/current", FollowLink: true}, &buf, nil)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if stat.GetName() != "app.log" {
		t.Errorf("stat = %v, want the link's target", stat)
	}
}

func TestRealAgent_CopyFromRefusesOverTheCap(t *testing.T) {
	_, ac, ep := copyAgent(t)
	_, _, err := ac.CopyFrom(context.Background(), ep, testTimeout,
		CopyFromOptions{Path: "/var/log/app.log", MaxBytes: 4}, io.Discard, nil)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("err = %v, want ResourceExhausted", err)
	}
}

func TestRealAgent_CopyToWritesIntoTheContainer(t *testing.T) {
	a, ac, ep := copyAgent(t)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "patch.sql", Mode: 0o644, Size: 8}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("SELECT 1")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	n, err := ac.CopyTo(context.Background(), ep, testTimeout,
		CopyToOptions{DestDir: "/tmp"}, bytes.NewReader(archive.Bytes()), nil)
	if err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if n != int64(archive.Len()) {
		t.Errorf("bytes_written = %d, want %d", n, archive.Len())
	}
	if got := string(a.Docker.Files("c1")["/tmp/patch.sql"]); got != "SELECT 1" {
		t.Errorf("/tmp/patch.sql = %q, want the content", got)
	}
	if recs := a.AuditEvents("copy_end"); len(recs) != 1 || recs[0]["direction"] != "to" || recs[0]["ok"] != true {
		t.Errorf("audit copy_end = %v", recs)
	}
}

func TestRealAgent_StatPathAnswersBothWays(t *testing.T) {
	_, ac, ep := copyAgent(t)
	ctx := context.Background()

	stat, exists, err := ac.StatPath(ctx, ep, testTimeout, "/etc/nginx")
	if err != nil || !exists || !stat.GetIsDir() {
		t.Fatalf("StatPath(/etc/nginx) = %v, %v, %v; want an existing directory", stat, exists, err)
	}
	if _, exists, err = ac.StatPath(ctx, ep, testTimeout, "/nope"); err != nil || exists {
		t.Fatalf("StatPath(/nope) = %v, %v; want exists=false and no error", exists, err)
	}
}

// The container-to-container path, end to end through two real agents: the
// archive is re-framed in flight and never lands on this machine.
func TestRealAgent_CopyBetweenTwoContainers(t *testing.T) {
	_, srcAC, srcEP := copyAgent(t)
	dstDocker := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c2", Name: "db.1", Service: "db"})
	dstAgent, dstAC, dstNode := realAgent(t, dstDocker)
	dstEP := resolve.Endpoint{DialHost: dstNode.DialHost, ContainerID: "c2"}

	fromR, fromW := io.Pipe()
	toR, toW := io.Pipe()
	ctx := context.Background()
	go func() {
		_, _, err := srcAC.CopyFrom(ctx, srcEP, testTimeout, CopyFromOptions{Path: "/var/log/app.log"}, fromW, nil)
		_ = fromW.CloseWithError(err)
	}()
	go func() {
		err := cptar.Rename(fromR, toW, cptar.ExtractOptions{Rename: "copied.log"})
		_ = toW.CloseWithError(err)
	}()
	if _, err := dstAC.CopyTo(ctx, dstEP, testTimeout, CopyToOptions{DestDir: "/tmp"}, toR, nil); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	if got := string(dstAgent.Docker.Files("c2")["/tmp/copied.log"]); got != "the log line" {
		t.Errorf("/tmp/copied.log = %q, want the source's content under the new name", got)
	}
}

// tarEntry returns the body of one entry of an archive.
func tarEntry(t *testing.T, archive []byte, name string) string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("entry %q not in the archive: %v", name, err)
		}
		if strings.TrimSuffix(h.Name, "/") == name {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return string(body)
		}
	}
}

// A CopyTo the agent refuses must come back with the agent's reason. gRPC
// reports a Send onto a stream whose server side has already failed as a bare
// "EOF"; only CloseAndRecv carries the status that says why, and a client that
// reported the EOF would tell the operator nothing.
func TestRealAgent_CopyToReportsTheAgentsReasonNotEOF(t *testing.T) {
	_, ac, ep := copyAgent(t)
	// An empty destination is refused by the handler before it reads any
	// payload, so the client is still sending when the stream is already dead.
	big := strings.NewReader(strings.Repeat("x", 4<<20))

	_, err := ac.CopyTo(context.Background(), ep, testTimeout, CopyToOptions{DestDir: ""}, big, nil)
	if err == nil {
		t.Fatal("a CopyTo without a destination succeeded")
	}
	if status.Code(err) != codes.InvalidArgument || strings.TrimSpace(err.Error()) == "EOF" {
		t.Errorf("err = %v, want the agent's InvalidArgument reason", err)
	}
}
