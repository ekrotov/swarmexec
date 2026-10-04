// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/resolve"
)

func mkNode(id, host, role, avail, state string, leader bool, labels map[string]string) swarm.Node {
	var n swarm.Node
	n.ID = id
	n.Description.Hostname = host
	n.Spec.Role = swarm.NodeRole(role)
	n.Spec.Availability = swarm.NodeAvailability(avail)
	n.Spec.Annotations.Labels = labels
	n.Status.State = swarm.NodeState(state)
	if leader {
		n.ManagerStatus = &swarm.ManagerStatus{Leader: true}
	}
	return n
}

func mkTask(nodeID string, state swarm.TaskState) swarm.Task {
	var t swarm.Task
	t.NodeID = nodeID
	t.Status.State = state
	return t
}

func TestBuildNodeInfos(t *testing.T) {
	nodes := []swarm.Node{
		mkNode("w1", "worker-b", "worker", "active", "ready", false, map[string]string{"zone": "eu"}),
		mkNode("m1", "mgr-a", "manager", "active", "ready", true, nil),
		mkNode("w2", "worker-a", "worker", "drain", "down", false, nil),
	}
	tasks := []swarm.Task{
		mkTask("w1", swarm.TaskStateRunning),
		mkTask("w1", swarm.TaskStateRunning),
		mkTask("w1", swarm.TaskStateShutdown), // not counted
		mkTask("m1", swarm.TaskStateRunning),
	}
	got := buildNodeInfos(nodes, tasks)

	// Managers first, then hostname order: mgr-a, worker-a, worker-b.
	if len(got) != 3 || got[0].Hostname != "mgr-a" || got[1].Hostname != "worker-a" || got[2].Hostname != "worker-b" {
		t.Fatalf("ordering wrong: %+v", []string{got[0].Hostname, got[1].Hostname, got[2].Hostname})
	}
	if !got[0].Leader || got[0].Role != "manager" {
		t.Errorf("manager/leader not set: %+v", got[0])
	}
	if got[2].Tasks != 2 { // worker-b (w1) has 2 running, 1 shutdown
		t.Errorf("worker-b tasks = %d, want 2", got[2].Tasks)
	}
	if got[0].Tasks != 1 {
		t.Errorf("mgr-a tasks = %d, want 1", got[0].Tasks)
	}
	if len(got[2].Labels) != 1 || got[2].Labels["zone"] != "eu" {
		t.Errorf("worker-b labels = %v", got[2].Labels)
	}
}

func TestVolumeCountsByNode(t *testing.T) {
	vols := []swarmVolume{
		{Name: "a", Nodes: []resolve.Node{{Name: "n1"}, {Name: "n2"}}},
		{Name: "b", Nodes: []resolve.Node{{Name: "n1"}}},
	}
	got := volumeCountsByNode(vols)
	if got["n1"] != 2 || got["n2"] != 1 {
		t.Errorf("counts = %v, want n1=2 n2=1", got)
	}
}
