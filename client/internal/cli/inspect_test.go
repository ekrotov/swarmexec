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
		for _, c := range l.Children { // collapsible net rows keep DNS/addr here
			b.WriteString(c)
			b.WriteByte('\n')
		}
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

	lines := formatServiceInspect(s, map[string]string{"netid1": "frontend-net"}, map[string]bool{"netid1": true})
	joined := joinInspLines(lines)

	for _, want := range []string{"frontend-net", "tier=frontend", "assets -> /data", "db-pw", "nginx:1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("formatted output missing %q:\n%s", want, joined)
		}
	}
	// The encrypted network's inspNet row must be marked so the view can show a lock.
	for _, l := range lines {
		if l.Kind == inspNet && l.Net == "frontend-net" && !l.Encrypted {
			t.Errorf("network row not marked encrypted: %+v", l)
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

func TestServiceSpecDiff(t *testing.T) {
	var svc swarm.Service
	prev := swarm.ServiceSpec{}
	prev.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:1", Env: []string{"LOG=info", "KEEP=1"}}
	prev.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:2", Env: []string{"LOG=debug", "KEEP=1"}}
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}, {Target: "netid2"}}
	svc.PreviousSpec = &prev

	lines, hasPrev := serviceSpecDiff(svc, map[string]string{"netid1": "frontend", "netid2": "monitoring"})
	if !hasPrev {
		t.Fatal("hasPrev should be true when PreviousSpec is set")
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"- image: nginx:1", "+ image: nginx:2", "- env: LOG=info", "+ env: LOG=debug", "+ network: monitoring"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diff missing %q:\n%s", want, joined)
		}
	}
	// Unchanged fields must be omitted.
	for _, notWant := range []string{"KEEP=1", "network: frontend"} {
		if strings.Contains(joined, notWant) {
			t.Errorf("diff should omit unchanged %q:\n%s", notWant, joined)
		}
	}
	// No previous spec → nothing to diff.
	svc.PreviousSpec = nil
	if _, ok := serviceSpecDiff(svc, nil); ok {
		t.Error("hasPrev should be false when PreviousSpec is nil")
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
	task.ServiceID = "svc1"
	task.Spec.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:1"}

	// Owning service carries the DNS name + an alias the task inherits.
	var owning swarm.Service
	owning.Spec.Name = "web"
	owning.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1", Aliases: []string{"frontend"}}}

	lines := formatTaskInspect(task, &owning, map[string]string{"netid1": "frontend-net"}, map[string]bool{"netid1": true}, map[string]string{"node1": "host-a"})
	joined := joinInspLines(lines)

	// Net name in the collapsible header; DNS names + alias + addr in its children.
	for _, want := range []string{"frontend-net", "10.0.1.5/24", "running", "cabc123", "web", "tasks.web", "frontend"} {
		if !strings.Contains(joined, want) {
			t.Errorf("task inspect missing %q:\n%s", want, joined)
		}
	}
	// The network row is a collapsible inspNet entry with a DNS-name count.
	var netLine *inspLine
	for i := range lines {
		if lines[i].Kind == inspNet && lines[i].Net == "frontend-net" {
			netLine = &lines[i]
		}
	}
	if netLine == nil {
		t.Fatalf("no collapsible net row for frontend-net")
	}
	if netLine.Count != 3 { // web, tasks.web, frontend
		t.Errorf("dns-name count = %d, want 3", netLine.Count)
	}
	if i, j := headerIndex(lines, "NETWORKS"), headerIndex(lines, "STATE"); !(i >= 0 && i < j) {
		t.Errorf("NETWORKS should precede STATE (net=%d state=%d)", i, j)
	}
}
