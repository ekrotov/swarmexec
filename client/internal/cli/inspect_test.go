// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

func TestFormatServiceInspect_OrderAndContent(t *testing.T) {
	var s swarm.Service
	s.ID = "svc123"
	s.Spec.Name = "web"
	s.Spec.Labels = map[string]string{"tier": "frontend"}
	s.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{
		Image:   "nginx:1",
		Secrets: []*swarm.SecretReference{{SecretName: "db-pw"}},
		Mounts:  []mount.Mount{{Type: mount.TypeVolume, Source: "assets", Target: "/data"}},
	}

	out := formatServiceInspect(s, map[string]string{"netid1": "frontend-net"})

	for _, want := range []string{"frontend-net", "tier=frontend", "assets -> /data", "db-pw", "nginx:1"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatted output missing %q:\n%s", want, out)
		}
	}
	// Operator-first ordering: networks, labels, volumes, secrets — all before META.
	iNet := strings.Index(out, "NETWORKS")
	iLbl := strings.Index(out, "LABELS")
	iVol := strings.Index(out, "VOLUMES")
	iSec := strings.Index(out, "SECRETS")
	iMeta := strings.Index(out, "META")
	if !(iNet >= 0 && iNet < iLbl && iLbl < iVol && iVol < iSec && iSec < iMeta) {
		t.Errorf("section order wrong: net=%d lbl=%d vol=%d sec=%d meta=%d", iNet, iLbl, iVol, iSec, iMeta)
	}
}

func TestFormatTaskInspect_NetworksAndState(t *testing.T) {
	var task swarm.Task
	task.ID = "task123"
	task.Slot = 2
	task.DesiredState = swarm.TaskStateRunning
	task.Status.State = swarm.TaskStateRunning
	task.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "cabc123"}
	task.NetworksAttachments = []swarm.NetworkAttachment{
		{Network: swarm.Network{ID: "netid1"}, Addresses: []string{"10.0.1.5/24"}},
	}
	task.Spec.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:1"}

	out := formatTaskInspect(task, map[string]string{"netid1": "frontend-net"}, map[string]string{"node1": "host-a"})

	for _, want := range []string{"frontend-net", "10.0.1.5/24", "running", "cabc123"} {
		if !strings.Contains(out, want) {
			t.Errorf("task inspect missing %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "NETWORKS"), strings.Index(out, "STATE"); !(i >= 0 && i < j) {
		t.Errorf("NETWORKS should precede STATE (net=%d state=%d)", i, j)
	}
}
