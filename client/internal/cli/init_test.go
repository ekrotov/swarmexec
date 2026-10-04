// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"registry.logle.io/internal-tools/x/agent:latest": "registry.logle.io",
		"localhost:5000/agent:1.0":                        "localhost:5000",
		"nginx":                                           "docker.io",
		"library/nginx:latest":                            "docker.io",
		"gcr.io/proj/img":                                 "gcr.io",
	}
	for image, want := range cases {
		if got := registryHost(image); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestGenerateSecret(t *testing.T) {
	a, b := generateSecret(), generateSecret()
	if a == "" || a == b {
		t.Fatalf("expected two distinct non-empty secrets, got %q and %q", a, b)
	}
	if len(a) < 32 {
		t.Errorf("secret too short: %d chars", len(a))
	}
}

func TestAgentServiceSpec(t *testing.T) {
	f := &initFlags{image: "reg/agent:1", serviceName: "swarmexec_agent", port: 9443}
	spec := agentServiceSpec(f, "sec-123")

	if spec.Mode.Global == nil {
		t.Error("service must be global")
	}
	if spec.Annotations.Labels[agentRoleLabel] != agentRoleValue {
		t.Error("missing role label")
	}
	cs := spec.TaskTemplate.ContainerSpec
	if cs.Image != "reg/agent:1" {
		t.Errorf("image = %q", cs.Image)
	}
	args := strings.Join(cs.Args, " ")
	for _, want := range []string{"-port=9443", "-self-signed", "/run/secrets/" + agentSecretName} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if len(cs.Secrets) != 1 || cs.Secrets[0].SecretID != "sec-123" {
		t.Errorf("secret ref wrong: %+v", cs.Secrets)
	}
	if len(cs.Mounts) != 1 || cs.Mounts[0].Source != "/var/run/docker.sock" {
		t.Errorf("socket mount wrong: %+v", cs.Mounts)
	}
	if spec.UpdateConfig.Order != swarm.UpdateOrderStopFirst {
		t.Errorf("update order = %q, want stop-first", spec.UpdateConfig.Order)
	}
	p := spec.EndpointSpec.Ports
	if len(p) != 1 || p[0].PublishedPort != 9443 || p[0].PublishMode != swarm.PortConfigPublishModeHost {
		t.Errorf("port config wrong: %+v", p)
	}
}

// An upgraded client talking to an agent that predates connection-bound
// authentication gets Unauthenticated with "invalid or missing agent secret" —
// true from the agent's side, and misleading from the operator's, because their
// secret is correct. The hint has to name the upgrade first.
func TestEnrichAgentError_ExplainsARejectedSecret(t *testing.T) {
	err := enrichAgentError(context.Background(), nil,
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

// The deprecation lever for the raw shared secret has to be reachable from the
// path operators actually use. It was not: agentServiceSpec had a hard-wired
// argument list, so the only way to set it was editing the service by hand —
// and the next `init --force` overwrites the whole spec and drops it again.
func TestAgentServiceSpec_LegacySecretSwitch(t *testing.T) {
	args := func(allowLegacy bool) []string {
		f := &initFlags{image: "reg/agent:1", serviceName: "swarmexec_agent", port: 9443, allowLegacy: allowLegacy}
		return agentServiceSpec(f, "sec-123").TaskTemplate.ContainerSpec.Args
	}

	// Default: nothing emitted, so a re-roll produces the same command earlier
	// versions did and no client is locked out by surprise.
	for _, a := range args(true) {
		if strings.Contains(a, "allow-legacy-secret") {
			t.Errorf("default must not emit the flag, got %q", a)
		}
	}

	var found bool
	for _, a := range args(false) {
		if a == "-allow-legacy-secret=false" {
			found = true
		}
	}
	if !found {
		t.Errorf("--allow-legacy-secret=false must reach the agent command, got %v", args(false))
	}
}

// With --metrics-port the agent both listens and publishes: a metrics flag
// without a published port is an endpoint nothing outside the container can
// reach, which is exactly how the metrics went unused before.
func TestAgentServiceSpec_MetricsPort(t *testing.T) {
	f := &initFlags{image: "reg/agent:1", serviceName: "swarmexec_agent", port: 9443, allowLegacy: true}
	if ports := agentServiceSpec(f, "s").EndpointSpec.Ports; len(ports) != 1 {
		t.Fatalf("no metrics: want only the agent port, got %+v", ports)
	}

	f.metricsPort = 9464
	spec := agentServiceSpec(f, "s")
	ports := spec.EndpointSpec.Ports
	if len(ports) != 2 || ports[1].PublishedPort != 9464 || ports[1].TargetPort != 9464 || ports[1].PublishMode != swarm.PortConfigPublishModeHost {
		t.Errorf("metrics port not published in host mode: %+v", ports)
	}
	args := strings.Join(spec.TaskTemplate.ContainerSpec.Args, " ")
	if !strings.Contains(args, "-metrics-addr=:9464") {
		t.Errorf("agent not told to listen: %s", args)
	}
}
