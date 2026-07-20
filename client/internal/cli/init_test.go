// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"
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
