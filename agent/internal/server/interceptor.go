// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"sync/atomic"

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

// SecretAuth verifies the shared secret on every RPC.
//
// It accepts two forms. The current one is a proof bound to this connection's
// server certificate (authmeta.Bind): the secret itself never crosses the wire,
// so an on-path attacker who terminates an unverified TLS connection captures
// nothing that works against a real agent. The legacy form is the raw secret,
// which an agent accepts only while AllowLegacy is set — every acceptance is
// logged, because it means some client is still handing the credential to
// whatever answers.
type SecretAuth struct {
	Secret string
	// CertDER is this server's leaf certificate, the value a client's proof is
	// computed over. Without it only the legacy form can be verified.
	CertDER []byte
	// AllowLegacy accepts the raw secret from clients that predate binding.
	AllowLegacy bool
	Log         *slog.Logger

	warnedLegacy atomic.Bool
}

// check verifies the incoming credentials in constant time.
func (a *SecretAuth) check(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)

	if proof := firstMD(md, authmeta.BindingKey); proof != "" {
		if len(a.CertDER) == 0 {
			return status.Error(codes.Unauthenticated,
				"connection-bound authentication is unavailable on this agent")
		}
		want := authmeta.Bind(a.Secret, a.CertDER)
		if subtle.ConstantTimeCompare([]byte(proof), []byte(want)) != 1 {
			return status.Error(codes.Unauthenticated, "invalid agent secret")
		}
		return nil
	}

	if raw := firstMD(md, authmeta.SecretKey); raw != "" {
		if !a.AllowLegacy {
			return status.Error(codes.Unauthenticated,
				"this client sends the raw shared secret, which this agent no longer accepts; "+
					"upgrade the client, or start the agent with -allow-legacy-secret")
		}
		if subtle.ConstantTimeCompare([]byte(raw), []byte(a.Secret)) != 1 {
			return status.Error(codes.Unauthenticated, "invalid agent secret")
		}
		// Once per agent lifetime: a line per RPC would bury it, and the fact
		// being reported is a standing condition, not an event.
		if a.Log != nil && a.warnedLegacy.CompareAndSwap(false, true) {
			a.Log.Warn("accepted a RAW shared secret from a legacy client",
				"why", "the credential travelled on the wire and can be captured on an unverified connection",
				"fix", "upgrade the clients, then drop -allow-legacy-secret")
		}
		return nil
	}

	return status.Error(codes.Unauthenticated, "missing agent secret")
}

// UnaryInterceptor rejects unary RPCs without valid credentials.
func (a *SecretAuth) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := a.check(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor rejects streaming RPCs without valid credentials.
func (a *SecretAuth) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := a.check(ss.Context()); err != nil {
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
