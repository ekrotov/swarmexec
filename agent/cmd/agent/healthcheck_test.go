// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

// testAgent serves the real Agent service over TLS on loopback, the way main
// wires it, and returns its address.
func testAgent(t *testing.T, secret string, mutate func(*tls.Config)) string {
	t.Helper()
	tlsCfg, _, err := tlsconf.SelfSignedServerConfig([]string{"IP:127.0.0.1"}, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(tlsCfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, auth.AllowAll{}, audit.New(log, true), log, nil, server.Options{})
	opts := []grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsCfg))}
	if secret != "" {
		sa := &server.SecretAuth{Secret: secret, CertDER: tlsCfg.Certificates[0].Certificate[0]}
		opts = append(opts, grpc.ChainUnaryInterceptor(sa.UnaryInterceptor()), grpc.ChainStreamInterceptor(sa.StreamInterceptor()))
	}
	gs := grpc.NewServer(opts...)
	pb.RegisterAgentServer(gs, srv)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis.Addr().String()
}

// testDocker answers the daemon's /_ping, as dockerd does.
func testDocker(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/_ping") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Api-Version", "1.45")
		_, _ = io.WriteString(w, "OK")
	}))
	t.Cleanup(ts.Close)
	return "tcp://" + ts.Listener.Addr().String()
}

// deadAddr is a loopback address nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

func noCmdline() ([]byte, error) { return nil, errors.New("no /proc here") }

func probe(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), healthTotalTimeout)
	defer cancel()
	return healthcheck(ctx, args, noCmdline)
}

// The mode `swarmexec init` deploys: self-signed, shared secret, no client CA.
// The probe holds no secret, and the refusal of the auth layer is an answer
// from a live agent.
func TestHealthcheck_HealthySharedSecretAgent(t *testing.T) {
	addr := testAgent(t, "s3cret", nil)
	msg, err := probe(t, "-listen="+addr, "-self-signed", "-agent-secret=s3cret", "-docker-host="+testDocker(t))
	if err != nil {
		t.Fatalf("healthy agent reported unhealthy: %v", err)
	}
	if !strings.Contains(msg, "docker reachable") {
		t.Errorf("message = %q", msg)
	}
}

// Without a secret the probe gets Version itself.
func TestHealthcheck_HealthyAgentAnswersVersion(t *testing.T) {
	addr := testAgent(t, "", nil)
	msg, err := probe(t, "-listen="+addr, "-docker-host="+testDocker(t))
	if err != nil {
		t.Fatalf("healthy agent reported unhealthy: %v", err)
	}
	if !strings.Contains(msg, "answering on "+addr) {
		t.Errorf("message = %q", msg)
	}
}

// With a client CA the agent requires a certificate the probe does not have;
// the TLS layer refusing it, in either TLS version, is a live listener.
func TestHealthcheck_HealthyClientCertAgent(t *testing.T) {
	for _, v := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		addr := testAgent(t, "", func(c *tls.Config) {
			c.ClientAuth = tls.RequireAndVerifyClientCert
			c.ClientCAs = x509.NewCertPool()
			c.MaxVersion = v
		})
		if _, err := probe(t, "-listen="+addr, "-ca-cert=/run/secrets/ca", "-docker-host="+testDocker(t)); err != nil {
			t.Errorf("TLS %x: healthy mTLS agent reported unhealthy: %v", v, err)
		}
	}
}

func TestHealthcheck_UnhealthyWhenNothingListens(t *testing.T) {
	_, err := probe(t, "-listen="+deadAddr(t), "-docker-host="+testDocker(t))
	if err == nil || !strings.Contains(err.Error(), "not answering") {
		t.Fatalf("want a not-answering error, got %v", err)
	}
	_, err = probe(t, "-listen="+deadAddr(t), "-ca-cert=/x", "-docker-host="+testDocker(t))
	if err == nil {
		t.Fatal("mTLS mode: nothing listening reported healthy")
	}
}

// A listener that accepts and never speaks — a wedged process — is not an
// answer, and the probe gives up inside its own bound.
func TestHealthcheck_UnhealthyWhenListenerHangs(t *testing.T) {
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
			defer c.Close()
		}
	}()
	start := time.Now()
	if _, err := probe(t, "-listen="+lis.Addr().String(), "-docker-host="+testDocker(t)); err == nil {
		t.Fatal("a silent listener reported healthy")
	}
	if d := time.Since(start); d > healthTotalTimeout {
		t.Errorf("probe took %v, beyond its %v bound", d, healthTotalTimeout)
	}
}

func TestHealthcheck_UnhealthyWhenDockerUnreachable(t *testing.T) {
	addr := testAgent(t, "", nil)
	_, err := probe(t, "-listen="+addr, "-docker-host=tcp://"+deadAddr(t))
	if err == nil || !strings.Contains(err.Error(), "docker not reachable") {
		t.Fatalf("want a docker error, got %v", err)
	}
}

// The probe reads the agent's command line from PID 1: that is where -port
// (init) and -listen (stack files) live, and Docker does not pass them to a
// HEALTHCHECK.
func TestProbeArgs(t *testing.T) {
	cmdline := func(argv ...string) func() ([]byte, error) {
		return func() ([]byte, error) { return []byte(strings.Join(argv, "\x00") + "\x00"), nil }
	}
	cases := []struct {
		name        string
		own         []string
		pid1        func() ([]byte, error)
		want        []string
		wantSidecar bool
	}{
		{"init deployment", nil, cmdline("/agent", "-port=9555", "-self-signed"), []string{"-port=9555", "-self-signed"}, false},
		{"stack file", nil, cmdline("/agent", "-listen=:9443"), []string{"-listen=:9443"}, false},
		{"docker-init wrapper", nil, cmdline("/sbin/docker-init", "--", "/agent", "-port=9000"), []string{"-port=9000"}, false},
		{"own flags win", []string{"-port=1234"}, cmdline("/agent", "-port=9555"), []string{"-port=1234"}, false},
		{"no proc", nil, noCmdline, nil, false},
		{"foreign pid 1", nil, cmdline("/bin/sh", "-c", "x"), nil, false},
		{"mux sidecar", nil, cmdline("/agent", "-forward-mux-to", "127.0.0.1:5432"), nil, true},
		{"legacy sidecar", nil, cmdline("/agent", "-forward-to=127.0.0.1:5432"), nil, true},
	}
	for _, c := range cases {
		got, sidecar := probeArgs(c.own, c.pid1)
		if !slices.Equal(got, c.want) || sidecar != c.wantSidecar {
			t.Errorf("%s: got %q sidecar=%v, want %q sidecar=%v", c.name, got, sidecar, c.want, c.wantSidecar)
		}
	}
}

// A sidecar's netns is the target's: the probe must not dial anything there.
func TestHealthcheck_SidecarIsNotProbed(t *testing.T) {
	pid1 := func() ([]byte, error) { return []byte("/agent\x00-forward-mux-to\x00127.0.0.1:9443\x00"), nil }
	msg, err := healthcheck(context.Background(), nil, pid1)
	if err != nil || !strings.Contains(msg, "sidecar") {
		t.Fatalf("sidecar: msg=%q err=%v", msg, err)
	}
}

func TestProbeAddr(t *testing.T) {
	for in, want := range map[string]string{
		":9443":          "127.0.0.1:9443",
		"0.0.0.0:9443":   "127.0.0.1:9443",
		"[::]:9443":      "127.0.0.1:9443",
		"10.0.0.5:9443":  "10.0.0.5:9443",
		"127.0.0.1:1234": "127.0.0.1:1234",
	} {
		got, err := probeAddr(in)
		if err != nil || got != want {
			t.Errorf("probeAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := probeAddr("nonsense"); err == nil {
		t.Error("probeAddr accepted an address without a port")
	}
}

func TestHealthcheckMode(t *testing.T) {
	if rest, ok := healthcheckMode([]string{"-healthcheck"}); !ok || len(rest) != 0 {
		t.Errorf("-healthcheck: %q %v", rest, ok)
	}
	if rest, ok := healthcheckMode([]string{"--healthcheck", "-port=1"}); !ok || !slices.Equal(rest, []string{"-port=1"}) {
		t.Errorf("--healthcheck -port=1: %q %v", rest, ok)
	}
	if _, ok := healthcheckMode([]string{"-port=1"}); ok {
		t.Error("an agent command line was taken for the probe")
	}
}
