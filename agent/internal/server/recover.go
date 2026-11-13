// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gRPC-Go does not recover panics in handlers: one unexpected nil in a Docker
// response would take the whole agent down, and with it every shell and
// forward open on the node. The recovery interceptors turn a handler panic into
// codes.Internal for that one RPC and an error log with the stack.
//
// They only see the handler's own goroutine. A panic in a goroutine a handler
// starts (exec's copy loops, for one) still ends the process.

// RecoveryUnaryInterceptor recovers a panicking unary handler.
func RecoveryUnaryInterceptor(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = recovered(log, info.FullMethod, r)
			}
		}()
		return handler(ctx, req)
	}
}

// RecoveryStreamInterceptor recovers a panicking streaming handler.
func RecoveryStreamInterceptor(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = recovered(log, info.FullMethod, r)
			}
		}()
		return handler(srv, ss)
	}
}

// recovered logs a recovered panic and returns the status the client gets. The
// panic value stays in the log: it may carry internals the caller has no
// business seeing.
func recovered(log *slog.Logger, method string, r any) error {
	log.Error("handler panicked; recovered", "method", method, "panic", r, "stack", string(debug.Stack()))
	return status.Error(codes.Internal, "internal error")
}
