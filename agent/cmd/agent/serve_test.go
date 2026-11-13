// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/config"
	"swarmexec/agent/internal/server"
	"swarmexec/internal/authmeta"
	"swarmexec/internal/pb"
)

// recorder collects the shutdown calls of the fakes below, in order.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(c string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// fakeDrainer drains at once when drains is set; otherwise WaitForSessions
// blocks until its context ends, like a session that never finishes.
type fakeDrainer struct {
	rec    *recorder
	drains bool
}

func (d *fakeDrainer) StartDrain() { d.rec.add("StartDrain") }
func (d *fakeDrainer) WaitForSessions(ctx context.Context) bool {
	d.rec.add("WaitForSessions")
	if d.drains {
		return true
	}
	<-ctx.Done()
	return false
}
func (d *fakeDrainer) ActiveSessions() int64 { return 1 }

// fakeGRPC serves until stopped; with hangGraceful its GracefulStop waits
// for Stop, like a grpc.Server holding a unary RPC from a client that never
// hangs up.
type fakeGRPC struct {
	rec          *recorder
	hangGraceful bool
	stopped      chan struct{}
	once         sync.Once
	serveErr     error
}

func newFakeGRPC(rec *recorder) *fakeGRPC { return &fakeGRPC{rec: rec, stopped: make(chan struct{})} }

func (g *fakeGRPC) Serve(net.Listener) error {
	if g.serveErr != nil {
		return g.serveErr
	}
	<-g.stopped
	return nil
}
func (g *fakeGRPC) GracefulStop() {
	g.rec.add("GracefulStop")
	if g.hangGraceful {
		<-g.stopped
		return
	}
	g.once.Do(func() { close(g.stopped) })
}
func (g *fakeGRPC) Stop() {
	g.rec.add("Stop")
	g.once.Do(func() { close(g.stopped) })
}

func stubAgent(d drainer, g grpcServer) *agent {
	return &agent{
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		srv:             d,
		grpc:            g,
		background:      func(context.Context) {},
		drainTimeout:    50 * time.Millisecond,
		gracefulTimeout: 50 * time.Millisecond,
		handlerTimeout:  50 * time.Millisecond,
	}
}

// R-01: the stop sequence. Drain first (new streams refused), wait for the
// running ones, and only then stop the server — gracefully when they ended,
// bounded so a client that never hangs up cannot hold the agent until SIGKILL,
// and by force when the drain window ran out, with a last bounded wait for the
// cancelled handlers to write their audit records.
func TestShutdownSequence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		drains       bool
		hangGraceful bool
		want         []string
	}{
		{"drained, graceful stop finishes", true, false,
			[]string{"StartDrain", "WaitForSessions", "GracefulStop"}},
		{"drained, graceful stop bounded then forced", true, true,
			[]string{"StartDrain", "WaitForSessions", "GracefulStop", "Stop"}},
		{"drain window elapsed: forced stop, then wait for handlers", false, false,
			[]string{"StartDrain", "WaitForSessions", "Stop", "WaitForSessions"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			g := newFakeGRPC(rec)
			g.hangGraceful = tc.hangGraceful
			a := stubAgent(&fakeDrainer{rec: rec, drains: tc.drains}, g)

			start := time.Now()
			a.shutdown(syscall.SIGTERM)
			if el := time.Since(start); el > 2*time.Second {
				t.Fatalf("shutdown took %v; every wait in it must be bounded", el)
			}
			if got := rec.got(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sequence = %v\nwant       %v", got, tc.want)
			}
		})
	}
}

// serve runs the background work for exactly its own lifetime, and a signal
// ends it with the shutdown sequence and a nil error.
func TestServe_SignalShutsDown(t *testing.T) {
	rec := &recorder{}
	a := stubAgent(&fakeDrainer{rec: rec, drains: true}, newFakeGRPC(rec))
	var bg context.Context
	a.background = func(ctx context.Context) { bg = ctx }

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	stop := make(chan os.Signal, 1)
	stop <- syscall.SIGTERM
	if err := a.serve(lis, stop); err != nil {
		t.Fatalf("serve = %v after a signal, want nil", err)
	}
	if bg == nil || bg.Err() == nil {
		t.Error("the background work must be started and stopped with serve")
	}
	if got := rec.got(); len(got) == 0 || got[0] != "StartDrain" {
		t.Errorf("no shutdown sequence ran: %v", got)
	}
}

func TestServe_ServeErrorIsReturned(t *testing.T) {
	rec := &recorder{}
	g := newFakeGRPC(rec)
	g.serveErr = errors.New("listener closed")
	a := stubAgent(&fakeDrainer{rec: rec, drains: true}, g)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	if err := a.serve(lis, make(chan os.Signal)); err == nil || err.Error() != "listener closed" {
		t.Fatalf("serve = %v, want the serve error", err)
	}
	if got := rec.got(); len(got) != 0 {
		t.Errorf("a failed serve must not run the shutdown sequence: %v", got)
	}
}

// infoDocker answers Info, the one call buildServer makes; anything else
// would hit the nil embed, which is the assertion that building starts no
// background work (serve does).
type infoDocker struct {
	server.DockerClient
	name, addr string
}

func (d infoDocker) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	var info system.Info
	info.Name = d.name
	info.Swarm.NodeAddr = d.addr
	return client.SystemInfoResult{Info: info}, nil
}

// buildServer wires a real gRPC server end to end: a self-signed certificate
// carrying the node's SANs, the shared-secret check in front of the handlers,
// and the drain on shutdown — exercised over a real TLS connection.
func TestBuildServer_SelfSignedWithSecret(t *testing.T) {
	cfg := &config.Config{
		SelfSigned:        true,
		AgentSecret:       "s3cret",
		AllowLegacySecret: true,
		DrainTimeout:      time.Second,
	}
	var auditBuf bytes.Buffer
	a, err := buildServer(cfg, deps{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		audit:  &auditBuf,
		docker: infoDocker{name: "node-1", addr: "10.0.0.7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.drainTimeout != time.Second || a.gracefulTimeout != gracefulStopTimeout || a.handlerTimeout != handlerExitTimeout {
		t.Errorf("timeouts not taken from config/consts: %+v", a)
	}
	a.background = func(context.Context) {} // the fake docker has no caches to feed

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- a.serve(lis, stop) }()

	// Unverified on purpose, as a client of a self-signed agent in insecure
	// mode is: the certificate is captured to check its SANs and to compute
	// the connection-bound proof over, which is what secret mode rests on.
	var (
		mu       sync.Mutex
		peerCert []byte
	)
	tlsCfg := &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			mu.Lock()
			defer mu.Unlock()
			peerCert = raw[0]
			return nil
		},
	}
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	agentClient := pb.NewAgentClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := agentClient.Version(ctx, &pb.VersionRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no secret: %v, want Unauthenticated", err)
	}
	mu.Lock()
	der := peerCert
	mu.Unlock()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cert.DNSNames, "node-1") || !slices.ContainsFunc(cert.IPAddresses, func(ip net.IP) bool { return ip.String() == "10.0.0.7" }) {
		t.Errorf("SANs from Docker info missing: DNS %v IP %v", cert.DNSNames, cert.IPAddresses)
	}
	bound := metadata.AppendToOutgoingContext(ctx, authmeta.BindingKey, authmeta.Bind("s3cret", der))
	if _, err := agentClient.Version(bound, &pb.VersionRequest{}); err != nil {
		t.Fatalf("with the connection-bound secret: %v", err)
	}

	stop <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the signal")
	}
}
