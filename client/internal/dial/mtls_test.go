// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package dial

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/agenttest"
	"swarmexec/client/internal/config"
	"swarmexec/internal/pb"
)

// These tests put the real client dial and the real agent — real TLS config,
// real interceptors, real identity resolution, real policy — on either side of
// a loopback socket. They pin the mode the docs recommend for untrusted
// networks: that a client without (or with the wrong) certificate is refused,
// that the certificate's CN is who the audit log says did it, and that the
// policy's identity rules rest on it.

func mtlsAgent(t *testing.T, o agenttest.Options) *agenttest.Agent {
	t.Helper()
	o.MTLS = true
	if o.Docker == nil {
		o.Docker = agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1", Service: "web"})
	}
	return agenttest.Start(t, o)
}

func withCert(a *agenttest.Agent, c agenttest.ClientCert) config.Config {
	return config.Config{CA: a.CAFile, ServerName: "localhost", Cert: c.Cert, Key: c.Key}
}

// call dials the agent with cfg and makes one authenticated RPC. The dial may
// itself fail (TLS 1.3 reports a refused client certificate only on first use,
// so it often does not); err is whichever failed first.
func call(t *testing.T, a *agenttest.Agent, cfg config.Config) (*pb.ListResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, a.Host, a.Port, cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return pb.NewAgentClient(conn).ListContainers(ctx, &pb.ListRequest{}, grpc.WaitForReady(false))
}

func TestMTLS_ClientCertificateIsAcceptedAndItsCNIsTheAuditIdentity(t *testing.T) {
	a := mtlsAgent(t, agenttest.Options{})
	res, err := call(t, a, withCert(a, a.IssueClient(t, "alice")))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(res.GetContainers()) != 1 || res.GetContainers()[0].GetService() != "web" {
		t.Errorf("containers = %v, want the fake node's web container", res.GetContainers())
	}
	recs := a.AuditEvents("container_list")
	if len(recs) != 1 {
		t.Fatalf("got %d container_list records, want 1: %v", len(recs), a.Audit())
	}
	if recs[0]["identity"] != "alice" {
		t.Errorf("audit identity = %v, want the certificate CN alice", recs[0]["identity"])
	}
	if recs[0]["identity_verified"] != true {
		t.Errorf("identity_verified = %v, want true under mTLS", recs[0]["identity_verified"])
	}
}

func TestMTLS_ClientWithoutACertificateIsRefused(t *testing.T) {
	a := mtlsAgent(t, agenttest.Options{})
	_, err := call(t, a, config.Config{CA: a.CAFile, ServerName: "localhost"})
	if err == nil {
		t.Fatal("a client with no certificate got an answer")
	}
	t.Logf("refused with: %v", err)
	if n := len(a.Audit()); n != 0 {
		t.Errorf("%d audit records for a refused client, want none: %v", n, a.Audit())
	}
	if c := a.Docker.Called("ContainerList"); c != 0 {
		t.Errorf("Docker was asked %d times on behalf of a refused client", c)
	}
}

func TestMTLS_CertificateFromAForeignCAIsRefused(t *testing.T) {
	a := mtlsAgent(t, agenttest.Options{})
	_, err := call(t, a, withCert(a, a.IssueForeignClient(t, "alice")))
	if err == nil {
		t.Fatal("a certificate from a CA the agent does not trust got an answer")
	}
	t.Logf("refused with: %v", err)
	if a.Docker.Called("ContainerList") != 0 || len(a.Audit()) != 0 {
		t.Error("a refused client reached the handler")
	}
}

// A certificate the CA signed but that names nobody is a valid chain with no
// identity to attribute to: the agent refuses to act for "".
func TestMTLS_EmptyCommonNameIsRefused(t *testing.T) {
	a := mtlsAgent(t, agenttest.Options{})
	_, err := call(t, a, withCert(a, a.IssueClient(t, "")))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
	if a.Docker.Called("ContainerList") != 0 {
		t.Error("an unattributable client reached Docker")
	}
}

// Under mTLS the identity is verified, so the policy's identity rules are
// usable — and they bind: alice may list, bob may not.
func TestMTLS_IdentityRulesDecideByCertificateCN(t *testing.T) {
	a := mtlsAgent(t, agenttest.Options{Policy: `
default: deny
rules:
  - allow: ["container.list"]
    identities: ["alice"]
`})
	if _, err := call(t, a, withCert(a, a.IssueClient(t, "alice"))); err != nil {
		t.Fatalf("alice: %v", err)
	}
	_, err := call(t, a, withCert(a, a.IssueClient(t, "bob")))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob: err = %v, want PermissionDenied", err)
	}
	var allowed, denied int
	for _, r := range a.AuditEvents("auth_decision") {
		switch {
		case r["identity"] == "alice" && r["allow"] == true:
			allowed++
		case r["identity"] == "bob" && r["allow"] == false:
			denied++
		}
	}
	if allowed != 1 || denied != 1 {
		t.Errorf("auth_decision records: %d allowed for alice, %d denied for bob, want 1 and 1: %v", allowed, denied, a.AuditEvents("auth_decision"))
	}
}

// Without client certificates the operator name is the client's say-so, and
// the audit log says so on every record.
func TestSecretMode_AuditSaysTheIdentityIsNotVerified(t *testing.T) {
	a := agenttest.Start(t, agenttest.Options{Secret: "s3cret"})
	cfg := config.Config{CA: a.CAFile, ServerName: "localhost", AgentSecret: "s3cret", Operator: "carol"}
	if _, err := call(t, a, cfg); err != nil {
		t.Fatalf("call: %v", err)
	}
	recs := a.AuditEvents("container_list")
	if len(recs) != 1 || recs[0]["identity"] != "carol" || recs[0]["identity_verified"] != false {
		t.Errorf("audit = %v, want identity carol with identity_verified=false", recs)
	}
}

func TestSecretMode_WrongSecretIsRefusedBeforeTheHandler(t *testing.T) {
	a := agenttest.Start(t, agenttest.Options{Secret: "s3cret"})
	cfg := config.Config{CA: a.CAFile, ServerName: "localhost", AgentSecret: "wrong"}
	_, err := call(t, a, cfg)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("err = %v, want Unauthenticated", err)
	}
	if a.Docker.Called("ContainerList") != 0 {
		t.Error("a caller with the wrong secret reached Docker")
	}
}

// Both credentials at once: a valid certificate does not excuse a wrong secret.
func TestMTLSWithSecret_NeedsBothCredentials(t *testing.T) {
	a := mtlsAgent(t, agenttest.Options{Secret: "s3cret"})
	cc := a.IssueClient(t, "alice")
	good := config.Config{CA: a.CAFile, ServerName: "localhost", Cert: cc.Cert, Key: cc.Key, AgentSecret: "s3cret"}
	if _, err := call(t, a, good); err != nil {
		t.Fatalf("both credentials: %v", err)
	}
	bad := good
	bad.AgentSecret = "nope"
	if _, err := call(t, a, bad); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("certificate with wrong secret: err = %v, want Unauthenticated", err)
	}
}
