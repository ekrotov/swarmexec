// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package dial

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"swarmexec/client/internal/config"
	"swarmexec/internal/authmeta"
	"swarmexec/internal/pb"
)

// These tests pin down what Dial promises its callers, against a real TLS
// gRPC server: it returns a READY connection or a clear error, it fails fast
// on a certificate problem, it waits out a node that is not listening yet only
// until the connect timeout, and it hands a tunnel the name it was given.

// selfSigned writes a self-signed certificate for the given DNS names and
// returns the TLS pair plus the PEM file path (usable as a CA).
func selfSigned(t *testing.T, names ...string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: names[0]},
		DNSNames:              names,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return pair, p
}

// versionServer answers Version and records the metadata of the last call.
type versionServer struct {
	pb.UnimplementedAgentServer
	mu sync.Mutex
	md metadata.MD
}

func (s *versionServer) Version(ctx context.Context, _ *pb.VersionRequest) (*pb.VersionResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.mu.Lock()
	s.md = md
	s.mu.Unlock()
	return &pb.VersionResponse{}, nil
}

func (s *versionServer) lastMD() metadata.MD {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.md
}

// serveTLS starts a TLS gRPC agent stand-in on 127.0.0.1 and returns its port.
func serveTLS(t *testing.T, pair tls.Certificate) (int, *versionServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}})))
	vs := &versionServer{}
	pb.RegisterAgentServer(srv, vs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().(*net.TCPAddr).Port, vs
}

func connectCtx(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestDial_ReturnsAReadyConnection(t *testing.T) {
	pair, caPath := selfSigned(t, "localhost")
	port, _ := serveTLS(t, pair)

	conn, err := Dial(connectCtx(t, 5*time.Second), "127.0.0.1", port, config.Config{CA: caPath, ServerName: "localhost"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Ready on return, not "will connect on first use": callers report an
	// unreachable node at connect time, and doctor's per-node verdict is the
	// return value of this call.
	if s := conn.GetState(); s != connectivity.Ready {
		t.Errorf("state after Dial = %v, want READY", s)
	}
}

// A certificate the client does not trust is not going to fix itself: fail at
// once, well inside the connect timeout, and say that it is the certificate.
func TestDial_UntrustedCertificateFailsFastWithTheReason(t *testing.T) {
	served, _ := selfSigned(t, "localhost")
	_, otherCA := selfSigned(t, "localhost") // a CA that did not sign it
	port, _ := serveTLS(t, served)

	start := time.Now()
	_, err := Dial(connectCtx(t, 10*time.Second), "127.0.0.1", port, config.Config{CA: otherCA, ServerName: "localhost"})
	if err == nil {
		t.Fatal("dial to an untrusted server must fail")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("took %v: a certificate error must not wait for the timeout", el)
	}
	for _, want := range []string{"cannot reach agent", "certificate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Nothing listening fails at once too, with the reason — the behaviour Dial
// had before NewClient, kept: doctor reports a node without an agent in
// milliseconds instead of a timeout per node.
func TestDial_NothingListeningFailsFastWithTheReason(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	lis.Close() // nothing on that port any more

	start := time.Now()
	_, err = Dial(connectCtx(t, 10*time.Second), "127.0.0.1", port, config.Config{Insecure: true})
	if err == nil {
		t.Fatal("dial with nothing listening must fail")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("took %v: a refused connection must not wait for the timeout", el)
	}
	if !strings.Contains(err.Error(), "cannot reach agent") || !strings.Contains(err.Error(), "refused") {
		t.Errorf("error %q should name the agent and the refused connection", err)
	}
}

// A peer that accepts TCP and then says nothing is still "connecting" — it
// may be slow, not broken — so Dial waits for the connect timeout, no longer.
func TestDial_SilentPeerWaitsForTheTimeout(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, never answer the handshake
		}
	}()
	port := lis.Addr().(*net.TCPAddr).Port

	start := time.Now()
	_, err = Dial(connectCtx(t, 1500*time.Millisecond), "127.0.0.1", port, config.Config{Insecure: true})
	if err == nil {
		t.Fatal("dial to a silent peer must fail")
	}
	if el := time.Since(start); el < time.Second || el > 5*time.Second {
		t.Errorf("gave up after %v, want about the 1.5s connect timeout", el)
	}
	if !strings.Contains(err.Error(), "deadline") {
		t.Errorf("error %q should say the connect timeout ran out", err)
	}
}

// Through an ssh bastion the node name often resolves only on the far side.
// The tunnel must receive the name as given, never a locally resolved address
// — a name that does not resolve here must still work.
func TestDial_TunnelReceivesTheUnresolvedName(t *testing.T) {
	pair, _ := selfSigned(t, "localhost")
	port, _ := serveTLS(t, pair)

	var gotAddr string
	var mu sync.Mutex
	cfg := config.Config{Insecure: true, ProxyDialer: func(ctx context.Context, addr string) (net.Conn, error) {
		mu.Lock()
		gotAddr = addr
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}}
	conn, err := Dial(connectCtx(t, 5*time.Second), "node-3.swarm.invalid", port, cfg)
	if err != nil {
		t.Fatalf("dial through the tunnel: %v", err)
	}
	defer conn.Close()
	mu.Lock()
	defer mu.Unlock()
	if want := net.JoinHostPort("node-3.swarm.invalid", strconv.Itoa(port)); gotAddr != want {
		t.Errorf("tunnel was asked for %q, want %q", gotAddr, want)
	}
}

// Self-signed mode end to end: the RPC carries the proof bound to THIS
// connection's certificate, and never the secret.
func TestDial_SelfSignedSendsTheBoundProof(t *testing.T) {
	pair, _ := selfSigned(t, "localhost")
	port, vs := serveTLS(t, pair)

	conn, err := Dial(connectCtx(t, 5*time.Second), "127.0.0.1", port, config.Config{Insecure: true, AgentSecret: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := pb.NewAgentClient(conn).Version(connectCtx(t, 5*time.Second), &pb.VersionRequest{}); err != nil {
		t.Fatalf("version: %v", err)
	}
	md := vs.lastMD()
	if len(md.Get(authmeta.SecretKey)) != 0 {
		t.Error("the raw secret went over the wire")
	}
	if got := md.Get(authmeta.BindingKey); len(got) != 1 || got[0] != authmeta.Bind("s3cret", pair.Certificate[0]) {
		t.Errorf("binding = %v, want the proof bound to the served certificate", got)
	}
}
