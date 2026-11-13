// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agenterr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/config"
)

func TestAuthOf(t *testing.T) {
	if AuthOf(config.Config{AgentSecret: "s"}) != AuthSecret {
		t.Error("an inline secret is shared-secret mode")
	}
	if AuthOf(config.Config{AgentSecretFile: "/f"}) != AuthSecret {
		t.Error("a secret file is shared-secret mode")
	}
	if AuthOf(config.Config{CA: "/ca", Cert: "/c", Key: "/k"}) != AuthMTLS {
		t.Error("no secret is mTLS mode")
	}
}

func TestExplain(t *testing.T) {
	deployed := func(v bool) func(context.Context) bool {
		return func(context.Context) bool { return v }
	}
	unauthSecret := status.Error(codes.Unauthenticated, "invalid agent secret")
	unauthMissing := status.Error(codes.Unauthenticated, "missing agent secret")
	unauthIdentity := status.Error(codes.Unauthenticated, "client identity unavailable: no verified client certificate chain")
	unavailable := status.Error(codes.Unavailable, "connection refused")
	dialErr := errors.New("cannot reach agent on n1:9443 (mTLS/connection error: refused)")

	for _, tc := range []struct {
		name string
		err  error
		opts Options
		want error  // an explanation errors.Is must find; nil = none
		text string // exact message, when given
	}{
		{name: "too old", err: status.Error(codes.Unimplemented, "unknown method Prune"), want: ErrAgentTooOld, text: ErrAgentTooOld.Error()},
		{name: "secret mode, wrong secret", err: unauthSecret, opts: Options{Auth: AuthSecret}, want: ErrSecretRejected, text: ErrSecretRejected.Error()},
		{name: "secret mode, any Unauthenticated", err: unauthIdentity, opts: Options{Auth: AuthSecret}, want: ErrSecretRejected},
		{name: "mTLS, agent wants a secret", err: unauthMissing, opts: Options{Auth: AuthMTLS}, want: ErrSecretMissing, text: ErrSecretMissing.Error()},
		{name: "mTLS, certificate identity", err: unauthIdentity, opts: Options{Auth: AuthMTLS}, want: ErrCertRejected},
		{name: "auth unknown stays readable", err: unauthSecret, text: "Unauthenticated: invalid agent secret"},
		{name: "unavailable, no agent", err: unavailable, opts: Options{AgentDeployed: deployed(false)}, want: ErrNoAgent, text: ErrNoAgent.Error()},
		{name: "unavailable, agent deployed", err: unavailable, opts: Options{AgentDeployed: deployed(true)}, text: "Unavailable: connection refused"},
		{name: "unavailable, nobody to ask", err: unavailable, text: "Unavailable: connection refused"},
		{name: "dial error, background load", err: dialErr, opts: Options{AgentDeployed: deployed(false)}, text: dialErr.Error()},
		{name: "dial error, session, no agent", err: dialErr, opts: Options{AgentDeployed: deployed(false), Session: true}, want: ErrNoAgent},
		{name: "dial error, session, agent deployed", err: dialErr, opts: Options{AgentDeployed: deployed(true), Session: true}, text: dialErr.Error()},
		{name: "other status reads as Code: message", err: status.Error(codes.FailedPrecondition, "volume v is in use"), text: "FailedPrecondition: volume v is in use"},
		{name: "wrapped status keeps its text", err: fmt.Errorf("open logs stream: %w", status.Error(codes.NotFound, "x")), text: "open logs stream: rpc error: code = NotFound desc = x"},
		{name: "plain error untouched", err: errors.New("plain"), text: "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Explain(context.Background(), tc.err, tc.opts)
			if tc.want != nil && !errors.Is(got, tc.want) {
				t.Fatalf("got %q, want it to be %q", got, tc.want)
			}
			if tc.text != "" && got.Error() != tc.text {
				t.Fatalf("message = %q, want %q", got.Error(), tc.text)
			}
			if !errors.Is(got, tc.err) {
				t.Fatal("the explanation must wrap the original")
			}
			if status.Code(got) != status.Code(tc.err) {
				t.Fatalf("status.Code = %v, want %v — the code must survive", status.Code(got), status.Code(tc.err))
			}
		})
	}
}

// The bug this package fixes: every Unauthenticated said "check your secret",
// including to an mTLS deployment that has none.
func TestExplain_MTLSNeverMentionsTheSecretForACertificateProblem(t *testing.T) {
	for _, msg := range []string{
		"client identity unavailable: client certificate has empty common name",
		"client identity unavailable: no verified client certificate chain",
	} {
		got := Explain(context.Background(), status.Error(codes.Unauthenticated, msg), Options{Auth: AuthMTLS})
		if strings.Contains(got.Error(), "secret") {
			t.Errorf("mTLS client told about a secret: %v", got)
		}
		if !strings.Contains(got.Error(), msg) {
			t.Errorf("the agent's own reason is lost: %v", got)
		}
	}
}

func TestExplain_IsIdempotent(t *testing.T) {
	once := Explain(context.Background(), status.Error(codes.Unimplemented, "x"), Options{})
	if twice := Explain(context.Background(), once, Options{}); twice != once {
		t.Fatalf("a second pass rewrote it: %v", twice)
	}
	readable := Readable(status.Error(codes.NotFound, "x"))
	if again := Explain(context.Background(), readable, Options{}); again.Error() != "NotFound: x" {
		t.Fatalf("a readable error was re-prefixed: %q", again)
	}
	if Explain(context.Background(), nil, Options{}) != nil {
		t.Fatal("nil must stay nil")
	}
}

// The readable message must not cost the status. This used to build the text
// with fmt.Errorf("%s: %s", …) and no %w, so every downstream status.Code saw
// Unknown — the too-old check stopped matching, and so did the hint that
// explains a rejected shared secret. It was only caught against a real cluster
// running older agents, because the unit tests all called the classifier
// directly. (Moved here from session.WrapStatus.)
func TestReadable_KeepsTheCodeInspectable(t *testing.T) {
	orig := status.Error(codes.Unauthenticated, "invalid or missing agent secret")
	wrapped := Readable(orig)

	if got := wrapped.Error(); got != "Unauthenticated: invalid or missing agent secret" {
		t.Errorf("message = %q", got)
	}
	if got := status.Code(wrapped); got != codes.Unauthenticated {
		t.Errorf("status.Code = %v, want Unauthenticated — the code must survive wrapping", got)
	}
	outer := fmt.Errorf("open logs stream: %w", wrapped)
	if got := status.Code(outer); got != codes.Unauthenticated {
		t.Errorf("status.Code through an outer wrap = %v, want Unauthenticated", got)
	}
}

// A non-status error passes through untouched, so nothing gains a misleading
// "Unknown:" prefix.
func TestReadable_LeavesPlainErrorsAlone(t *testing.T) {
	plain := errors.New("dial tcp: connection refused")
	if got := Readable(plain); got != plain {
		t.Errorf("plain error was rewritten: %v", got)
	}
}
