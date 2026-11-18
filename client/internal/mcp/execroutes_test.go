// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

func TestExecRefusesOtherRoutesToDocker(t *testing.T) {
	envSvc := swarm.Service{}
	envSvc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "app", Env: []string{"DOCKER_HOST=tcp://proxy:2375"}}
	if err := execRefusal(envSvc, nil); err == nil {
		t.Error("DOCKER_HOST service was allowed")
	}
	prev := swarm.Service{PreviousSpec: &envSvc.Spec}
	prev.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "app"}
	if err := execRefusal(prev, nil); err == nil {
		t.Error("DOCKER_HOST in the previous spec was allowed")
	}
	onProxyNet := swarm.Service{}
	onProxyNet.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "app"}
	onProxyNet.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "apinet"}}
	if err := execRefusal(onProxyNet, map[string]bool{"apinet": true}); err == nil {
		t.Error("service on a socket-proxy network was allowed")
	}
	if err := execRefusal(onProxyNet, map[string]bool{"other": true}); err != nil {
		t.Errorf("unrelated network refused: %v", err)
	}
}
