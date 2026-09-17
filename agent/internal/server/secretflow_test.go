// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/server"
	"swarmexec/agent/internal/tlsconf"
	"swarmexec/internal/authmeta"
	pb "swarmexec/internal/pb"
)

// stubAgent is a minimal Agent server used to exercise the secret interceptor
// and self-signed TLS end-to-end.
type stubAgent struct{ pb.UnimplementedAgentServer }

func (stubAgent) ListContainers(context.Context, *pb.ListRequest) (*pb.ListResponse, error) {
	return &pb.ListResponse{}, nil
}

// rawCreds sends the secret itself, the way clients did before connection
// binding. Kept so the legacy path can be exercised from the outside.
type rawCreds struct{ s string }

func (c rawCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{authmeta.SecretKey: c.s}, nil
}
func (rawCreds) RequireTransportSecurity() bool { return true }

// boundCreds mirrors what the cli sends today: a proof over the certificate of
// the connection this very call travels on.
type boundCreds struct{ s string }

func (c boundCreds) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	ri, _ := credentials.RequestInfoFromContext(ctx)
	tlsInfo := ri.AuthInfo.(credentials.TLSInfo)
	return map[string]string{
		authmeta.BindingKey: authmeta.Bind(c.s, tlsInfo.State.PeerCertificates[0].Raw),
	}, nil
}
func (boundCreds) RequireTransportSecurity() bool { return true }

// fixedCreds sends a proof computed over SOMEONE ELSE'S certificate — what an
// attacker who terminated an unverified connection would have captured.
type fixedCreds struct{ proof string }

func (c fixedCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{authmeta.BindingKey: c.proof}, nil
}
func (fixedCreds) RequireTransportSecurity() bool { return true }

// TestSelfSignedSecretFlow wires the real self-signed server TLS + the secret
// interceptors and drives them over a real (skip-verify) gRPC connection —
// exactly the documented deployment mode this is meant to protect.
func TestSelfSignedSecretFlow(t *testing.T) {
	serve := func(t *testing.T, allowLegacy bool) (addr string, certDER []byte) {
		t.Helper()
		tlsCfg, _, err := tlsconf.SelfSignedServerConfig([]string{"IP:127.0.0.1", "DNS:swarmexec-agent"}, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		auth := &server.SecretAuth{
			Secret:      "topsecret",
			CertDER:     tlsCfg.Certificates[0].Certificate[0],
			AllowLegacy: allowLegacy,
		}
		gs := grpc.NewServer(
			grpc.Creds(credentials.NewTLS(tlsCfg)),
			grpc.ChainUnaryInterceptor(auth.UnaryInterceptor()),
			grpc.ChainStreamInterceptor(auth.StreamInterceptor()),
		)
		pb.RegisterAgentServer(gs, stubAgent{})
		go gs.Serve(lis)
		t.Cleanup(gs.Stop)
		return lis.Addr().String(), auth.CertDER
	}

	dialOnce := func(addr string, creds credentials.PerRPCCredentials) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := grpc.DialContext(ctx, addr, //nolint:staticcheck
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})),
			grpc.WithPerRPCCredentials(creds),
			grpc.WithBlock(),
		)
		if err != nil {
			return err
		}
		defer conn.Close()
		_, err = pb.NewAgentClient(conn).ListContainers(context.Background(), &pb.ListRequest{})
		return err
	}

	t.Run("bound proof is accepted", func(t *testing.T) {
		addr, _ := serve(t, false)
		if err := dialOnce(addr, boundCreds{s: "topsecret"}); err != nil {
			t.Fatalf("valid binding should succeed: %v", err)
		}
		if err := dialOnce(addr, boundCreds{s: "wrong"}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("wrong secret should be Unauthenticated, got %v", err)
		}
	})

	// The point of the whole exercise. An attacker who presents their own
	// certificate on an unverified connection collects a proof — and it does not
	// work against the real agent, whose certificate is different. Before
	// binding, what they collected was the secret itself, valid on every node.
	t.Run("a proof captured elsewhere is useless here", func(t *testing.T) {
		addr, realCert := serve(t, true) // legacy allowed: still no way in
		captured := authmeta.Bind("topsecret", []byte("attacker-certificate-der"))

		err := dialOnce(addr, fixedCreds{proof: captured})
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("a captured proof must not authenticate, got %v", err)
		}
		// Sanity: the same secret over the REAL certificate does work, so the
		// rejection above is about the binding and not about the secret.
		if err := dialOnce(addr, fixedCreds{proof: authmeta.Bind("topsecret", realCert)}); err != nil {
			t.Fatalf("proof over the real certificate should succeed: %v", err)
		}
	})

	t.Run("legacy raw secret depends on the opt-in", func(t *testing.T) {
		lenient, _ := serve(t, true)
		if err := dialOnce(lenient, rawCreds{s: "topsecret"}); err != nil {
			t.Fatalf("legacy client should still work while allowed: %v", err)
		}

		strict, _ := serve(t, false)
		if err := dialOnce(strict, rawCreds{s: "topsecret"}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("raw secret should be refused when not allowed, got %v", err)
		}
	})
}
