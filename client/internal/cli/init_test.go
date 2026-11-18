// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/endpoint"
)

// An upgraded client talking to an agent that predates connection-bound
// authentication gets Unauthenticated with "invalid or missing agent secret" —
// true from the agent's side, and misleading from the operator's, because their
// secret is correct. The hint has to name the upgrade first.
func TestExplainSession_ExplainsARejectedSecret(t *testing.T) {
	err := explainSession(context.Background(), config.Config{AgentSecret: "s"}, nil,
		status.Error(codes.Unauthenticated, "invalid or missing agent secret"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"swarmexec init --force", "legacy_secret", "does not match"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("hint does not mention %q: %v", want, err)
		}
	}
}

// The same Unauthenticated, seen by a client that authenticates with a client
// certificate and has no secret at all: telling that operator to check "the
// shared secret" sent them hunting for a credential they do not use (review
// A-05). The CLI paths (exec, logs, port-forward) must name the certificate.
func TestExplainSession_CAModeDoesNotBlameTheSecret(t *testing.T) {
	ca := config.Config{CA: "/ca.pem", Cert: "/c.pem", Key: "/k.pem"}
	err := explainSession(context.Background(), ca, nil,
		status.Error(codes.Unauthenticated, "client identity unavailable: client certificate has empty common name"))
	msg := err.Error()
	if strings.Contains(msg, "secret") {
		t.Errorf("an mTLS client was told about a secret: %v", msg)
	}
	for _, want := range []string{"certificate", "empty common name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q: %v", want, msg)
		}
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("the code must survive: %v", status.Code(err))
	}
}

// The deprecation lever for the raw shared secret has to be reachable from the
// path operators actually use. It was not: agentServiceSpec had a hard-wired
// argument list, so the only way to set it was editing the service by hand —
// and the next `init --force` overwrites the whole spec and drops it again.
// The bug: with several contexts and DOCKER_HOST set, init and down took the
// active context instead — twice reaching a production cluster from a shell
// that pointed DOCKER_HOST at a local test daemon. Pinned end to end: the
// context resolveContext names must dial DOCKER_HOST.
func TestResolveContextHonoursDockerHost(t *testing.T) {
	writeContexts(t)
	cfg := `{"currentContext":"alpha"}`
	if err := os.WriteFile(filepath.Join(os.Getenv("DOCKER_CONFIG"), "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	const local = "unix:///tmp/test-docker.sock"
	t.Setenv("DOCKER_HOST", local)

	c, err := resolveContext(io.Discard, &globalFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != local {
		t.Errorf("resolved host %q, want DOCKER_HOST %q", c.Host, local)
	}
	if got := endpoint.Resolve(c.Name).Host; got != local {
		t.Errorf("context %q dials %q, want DOCKER_HOST %q", c.Name, got, local)
	}

	// An explicit --context still wins over DOCKER_HOST, as with docker.
	c, err = resolveContext(io.Discard, &globalFlags{dockerContext: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "beta" || endpoint.Resolve(c.Name).Host != "ssh://bastion-beta" {
		t.Errorf("--context beta resolved to %q (%s)", c.Name, endpoint.Resolve(c.Name).Host)
	}
}
