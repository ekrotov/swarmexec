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

// secretCreds is a per-RPC credential mirroring what the cli sends.
type secretCreds struct{ s string }

func (c secretCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{authmeta.SecretKey: c.s}, nil
}
func (secretCreds) RequireTransportSecurity() bool { return true }

// TestSelfSignedSecretFlow wires the real self-signed server TLS + secret
// interceptors and drives them over a real (skip-verify) gRPC connection.
func TestSelfSignedSecretFlow(t *testing.T) {
	tlsCfg, _, err := tlsconf.SelfSignedServerConfig([]string{"IP:127.0.0.1", "DNS:swarmexec-agent"}, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.ChainUnaryInterceptor(server.SecretUnaryInterceptor("topsecret")),
		grpc.ChainStreamInterceptor(server.SecretStreamInterceptor("topsecret")),
	)
	pb.RegisterAgentServer(gs, stubAgent{})
	go gs.Serve(lis)
	defer gs.Stop()

	dialOnce := func(secret string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := grpc.DialContext(ctx, lis.Addr().String(), //nolint:staticcheck
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})),
			grpc.WithPerRPCCredentials(secretCreds{s: secret}),
			grpc.WithBlock(),
		)
		if err != nil {
			return err
		}
		defer conn.Close()
		_, err = pb.NewAgentClient(conn).ListContainers(context.Background(), &pb.ListRequest{})
		return err
	}

	if err := dialOnce("topsecret"); err != nil {
		t.Fatalf("valid secret should succeed: %v", err)
	}
	if err := dialOnce("wrong"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong secret should be Unauthenticated, got %v", err)
	}
}
