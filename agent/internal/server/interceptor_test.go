// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"swarmexec/internal/authmeta"
)

func ctxWithMD(pairs ...string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

var testCert = []byte("agent-leaf-certificate-der")

func testAuth(secret string) *SecretAuth {
	return &SecretAuth{Secret: secret, CertDER: testCert, AllowLegacy: true}
}

func TestSecretUnaryInterceptor(t *testing.T) {
	intc := testAuth("s3cr3t").UnaryInterceptor()
	called := false
	handler := func(context.Context, any) (any, error) { called = true; return "ok", nil }

	// A proof bound to this connection -> handler runs.
	proof := authmeta.Bind("s3cr3t", testCert)
	if _, err := intc(ctxWithMD(authmeta.BindingKey, proof), nil, &grpc.UnaryServerInfo{}, handler); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}
	if !called {
		t.Fatal("handler should have been called")
	}

	// Wrong secret -> Unauthenticated, handler not run.
	called = false
	bad := authmeta.Bind("wrong", testCert)
	_, err := intc(ctxWithMD(authmeta.BindingKey, bad), nil, &grpc.UnaryServerInfo{}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	if called {
		t.Fatal("handler must not run on bad secret")
	}

	// Missing credentials -> Unauthenticated.
	if _, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{}, handler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated for missing secret, got %v", err)
	}
}

// The attack the binding exists to stop: a proof captured from a connection to
// an impostor is worthless against the real agent, because the agent computes
// the expected value over its OWN certificate.
func TestSecretAuth_ProofFromAnotherConnectionIsRejected(t *testing.T) {
	a := testAuth("s3cr3t")
	stolen := authmeta.Bind("s3cr3t", []byte("man-in-the-middle-certificate"))

	err := a.check(ctxWithMD(authmeta.BindingKey, stolen))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("a proof bound to a different certificate must be rejected, got %v", err)
	}
}

func TestSecretAuth_LegacyRawSecret(t *testing.T) {
	// Accepted while the legacy form is allowed, so an agent upgrade does not
	// strand older clients.
	if err := testAuth("tok").check(ctxWithMD(authmeta.SecretKey, "tok")); err != nil {
		t.Fatalf("legacy secret rejected while allowed: %v", err)
	}
	// …and refused once it is not, with an error that says what to do.
	strict := &SecretAuth{Secret: "tok", CertDER: testCert}
	err := strict.check(ctxWithMD(authmeta.SecretKey, "tok"))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	if !strings.Contains(err.Error(), "-allow-legacy-secret") {
		t.Errorf("error should name the flag: %v", err)
	}
	// A wrong raw secret is refused even when the legacy form is allowed.
	if err := testAuth("tok").check(ctxWithMD(authmeta.SecretKey, "nope")); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

// streamWithCtx is a minimal grpc.ServerStream carrying a context.
type streamWithCtx struct {
	grpc.ServerStream
	ctx context.Context
}

func (s streamWithCtx) Context() context.Context { return s.ctx }

func TestSecretStreamInterceptor(t *testing.T) {
	intc := testAuth("tok").StreamInterceptor()
	ran := false
	handler := func(any, grpc.ServerStream) error { ran = true; return nil }

	good := ctxWithMD(authmeta.BindingKey, authmeta.Bind("tok", testCert))
	if err := intc(nil, streamWithCtx{ctx: good}, &grpc.StreamServerInfo{}, handler); err != nil {
		t.Fatalf("valid binding rejected: %v", err)
	}
	if !ran {
		t.Fatal("stream handler should have run")
	}

	ran = false
	bad := ctxWithMD(authmeta.BindingKey, authmeta.Bind("nope", testCert))
	err := intc(nil, streamWithCtx{ctx: bad}, &grpc.StreamServerInfo{}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	if ran {
		t.Fatal("stream handler must not run on bad secret")
	}
}

func TestIdentityFromContextLenient(t *testing.T) {
	// Operator header is used when present (no client cert).
	id, err := identityFromContextLenient(ctxWithMD(authmeta.OperatorKey, "alice"))
	if err != nil || id != "alice" {
		t.Fatalf("want alice, got %q (err %v)", id, err)
	}
	// Falls back to anonymous with nothing.
	id, err = identityFromContextLenient(context.Background())
	if err != nil || id != "anonymous" {
		t.Fatalf("want anonymous, got %q (err %v)", id, err)
	}
}
