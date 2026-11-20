// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package agenttest starts the real swarmexec agent — server.New, the real
// interceptor chain, the real TLS configuration and policy loader — on a
// loopback listener, with a fake Docker daemon behind it, so that tests of the
// client (or of the agent's wire behaviour) talk to the code they will meet in
// production instead of a hand-written imitation of it.
//
// It lives under agent/ so it may import agent/internal; client tests import
// agenttest, which is the one door between the two trees.
//
// What is real: the TLS config (tlsconf.ServerConfig for mTLS,
// tlsconf.SelfSignedServerConfig for shared-secret mode), the shared-secret
// check with its connection binding, the recovery and stream-gate interceptors,
// the policy file, the audit logger, every RPC handler and dockerStatus. What
// is fake: the Docker Engine (Docker), and the background caches (volume sizes,
// stats, reclaim, prune schedule), which are not started.
//
// What it does not do is mirror cmd/agent's flag handling; buildServer stays
// tested there. The wiring below follows buildServer line by line and must
// move with it.
package agenttest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"swarmexec/agent/internal/audit"
	"swarmexec/agent/internal/auth"
	"swarmexec/agent/internal/server"
	"swarmexec/agent/internal/tlsconf"
	"swarmexec/internal/pb"
)

// Options selects how the agent authenticates and what it enforces. At least
// one of Secret and MTLS is required: an agent with neither would accept
// anyone, and the real one refuses to start that way.
type Options struct {
	// Secret turns on shared-secret authentication (the self-signed agent of
	// `swarmexec init`). Without MTLS the server certificate is generated and
	// self-signed, the audit identity is the client-supplied operator name, and
	// the audit log says identity_verified=false.
	Secret string
	// AllowLegacySecret accepts the raw, unbound secret from old clients.
	AllowLegacySecret bool
	// MTLS requires and verifies a client certificate against the harness's CA
	// (Agent.IssueClient). The certificate's CN is the audit identity and
	// identity_verified is true. It combines with Secret.
	MTLS bool
	// Policy is the content of an authorization policy file (the YAML
	// auth.LoadPolicy reads); empty allows every authenticated request.
	Policy string
	// Docker is the fake daemon; nil starts an empty one.
	Docker *Docker
	// Metrics is the sink the server and the authentication check count into;
	// nil counts nothing.
	Metrics server.Metrics
	// Server tunes the agent (MaxStreams, IdleTimeout, ...). SecretAuth is set
	// from the authentication mode, not from here.
	Server server.Options
}

// Agent is a running agent. Everything a client test needs to reach it is a
// field or a method here.
type Agent struct {
	// Host and Port are the loopback address the agent listens on.
	Host string
	Port int
	// CAFile is a PEM file that verifies the agent's server certificate: the CA
	// in MTLS mode, the agent's own self-signed certificate otherwise. The
	// certificate carries the names "localhost" and "127.0.0.1".
	CAFile string
	// Secret is the shared secret ("" when not enabled).
	Secret string
	// Docker is the fake daemon behind the agent.
	Docker *Docker
	// Server is the real server, for the drain and session counters.
	Server *server.Server

	grpc *grpc.Server
	pki  *pki
	dir  string

	auditMu  sync.Mutex
	auditBuf bytes.Buffer
	logBuf   bytes.Buffer // the operational log, under auditMu
}

// Log returns the agent's operational (non-audit) log so far, as text.
func (a *Agent) Log() string {
	a.auditMu.Lock()
	defer a.auditMu.Unlock()
	return a.logBuf.String()
}

// Addr is "host:port".
func (a *Agent) Addr() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// Dial is a ProxyDialer: it ignores the address it is given and connects to
// the agent, so a client may use any node name and still reach it.
func (a *Agent) Dial(ctx context.Context, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", a.Addr())
}

// ClientCert is a client certificate and key on disk, in the form the client's
// configuration takes.
type ClientCert struct{ Cert, Key string }

// IssueClient writes a client certificate for cn signed by the agent's CA. It
// is only meaningful in MTLS mode.
func (a *Agent) IssueClient(t testing.TB, cn string) ClientCert {
	t.Helper()
	return a.pki.writeClient(t, a.dir, a.pki.ca, cn)
}

// IssueForeignClient writes a client certificate for cn signed by a different
// CA than the agent trusts.
func (a *Agent) IssueForeignClient(t testing.TB, cn string) ClientCert {
	t.Helper()
	return a.pki.writeClient(t, a.dir, newCA(t, "foreign CA"), cn)
}

// Audit returns every audit record written so far, decoded.
func (a *Agent) Audit() []map[string]any {
	a.auditMu.Lock()
	defer a.auditMu.Unlock()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(a.auditBuf.Bytes()))
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			break
		}
		if rec["component"] == "audit" {
			out = append(out, rec)
		}
	}
	return out
}

// AuditEvents returns the audit records whose event is name ("session_start",
// "auth_decision", ...).
func (a *Agent) AuditEvents(name string) []map[string]any {
	var out []map[string]any
	for _, r := range a.Audit() {
		if r["event"] == name {
			out = append(out, r)
		}
	}
	return out
}

// WaitAuditEvents waits for at least n audit records with this event name and
// returns them, failing the test if they do not arrive.
//
// An audit record is written when the AGENT's side of the work finishes, which
// is not the moment the client's call returns: server/exec.go sends the exit
// code and only then writes session_end, so a test that reads once can look
// before that line exists. It passed on an idle machine and failed on a loaded
// CI runner, which is the worst way for a test to be wrong — the assertion was
// about the scheduler, not about the agent. Waiting makes it about the agent
// again, and costs nothing when the record is already there.
func (a *Agent) WaitAuditEvents(t testing.TB, name string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		recs := a.AuditEvents(name)
		if len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("agenttest: %d audit %q record(s) within 5s, want %d: %v", len(recs), name, n, recs)
			return recs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Start builds the agent the way cmd/agent's buildServer does and serves it on
// 127.0.0.1 until the test ends.
func Start(t testing.TB, o Options) *Agent {
	t.Helper()
	if o.Secret == "" && !o.MTLS {
		t.Fatal("agenttest: Options needs Secret or MTLS; an agent with neither authenticates nobody")
	}
	if o.Docker == nil {
		o.Docker = NewDocker()
	}
	a := &Agent{Host: "127.0.0.1", Secret: o.Secret, Docker: o.Docker, dir: t.TempDir()}

	log := slog.New(slog.NewTextHandler(lockedWriter{&a.auditMu, &a.logBuf}, nil))
	identityVerified := o.MTLS
	auditLog := audit.New(slog.New(slog.NewJSONHandler(
		lockedWriter{&a.auditMu, &a.auditBuf}, &slog.HandlerOptions{Level: slog.LevelInfo})), identityVerified)

	// TLS: provisioned certificates (mTLS) or a generated self-signed one.
	var tlsCfg *tls.Config
	if o.MTLS {
		a.pki = newPKI(t)
		caPath, certPath, keyPath := a.pki.writeServer(t, a.dir)
		cfg, err := tlsconf.ServerConfig(caPath, certPath, keyPath)
		if err != nil {
			t.Fatalf("agenttest: tlsconf.ServerConfig: %v", err)
		}
		tlsCfg, a.CAFile = cfg, caPath
	} else {
		a.pki = newPKI(t) // for IssueForeignClient's signature only
		cfg, _, err := tlsconf.SelfSignedServerConfig([]string{"127.0.0.1", "localhost"}, "", time.Now())
		if err != nil {
			t.Fatalf("agenttest: tlsconf.SelfSignedServerConfig: %v", err)
		}
		tlsCfg = cfg
		a.CAFile = writeCertPEM(t, a.dir, tlsCfg.Certificates[0].Certificate[0])
	}

	var authz auth.Authorizer = auth.AllowAll{}
	if o.Policy != "" {
		p := filepath.Join(a.dir, "policy.yaml")
		if err := os.WriteFile(p, []byte(o.Policy), 0o600); err != nil {
			t.Fatal(err)
		}
		pol, err := auth.LoadPolicy(p, identityVerified)
		if err != nil {
			t.Fatalf("agenttest: policy: %v", err)
		}
		authz = pol
	}

	so := o.Server
	so.SecretAuth = !o.MTLS // lenient identity only when no client certificate is required
	srv := server.New(o.Docker, authz, auditLog, log, o.Metrics, so)
	a.Server = srv

	var secretAuth *server.SecretAuth
	if o.Secret != "" {
		secretAuth = &server.SecretAuth{
			Secret:      o.Secret,
			CertDER:     tlsCfg.Certificates[0].Certificate[0],
			AllowLegacy: o.AllowLegacySecret,
			Log:         log,
			Metrics:     o.Metrics,
		}
	}
	opts := append([]grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsCfg))}, server.Interceptors(log, secretAuth, srv)...)
	a.grpc = grpc.NewServer(opts...)
	pb.RegisterAgentServer(a.grpc, srv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.Port = lis.Addr().(*net.TCPAddr).Port
	go func() { _ = a.grpc.Serve(lis) }()
	t.Cleanup(a.grpc.Stop)
	return a
}
