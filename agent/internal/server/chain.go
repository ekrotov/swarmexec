// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"

	"google.golang.org/grpc"
)

// Interceptors builds the server's interceptor chains. Order, outermost first
// — each ChainXInterceptor option appends to its chain, so the order of the
// options is the order of execution:
//
//  1. recovery — first, so a panic anywhere below, authentication included,
//     costs one RPC, not every session on the node;
//  2. the shared-secret check, when a secret is configured (secretAuth != nil)
//     — before the stream gate, so a caller without the secret can neither
//     take a stream slot nor hold up a drain, and is told Unauthenticated even
//     while the agent drains or sits at its cap, as it always was;
//  3. the stream gate (streams only) — drain check, stream slot, drain
//     accounting and per-stream metrics, before the handler's first Recv.
//
// Authorization is not an interceptor: it needs the container's service, which
// only the handler resolves.
//
// It lives here, not in cmd/agent, so that the agent test harness
// (agent/agenttest) runs the very chain the binary does.
func Interceptors(log *slog.Logger, secretAuth *SecretAuth, srv *Server) []grpc.ServerOption {
	unary := []grpc.UnaryServerInterceptor{RecoveryUnaryInterceptor(log)}
	stream := []grpc.StreamServerInterceptor{RecoveryStreamInterceptor(log)}
	if secretAuth != nil {
		unary = append(unary, secretAuth.UnaryInterceptor())
		stream = append(stream, secretAuth.StreamInterceptor())
	}
	stream = append(stream, srv.StreamInterceptor())
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
	}
}
