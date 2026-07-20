// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"swarmexec/internal/authmeta"
)

// firstMD returns the first metadata value for key, or "".
func firstMD(md metadata.MD, key string) string {
	if vs := md.Get(key); len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// checkSecret verifies the incoming shared secret in constant time.
func checkSecret(ctx context.Context, want string) error {
	md, _ := metadata.FromIncomingContext(ctx)
	got := firstMD(md, authmeta.SecretKey)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid or missing agent secret")
	}
	return nil
}

// SecretUnaryInterceptor rejects unary RPCs without the matching shared secret.
func SecretUnaryInterceptor(secret string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := checkSecret(ctx, secret); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// SecretStreamInterceptor rejects streaming RPCs without the matching shared secret.
func SecretStreamInterceptor(secret string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := checkSecret(ss.Context(), secret); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// identityFromContextLenient resolves the operator identity for audit without
// requiring a client certificate: it prefers the verified client-cert CN, then
// a client-supplied operator header, and falls back to "anonymous". Used in
// self-signed / shared-secret mode where a client cert may be absent.
func identityFromContextLenient(ctx context.Context) (string, error) {
	if id, err := identityFromPeer(ctx); err == nil && id != "" {
		return id, nil
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if op := firstMD(md, authmeta.OperatorKey); op != "" {
			return op, nil
		}
	}
	return "anonymous", nil
}
