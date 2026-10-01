// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"io"
	"testing"

	"swarmexec/internal/deploy"
)

// This is the test A1 was missing: the command line `swarmexec init` writes,
// parsed by the agent that has to run it.
//
// The two halves live in packages that cannot import each other — Go's internal
// rule makes client↔agent imports unconstructible, which is a feature — so the
// only place this round trip can happen is here, over the shared vocabulary.
// Rename a deployed flag on either side and this goes red: the agent's flag set
// no longer has what deploy.AgentArgs writes.
func TestDeployedArgsParseAndMeanWhatTheySay(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		args := deploy.AgentArgs(deploy.AgentOptions{Port: 9443, AllowLegacySecret: legacy})
		c, err := Parse(args, io.Discard)
		if err != nil {
			t.Fatalf("the agent rejects what init deploys (legacy=%v): %v\nargs: %v", legacy, err, args)
		}
		if c.Port != 9443 || c.ListenAddr != ":9443" {
			t.Errorf("port = %d, listen = %q", c.Port, c.ListenAddr)
		}
		if !c.SelfSigned {
			t.Error("the deployed agent must be self-signed")
		}
		if c.AgentSecretFile != deploy.SecretPath {
			t.Errorf("secret file = %q, want the mounted secret %q", c.AgentSecretFile, deploy.SecretPath)
		}
		if c.DockerHost != deploy.SocketURL {
			t.Errorf("docker host = %q, want %q", c.DockerHost, deploy.SocketURL)
		}
		if c.DrainTimeout != deploy.DrainTimeout {
			t.Errorf("drain timeout = %v, want %v", c.DrainTimeout, deploy.DrainTimeout)
		}
		if c.LogFormat != "json" || c.AuditDest != "stdout" {
			t.Errorf("log format = %q, audit dest = %q", c.LogFormat, c.AuditDest)
		}
		if c.AllowLegacySecret != legacy {
			t.Errorf("allow-legacy-secret = %v, want %v", c.AllowLegacySecret, legacy)
		}
	}
}

// A port other than the default has to reach the listener, because that is the
// port `init` publishes and the client dials.
func TestDeployedPortReachesTheListenAddress(t *testing.T) {
	c, err := Parse(deploy.AgentArgs(deploy.AgentOptions{Port: 9500}), io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.ListenAddr != ":9500" {
		t.Errorf("listen = %q, want :9500", c.ListenAddr)
	}
}

// The agent's own default is the one the client writes out, so a deployed spec
// and a bare `swarmexec-agent` behave the same where the contract is silent.
func TestAgentDefaultsAgreeWithTheDeployedValues(t *testing.T) {
	c, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Port != deploy.DefaultPort {
		t.Errorf("default port = %d, want %d", c.Port, deploy.DefaultPort)
	}
	if c.DockerHost != deploy.SocketURL {
		t.Errorf("default docker host = %q, want %q", c.DockerHost, deploy.SocketURL)
	}
	if c.DrainTimeout != deploy.DrainTimeout {
		t.Errorf("default drain timeout = %v, want %v (deploy writes it out explicitly)", c.DrainTimeout, deploy.DrainTimeout)
	}
}

// The metrics port init writes has to become the listen address the agent's
// metrics server binds.
func TestDeployedMetricsPortReachesTheMetricsListener(t *testing.T) {
	c, err := Parse(deploy.AgentArgs(deploy.AgentOptions{Port: 9443, MetricsPort: 9464}), io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.MetricsAddr != ":9464" {
		t.Errorf("metrics addr = %q, want :9464", c.MetricsAddr)
	}
}

func TestDeployedPolicyFlagReachesTheConfig(t *testing.T) {
	c, err := Parse(deploy.AgentArgs(deploy.AgentOptions{Port: 9443, Policy: true}), io.Discard)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.PolicyFile != deploy.PolicyPath {
		t.Errorf("policy file = %q, want %q", c.PolicyFile, deploy.PolicyPath)
	}
}
