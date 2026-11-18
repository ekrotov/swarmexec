// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"swarmexec/agent/agenttest"
	"swarmexec/internal/authmeta"
	"swarmexec/internal/pb"
)

const auditSecret = "s3cret-value"

// rawClient dials the agent with no client library in between, so a test
// chooses exactly which credential (if any) each call carries. cred is the
// metadata sent; empty sends none.
func rawClient(t testing.TB, a *agenttest.Agent, md map[string]string) (pb.AgentClient, context.Context) {
	t.Helper()
	pem, err := os.ReadFile(a.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	conn, err := grpc.NewClient("passthrough:///"+a.Addr(), grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "localhost"})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if len(md) > 0 {
		kv := make([]string, 0, 2*len(md))
		for k, v := range md {
			kv = append(kv, k, v)
		}
		ctx = metadata.AppendToOutgoingContext(ctx, kv...)
	}
	return pb.NewAgentClient(conn), ctx
}

func legacyCreds(operator string) map[string]string {
	return map[string]string{authmeta.SecretKey: auditSecret, authmeta.OperatorKey: operator}
}

func only(t *testing.T, recs []map[string]any, what string) map[string]any {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("%s: got %d records, want 1: %v", what, len(recs), recs)
	}
	return recs[0]
}

const denyPolicy = `
default: allow
rules:
  - deny: [volume.remove]
    volumes: ["prod_*"]
    reason: production volumes stay
  - deny: [exec]
    users: [root]
    reason: no root shell
`

// SEC-03: the record of a refused request is the only record of it, so it has
// to say what was asked for and of what — not just which rule fired.
func TestAudit_DeniedRequestNamesActionAndObject(t *testing.T) {
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1", Service: "web"})
	a := agenttest.Start(t, agenttest.Options{Secret: auditSecret, AllowLegacySecret: true, Policy: denyPolicy, Docker: d})
	c, ctx := rawClient(t, a, legacyCreds("alice"))

	_, err := c.RemoveVolume(ctx, &pb.RemoveVolumeRequest{Name: "prod_db"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("RemoveVolume = %v, want PermissionDenied", err)
	}
	rec := only(t, a.AuditEvents("auth_decision"), "after the volume denial")
	for k, want := range map[string]any{
		"action": "volume.remove", "volume": "prod_db", "identity": "alice", "allow": false,
	} {
		if rec[k] != want {
			t.Errorf("volume denial: %s = %v, want %v (record %v)", k, rec[k], want, rec)
		}
	}
	if addr, _ := rec["client_addr"].(string); !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("client_addr = %v, want the peer address", rec["client_addr"])
	}

	st, err := c.Exec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&pb.ClientMessage{Payload: &pb.ClientMessage_Start{Start: &pb.StartExec{
		ContainerId: "c1", Cmd: []string{"sh", "-c", "echo TOPSECRET=hunter2"}, User: "root",
		Env: []string{"API_KEY=hunter2"},
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Exec = %v, want PermissionDenied", err)
	}
	var exec map[string]any
	for _, r := range a.AuditEvents("auth_decision") {
		if r["action"] == "exec" {
			exec = r
		}
	}
	if exec == nil || exec["user"] != "root" || exec["service"] != "web" || exec["container_id"] != "c1" || exec["allow"] != false {
		t.Errorf("exec denial = %v, want action exec, user root, service web, container c1", exec)
	}
	for _, r := range a.Audit() {
		if b, _ := json.Marshal(r); strings.Contains(string(b), "hunter2") || strings.Contains(string(b), auditSecret) {
			t.Errorf("audit record leaks the command, an env value or the secret: %v", r)
		}
	}
}

// SEC-03: a poll is one record per identity and action per interval, the first
// one is never hidden, and a denial of the same poll is never throttled.
func TestAudit_PolledAllowsAreThrottledDenialsAreNot(t *testing.T) {
	d := agenttest.NewDocker().AddVolume("data")
	a := agenttest.Start(t, agenttest.Options{Secret: auditSecret, AllowLegacySecret: true, Docker: d,
		Policy: "default: allow\nrules:\n  - deny: [image.list]\n"})
	c, ctx := rawClient(t, a, legacyCreds("alice"))

	for i := 0; i < 5; i++ {
		if _, err := c.ListVolumes(ctx, &pb.ListVolumesRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	rec := only(t, a.AuditEvents("auth_decision"), "five allowed polls")
	if rec["action"] != "volume.list" || rec["allow"] != true {
		t.Errorf("the surviving record = %v, want the first volume.list allow", rec)
	}

	for i := 0; i < 3; i++ {
		if _, err := c.ListImages(ctx, &pb.ListImagesRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("ListImages = %v, want PermissionDenied", err)
		}
	}
	n := 0
	for _, r := range a.AuditEvents("auth_decision") {
		if r["action"] == "image.list" && r["allow"] == false {
			n++
		}
	}
	if n != 3 {
		t.Errorf("%d denial records of image.list, want all 3", n)
	}
}
