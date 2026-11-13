// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"swarmexec/agent/internal/audit"
	"swarmexec/agent/internal/auth"
	"swarmexec/agent/internal/server"
	"swarmexec/internal/authmeta"
	"swarmexec/internal/pb"
)

// The chain main builds must run the shared-secret check before the stream
// gate: a caller without the secret is told Unauthenticated, even while the
// agent drains, and only an authenticated caller learns it is shutting down.
func TestInterceptorChain_SecretBeforeStreamGate(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(nil, auth.AllowAll{}, audit.New(log, true), log, nil, server.Options{})
	srv.StartDrain()
	secretAuth := &server.SecretAuth{Secret: "s3cret", AllowLegacy: true}

	lis := bufconn.Listen(1 << 16)
	gs := grpc.NewServer(interceptors(log, secretAuth, srv)...)
	pb.RegisterAgentServer(gs, srv)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewAgentClient(conn)

	logsCode := func(ctx context.Context) codes.Code {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		st, err := client.Logs(ctx, &pb.LogsRequest{ContainerId: "c1"})
		if err == nil {
			_, err = st.Recv()
		}
		return status.Code(err)
	}

	if got := logsCode(context.Background()); got != codes.Unauthenticated {
		t.Errorf("no secret while draining: got %v, want Unauthenticated", got)
	}
	withSecret := metadata.AppendToOutgoingContext(context.Background(), authmeta.SecretKey, "s3cret")
	if got := logsCode(withSecret); got != codes.Unavailable {
		t.Errorf("with the secret while draining: got %v, want Unavailable", got)
	}
}
