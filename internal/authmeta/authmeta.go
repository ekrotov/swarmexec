// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package authmeta defines the gRPC metadata keys for shared-secret
// authentication, shared by the agent (server) and cli (client) so they cannot
// drift apart.
package authmeta

const (
	// SecretKey carries the shared secret on each RPC (self-signed agent mode).
	SecretKey = "x-swarmexec-secret"
	// OperatorKey optionally carries the operator identity for audit when no
	// client certificate is presented.
	OperatorKey = "x-swarmexec-operator"
)
