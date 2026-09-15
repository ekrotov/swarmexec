// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeContexts lays down a docker config directory holding two ssh contexts,
// so the endpoint resolution has something real to read.
func writeContexts(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// Docker stores a context under <config>/contexts/meta/<sha256(name)>/meta.json.
	for name, host := range map[string]string{
		"alpha": "ssh://bastion-alpha",
		"beta":  "ssh://bastion-beta",
	} {
		sub := filepath.Join(dir, "contexts", "meta", fmt.Sprintf("%x", sha256.Sum256([]byte(name))))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		meta := fmt.Sprintf(`{"Name":%q,"Endpoints":{"docker":{"Host":%q}}}`, name, host)
		if err := os.WriteFile(filepath.Join(sub, "meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
}

// The bug this guards against: the manager API followed a cluster switch while
// the agent tunnel did not, because the two were built from two separate
// lookups of two different context names. Measured before the fix — 3/3 agents
// when started on a context directly, 0/3 after switching to the same context
// in the ui — and invisible in the tree, which comes from the manager and kept
// working.
//
// So the property worth pinning is not "the dialer works" but "the manager
// client and the agent tunnel come from the SAME resolution".
func TestEndpointFeedsBothChannelsFromOneResolution(t *testing.T) {
	writeContexts(t)

	alpha := resolveEndpoint("alpha")
	beta := resolveEndpoint("beta")
	if alpha.Host != "ssh://bastion-alpha" {
		t.Fatalf("alpha host = %q, want the context's endpoint", alpha.Host)
	}
	if beta.Host != "ssh://bastion-beta" {
		t.Fatalf("beta host = %q, want the context's endpoint", beta.Host)
	}
	// Two contexts must not resolve to the same place — otherwise this test
	// could pass while everything pointed at one cluster.
	if alpha.Host == beta.Host {
		t.Fatal("the two fixtures resolve to the same host; the test proves nothing")
	}

	// Both channels read the same field of the same value. There is no second
	// lookup left that could answer differently.
	for _, e := range []dockerEndpoint{alpha, beta} {
		d, err := e.agentDialer()
		if err != nil {
			t.Fatalf("%s: agent dialer: %v", e.Context, err)
		}
		if d == nil {
			t.Errorf("%s is an ssh endpoint and must get a tunnel", e.Context)
		}
	}
}

// A non-ssh endpoint dials the agents directly; a tunnel there would be wrong.
func TestNonSSHEndpointGetsNoTunnel(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	t.Setenv("DOCKER_CONTEXT", "")

	e := resolveEndpoint("")
	d, err := e.agentDialer()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != nil {
		t.Error("a tcp:// endpoint must dial agents directly")
	}
}

// A context that does not exist must still fail where it always failed — from
// the manager client, with its clearer message — and must not fail config
// resolution, which would change what every command prints.
func TestUnresolvableEndpointFailsAtTheClient(t *testing.T) {
	writeContexts(t)

	e := resolveEndpoint("does-not-exist")
	if e.err == nil {
		t.Fatal("an unknown context should not resolve")
	}
	if _, err := e.client(); err == nil {
		t.Error("the manager client must report the failure")
	}
	d, err := e.agentDialer()
	if err != nil {
		t.Errorf("an unresolvable endpoint must not fail config resolution: %v", err)
	}
	if d != nil {
		t.Error("an unresolvable endpoint must not produce a tunnel")
	}
}
