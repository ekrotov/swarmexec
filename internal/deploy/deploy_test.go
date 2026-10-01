// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"strings"
	"testing"
)

// The deployed command line is pinned, byte for byte.
//
// Not pedantry: `swarmexec init` compares the spec it would deploy against the
// one already running, and a re-roll that shows a spurious change teaches
// operators to ignore the diff. Any edit to AgentArgs has to be a deliberate
// one, made here first.
func TestAgentArgsIsPinned(t *testing.T) {
	want := []string{
		"-port=9443",
		"-self-signed",
		"-agent-secret-file=/run/secrets/swarmexec_agent_secret",
		"-docker-host=unix:///var/run/docker.sock",
		"-drain-timeout=5s",
		"-log-format=json",
		"-audit-dest=stdout",
	}
	got := AgentArgs(AgentOptions{Port: 9443, AllowLegacySecret: true})
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("AgentArgs =\n  %v\nwant\n  %v", got, want)
	}
}

// The legacy flag appears only when turning the path OFF, so an agent deployed
// by an older client and one deployed by this one carry the same command line
// unless the operator actually asked for the change.
func TestAgentArgsOnlyWritesTheLegacyFlagWhenDisabling(t *testing.T) {
	on := strings.Join(AgentArgs(AgentOptions{Port: 9443, AllowLegacySecret: true}), " ")
	if strings.Contains(on, FlagAllowLegacySecret) {
		t.Errorf("the default must not be written out: %q", on)
	}
	off := strings.Join(AgentArgs(AgentOptions{Port: 9443, AllowLegacySecret: false}), " ")
	if !strings.Contains(off, "-allow-legacy-secret=false") {
		t.Errorf("turning it off must be explicit: %q", off)
	}
}

// The port is the one knob in the command line, and it is also published and
// dialled — it has to survive into the argv exactly.
func TestAgentArgsCarriesThePort(t *testing.T) {
	if got := AgentArgs(AgentOptions{Port: 12345})[0]; got != "-port=12345" {
		t.Errorf("port arg = %q", got)
	}
}

// The mount path and the flag value are two halves of one fact: the client
// mounts the secret under SecretName, the agent is told to read SecretPath.
func TestSecretPathIsTheMountedSecret(t *testing.T) {
	if SecretPath != "/run/secrets/"+SecretName {
		t.Errorf("SecretPath %q does not name the mounted secret %q", SecretPath, SecretName)
	}
	if SocketURL != "unix://"+SocketPath {
		t.Errorf("SocketURL %q does not address the mounted socket %q", SocketURL, SocketPath)
	}
}

// Metrics are opt-in: no port writes nothing, so a deployment without them
// keeps the command line older clients produced; a port writes one flag.
func TestAgentArgsWritesMetricsOnlyWhenAsked(t *testing.T) {
	off := strings.Join(AgentArgs(AgentOptions{Port: 9443, AllowLegacySecret: true}), " ")
	if strings.Contains(off, FlagMetricsAddr) {
		t.Errorf("metrics off must write nothing: %q", off)
	}
	on := AgentArgs(AgentOptions{Port: 9443, AllowLegacySecret: true, MetricsPort: 9464})
	if got := on[len(on)-1]; got != "-metrics-addr=:9464" {
		t.Errorf("last arg = %q, want -metrics-addr=:9464", got)
	}
}

// The policy flag and the mount path are two halves of one fact, like the
// secret's: init mounts the config at PolicyPath, the agent reads that path.
func TestAgentArgsPointsAtThePolicyOnlyWhenOneIsDeployed(t *testing.T) {
	if s := strings.Join(AgentArgs(AgentOptions{Port: 9443, AllowLegacySecret: true}), " "); strings.Contains(s, FlagPolicyFile) {
		t.Errorf("no policy must write nothing: %q", s)
	}
	a := AgentArgs(AgentOptions{Port: 9443, AllowLegacySecret: true, Policy: true})
	if got := a[len(a)-1]; got != "-policy-file="+PolicyPath {
		t.Errorf("last arg = %q", got)
	}
}
