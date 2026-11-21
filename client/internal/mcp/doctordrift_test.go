// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/agentdeploy"
	"swarmexec/internal/deploy"
)

// A7: the MCP `doctor` tool reports spec drift, not only reachability.
//
// Without this, an assistant reading a cluster through MCP is told "healthy"
// about the exact fault that looks healthy from the outside — the CLI would
// say otherwise about the same cluster, and the two must not disagree.
func TestMCPDoctorReportsSpecDrift(t *testing.T) {
	isolateDockerConfig(t)
	fx := newMCPFixture(t, "none", true)

	spec := agentdeploy.Spec(agentdeploy.Options{
		Image:       "logleio/swarmexec-agent:1.2.3",
		ServiceName: deploy.ServiceName,
		Port:        deploy.DefaultPort,
	}, "sec-1")
	cs := spec.TaskTemplate.ContainerSpec
	var kept []mount.Mount
	for _, m := range cs.Mounts {
		if m.Source != deploy.HostSysfsPath {
			kept = append(kept, m)
		}
	}
	cs.Mounts = kept
	fx.swarm.Services = append(fx.swarm.Services, swarm.Service{ID: "s-agent", Spec: spec})

	out := callTool(t, mcpSession(t, fx), mcpCall{"doctor", map[string]any{}})

	var got mcpDoctor
	if err := json.Unmarshal([]byte(out.text), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out.text)
	}
	if got.Healthy {
		t.Errorf("healthy=true on a drifted cluster:\n%s", out.text)
	}
	if len(got.SpecDrift) != 1 || !strings.Contains(got.SpecDrift[0].Field, deploy.HostSysfsPath) {
		t.Errorf("spec_drift = %+v", got.SpecDrift)
	}
	if !strings.Contains(got.DriftHint, "init --force") {
		t.Errorf("spec_drift_hint = %q", got.DriftHint)
	}
}

// An agent service that matches says nothing, and the tool stays healthy.
func TestMCPDoctorQuietWhenTheSpecMatches(t *testing.T) {
	isolateDockerConfig(t)
	fx := newMCPFixture(t, "none", true)
	spec := agentdeploy.Spec(agentdeploy.Options{
		Image:       "logleio/swarmexec-agent:1.2.3",
		ServiceName: deploy.ServiceName,
		Port:        deploy.DefaultPort,
	}, "sec-1")
	fx.swarm.Services = append(fx.swarm.Services, swarm.Service{ID: "s-agent", Spec: spec})

	var got mcpDoctor
	if err := json.Unmarshal([]byte(callTool(t, mcpSession(t, fx), mcpCall{"doctor", map[string]any{}}).text), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.SpecDrift) != 0 || got.DriftHint != "" {
		t.Errorf("drift reported on a matching spec: %+v / %q", got.SpecDrift, got.DriftHint)
	}
}
