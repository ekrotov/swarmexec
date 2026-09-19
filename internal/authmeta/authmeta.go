// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package authmeta defines the gRPC metadata keys for shared-secret
// authentication, shared by the agent (server) and cli (client) so they cannot
// drift apart.
package authmeta

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

const (
	// SecretKey carries the RAW shared secret on each RPC. Legacy: a client that
	// sends this hands the credential itself to whatever server answered the
	// connection. Superseded by BindingKey; see Bind.
	SecretKey = "x-swarmexec-secret"
	// BindingKey carries a proof of the shared secret bound to the TLS
	// connection it travels on (see Bind). Current clients send this INSTEAD of
	// SecretKey, so there is no credential on the wire to capture.
	BindingKey = "x-swarmexec-binding"
	// OperatorKey optionally carries the operator identity for audit when no
	// client certificate is presented.
	OperatorKey = "x-swarmexec-operator"
)

// bindingContext separates this use of the secret from any other, and carries a
// version so the scheme can change without an old proof being accepted under
// new rules.
const bindingContext = "swarmexec-channel-binding-v1"

// Bind returns a proof that the holder knows secret, valid only on the TLS
// connection whose server certificate is serverCertDER.
//
// Why this exists. In the documented self-signed mode the client cannot verify
// the agent's certificate — the agent generates a fresh self-signed one on every
// restart, on every node, so there is nothing stable to trust. Sending the raw
// secret over such a connection means handing it to whoever answered: an on-path
// attacker presents any certificate, the client connects, and the attacker
// captures a credential that is valid on EVERY node of the cluster — root
// equivalent on each, since the agent holds the Docker socket.
//
// A bound proof removes the thing worth stealing. The attacker receives a proof
// computed over THEIR certificate; replaying it to a real agent fails, because
// the agent computes the expected value over its own. The secret itself never
// leaves the client.
//
// What this does NOT fix: the attacker still terminates a TLS connection the
// client did not verify, so they see what that client sends them. Binding
// defeats credential theft and replay, not eavesdropping on a session the
// operator chose not to authenticate. On an untrusted network, use a CA.
func Bind(secret string, serverCertDER []byte) string {
	sum := sha256.Sum256(serverCertDER)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(bindingContext))
	mac.Write(sum[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
