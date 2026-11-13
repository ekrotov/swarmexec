// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

func TestRecoveryUnaryInterceptor(t *testing.T) {
	buf := &syncBuffer{}
	ic := RecoveryUnaryInterceptor(slog.New(slog.NewTextHandler(buf, nil)))
	info := &grpc.UnaryServerInfo{FullMethod: "/swarmexec.Agent/ListVolumes"}

	resp, err := ic(context.Background(), nil, info, func(context.Context, any) (any, error) {
		var m map[string]int
		m["boom"] = 1 // nil-map write: a runtime panic like the ones this guards against
		return "unreachable", nil
	})
	if statusCode(err) != codes.Internal || resp != nil {
		t.Fatalf("got (%v, %v), want (nil, Internal)", resp, err)
	}
	if strings.Contains(err.Error(), "nil map") {
		t.Errorf("panic detail leaked to the client: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ListVolumes") || !strings.Contains(out, "nil map") || !strings.Contains(out, "recover_test.go") {
		t.Errorf("log should name the method, the panic and its stack:\n%s", out)
	}

	// A handler that does not panic passes through untouched.
	resp, err = ic(context.Background(), nil, info, func(context.Context, any) (any, error) { return "ok", nil })
	if resp != "ok" || err != nil {
		t.Errorf("got (%v, %v), want (ok, nil)", resp, err)
	}
}

func TestRecoveryStreamInterceptor(t *testing.T) {
	buf := &syncBuffer{}
	ic := RecoveryStreamInterceptor(slog.New(slog.NewTextHandler(buf, nil)))
	info := &grpc.StreamServerInfo{FullMethod: "/swarmexec.Agent/Logs"}

	err := ic(nil, nil, info, func(any, grpc.ServerStream) error { panic("unexpected nil") })
	if statusCode(err) != codes.Internal {
		t.Fatalf("err = %v, want Internal", err)
	}
	if out := buf.String(); !strings.Contains(out, "Logs") || !strings.Contains(out, "unexpected nil") {
		t.Errorf("log should name the method and the panic:\n%s", out)
	}
}
