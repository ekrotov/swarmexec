// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package agenterr is the one place an error from an agent is turned into what
// the operator reads. It replaces three wrappers that each knew part of the
// mapping — session.WrapStatus (readable "Code: message"), cli's wrapGRPC
// (too-old agent) and enrichAgentError (no agent deployed, rejected secret) —
// and it fixes what none of them could know: which credential the operator
// actually uses. An Unauthenticated status means "check your secret" only to a
// client that has one; to an mTLS client it is about its certificate.
//
// The status codes it reads are CONTRACT.md §3.8. Every error it returns
// still wraps the original, so errors.Is and status.Code on the result keep
// answering for the cause.
//
// The package is deliberately small and free of cli: agentcall (review A-01)
// explains every per-node RPC error through it.
package agenterr

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/config"
)

// Auth is how this client authenticates to its agents.
type Auth int

const (
	// AuthUnknown is the zero value: the caller did not say. Unauthenticated
	// is then shown readable but without a hint, rather than with one that
	// may point at the wrong credential.
	AuthUnknown Auth = iota
	// AuthSecret is shared-secret mode (self-signed agents, §2.1).
	AuthSecret
	// AuthMTLS is mutual TLS with client certificates and no shared secret.
	AuthMTLS
)

// AuthOf reports the auth mode a client configuration selects. The rule is
// the one dial.Dial applies: a configured secret is sent; otherwise only the
// client certificate speaks for the operator.
func AuthOf(cfg config.Config) Auth {
	if cfg.SecretMode() {
		return AuthSecret
	}
	return AuthMTLS
}

// Options says what Explain may assume and ask.
type Options struct {
	// Auth selects the Unauthenticated explanation.
	Auth Auth

	// AgentDeployed, when set, answers "is there an agent in this swarm at
	// all" — asked only for a failure that could mean there is none. Nil
	// skips the question (no manager client at hand).
	AgentDeployed func(context.Context) bool

	// Session marks the error as the end of a whole agent session (CLI exec,
	// logs, port-forward), where ANY failure — a dial error, not only an
	// Unavailable status — is checked against AgentDeployed. A background
	// TUI load sets it false: there only an Unavailable status means the
	// agent may be missing, and a manager-API error must not be misreported
	// as "no agent".
	Session bool
}

// The explanations. Callers may test for them with errors.Is.
var (
	// ErrNoAgent is shown when an agent-needing command finds no agent deployed.
	ErrNoAgent = errors.New("no swarmexec agent found in this swarm — run `swarmexec init` to provision it")

	// ErrAgentTooOld is shown when an agent rejects an RPC with Unimplemented.
	ErrAgentTooOld = errors.New("agent is older than this client (missing RPC) — update it with `swarmexec init --force`")

	// ErrSecretRejected explains Unauthenticated to a shared-secret client. It
	// has a far more likely cause than a wrong secret since v1.17.3: this
	// client sends a proof bound to the connection instead of the secret
	// itself, and an agent predating that does not recognise it. The bare
	// "invalid or missing agent secret" the agent returns is actively
	// misleading in that case — the secret is fine.
	//
	//lint:ignore ST1005 shown verbatim to the operator; the layout is the message
	ErrSecretRejected = errors.New("agent rejected the shared secret.\n" +
		"  Most likely the agents predate connection-bound authentication (v1.17.3):\n" +
		"  update them with `swarmexec init --force`, or set `legacy_secret: true` in\n" +
		"  the client config to send the raw secret meanwhile.\n" +
		"  Otherwise the configured secret does not match the one the agents hold.")

	// ErrSecretMissing explains Unauthenticated to an mTLS client whose agent
	// runs in shared-secret mode: it asked for a secret this client does not
	// have. Telling this operator their certificate is wrong would be as
	// misleading as telling an mTLS operator to check a secret.
	//
	//lint:ignore ST1005 shown verbatim to the operator; the layout is the message
	ErrSecretMissing = errors.New("agent requires a shared secret, and this client has none configured.\n" +
		"  The agents run in shared-secret mode (`swarmexec init`); set `agent_secret_file`\n" +
		"  (or `agent_secret`) in the client config, or pass --agent-secret-file.")

	// ErrCertRejected explains Unauthenticated to an mTLS client: the TLS
	// handshake passed, but the agent could not take an identity from the
	// client certificate.
	//
	//lint:ignore ST1005 shown verbatim to the operator; the layout is the message
	ErrCertRejected = errors.New("agent did not accept this client's certificate identity.\n" +
		"  This client authenticates with mutual TLS (a client certificate):\n" +
		"  check that `cert`/`key` are the pair issued by the CA the agents trust and\n" +
		"  that the certificate has a Common Name — the agents use it as your identity.")
)

// Explain turns err into what the operator should read, or returns it
// unchanged when there is nothing better to say. It is idempotent: an error
// already explained is returned as is.
//
//   - Unimplemented → ErrAgentTooOld.
//   - Unauthenticated → the explanation for opts.Auth (secret rejected, secret
//     missing, certificate identity).
//   - Unavailable, or with opts.Session any failure, while AgentDeployed says
//     no agent is deployed → ErrNoAgent.
//   - Any other gRPC status → "Code: message" (Readable).
//   - Anything else → unchanged.
func Explain(ctx context.Context, err error, opts Options) error {
	if err == nil {
		return nil
	}
	var done explained
	if errors.As(err, &done) {
		return err
	}
	for _, known := range []error{ErrAgentTooOld, ErrNoAgent, ErrSecretRejected, ErrSecretMissing, ErrCertRejected} {
		if errors.Is(err, known) {
			return err
		}
	}

	st, isStatus := status.FromError(err)
	if isStatus {
		switch st.Code() {
		case codes.Unimplemented:
			return explained{shown: ErrAgentTooOld, cause: err}
		case codes.Unauthenticated:
			if shown := authError(opts.Auth, st.Message()); shown != nil {
				return explained{shown: shown, cause: err}
			}
		}
	}

	checkAgent := opts.Session || (isStatus && st.Code() == codes.Unavailable)
	if checkAgent && opts.AgentDeployed != nil && !opts.AgentDeployed(ctx) {
		return explained{shown: ErrNoAgent, cause: err}
	}
	return Readable(err)
}

// authError picks the Unauthenticated explanation for the client's auth mode.
// msg is the agent's own description (CONTRACT.md §3.8).
func authError(mode Auth, msg string) error {
	switch mode {
	case AuthSecret:
		return ErrSecretRejected
	case AuthMTLS:
		// An agent in shared-secret mode answers a client without one with
		// "missing agent secret"; every other Unauthenticated an mTLS client
		// can get is about its certificate.
		if strings.Contains(msg, "missing agent secret") {
			return ErrSecretMissing
		}
		return fmt.Errorf("%w\n  The agent said: %s", ErrCertRejected, msg)
	}
	return nil
}

// Readable turns a gRPC status error into "Code: message" — "Unauthenticated:
// invalid agent secret" rather than grpc's "rpc error: code = ... desc = ..."
// — while genuinely preserving the original for callers that inspect it.
//
// Only an error that IS a status is rewritten. One that merely wraps a status
// (fmt.Errorf("open logs stream: %w", st), or an already readable one) keeps
// its own text: status.FromError would hand back that whole text as the
// message, and the result would read "Code: Code: …".
//
// A plain error passes through untouched, so nothing gains a misleading
// "Unknown:" prefix.
func Readable(err error) error {
	se, ok := err.(interface{ GRPCStatus() *status.Status })
	if !ok {
		return err
	}
	st := se.GRPCStatus()
	return statusError{msg: fmt.Sprintf("%s: %s", st.Code(), st.Message()), err: err}
}

// statusError carries a human-readable message and still unwraps to the gRPC
// status, so status.FromError (which uses errors.As) keeps finding the code.
//
// The previous version of this (session.WrapStatus) once built the message
// with fmt.Errorf("%s: %s", ...), no %w, so the status was flattened into a
// plain string and every downstream check saw codes.Unknown — the too-old and
// rejected-secret explanations never matched on those paths.
type statusError struct {
	msg string
	err error
}

func (e statusError) Error() string { return e.msg }
func (e statusError) Unwrap() error { return e.err }

// explained shows one error and keeps another as its cause: errors.Is finds
// both, status.Code finds the cause's code.
type explained struct{ shown, cause error }

func (e explained) Error() string   { return e.shown.Error() }
func (e explained) Unwrap() []error { return []error{e.shown, e.cause} }
