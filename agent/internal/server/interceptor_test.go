package server

import (
	"context"
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

func TestSecretUnaryInterceptor(t *testing.T) {
	intc := SecretUnaryInterceptor("s3cr3t")
	called := false
	handler := func(context.Context, any) (any, error) { called = true; return "ok", nil }

	// Correct secret -> handler runs.
	if _, err := intc(ctxWithMD(authmeta.SecretKey, "s3cr3t"), nil, &grpc.UnaryServerInfo{}, handler); err != nil {
		t.Fatalf("valid secret rejected: %v", err)
	}
	if !called {
		t.Fatal("handler should have been called")
	}

	// Wrong secret -> Unauthenticated, handler not run.
	called = false
	_, err := intc(ctxWithMD(authmeta.SecretKey, "wrong"), nil, &grpc.UnaryServerInfo{}, handler)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
	if called {
		t.Fatal("handler must not run on bad secret")
	}

	// Missing secret -> Unauthenticated.
	if _, err := intc(context.Background(), nil, &grpc.UnaryServerInfo{}, handler); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("want Unauthenticated for missing secret, got %v", err)
	}
}

// streamWithCtx is a minimal grpc.ServerStream carrying a context.
type streamWithCtx struct {
	grpc.ServerStream
	ctx context.Context
}

func (s streamWithCtx) Context() context.Context { return s.ctx }

func TestSecretStreamInterceptor(t *testing.T) {
	intc := SecretStreamInterceptor("tok")
	ran := false
	handler := func(any, grpc.ServerStream) error { ran = true; return nil }

	if err := intc(nil, streamWithCtx{ctx: ctxWithMD(authmeta.SecretKey, "tok")}, &grpc.StreamServerInfo{}, handler); err != nil {
		t.Fatalf("valid secret rejected: %v", err)
	}
	if !ran {
		t.Fatal("stream handler should have run")
	}

	ran = false
	err := intc(nil, streamWithCtx{ctx: ctxWithMD(authmeta.SecretKey, "nope")}, &grpc.StreamServerInfo{}, handler)
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
