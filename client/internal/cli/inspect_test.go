// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

func joinInspLines(lines []inspLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func headerIndex(lines []inspLine, header string) int {
	for i, l := range lines {
		if l.Kind == inspHeader && l.Text == header {
			return i
		}
	}
	return -1
}

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

	lines := formatServiceInspect(s, map[string]string{"netid1": "frontend-net"})
	joined := joinInspLines(lines)

	for _, want := range []string{"frontend-net", "tier=frontend", "assets -> /data", "db-pw", "nginx:1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("formatted output missing %q:\n%s", want, joined)
		}
	}
	// Operator-first ordering: networks, labels, volumes, secrets — all before META.
	iNet := headerIndex(lines, "NETWORKS")
	iLbl := headerIndex(lines, "LABELS")
	iVol := headerIndex(lines, "VOLUMES / MOUNTS")
	iSec := headerIndex(lines, "SECRETS")
	iMeta := headerIndex(lines, "META")
	if !(iNet >= 0 && iNet < iLbl && iLbl < iVol && iVol < iSec && iSec < iMeta) {
		t.Errorf("section order wrong: net=%d lbl=%d vol=%d sec=%d meta=%d", iNet, iLbl, iVol, iSec, iMeta)
	}
	// The value lines must be selectable fields, the headers must not be.
	for _, l := range lines {
		if l.Kind == inspField && strings.TrimSpace(l.Text) == "" {
			t.Errorf("empty field line should not be a selectable field")
		}
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

	lines := formatTaskInspect(task, map[string]string{"netid1": "frontend-net"}, map[string]string{"node1": "host-a"})
	joined := joinInspLines(lines)

	for _, want := range []string{"frontend-net", "10.0.1.5/24", "running", "cabc123"} {
		if !strings.Contains(joined, want) {
			t.Errorf("task inspect missing %q:\n%s", want, joined)
		}
	}
	if i, j := headerIndex(lines, "NETWORKS"), headerIndex(lines, "STATE"); !(i >= 0 && i < j) {
		t.Errorf("NETWORKS should precede STATE (net=%d state=%d)", i, j)
	}
}
