// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

func globalSvc(name string, constraints ...string) swarm.Service {
	var s swarm.Service
	s.Spec.Name = name
	s.Spec.Mode.Global = &swarm.GlobalService{}
	if len(constraints) > 0 {
		s.Spec.TaskTemplate.Placement = &swarm.Placement{Constraints: constraints}
	}
	return s
}

func replicatedSvc(name string, replicas uint64) swarm.Service {
	var s swarm.Service
	s.Spec.Name = name
	r := replicas
	s.Spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &r}
	return s
}

func task(nodeID string, slot int, desired, state swarm.TaskState, errMsg string) swarm.Task {
	var t swarm.Task
	t.NodeID = nodeID
	t.Slot = slot
	t.DesiredState = desired
	t.Status.State = state
	t.Status.Err = errMsg
	return t
}

func rowFor(rep placeReport, node string) (placeRow, bool) {
	for _, r := range rep.Rows {
		if r.Node == node {
			return r, true
		}
	}
	return placeRow{}, false
}

func TestBuildPlaceReport_GlobalDrainedNode(t *testing.T) {
	nodes := []swarm.Node{
		mkNode("n1", "node-1", "worker", "active", "ready", false, nil),
		mkNode("n2", "node-2", "worker", "active", "ready", false, nil),
		mkNode("n3", "node-3", "worker", "drain", "ready", false, nil),
	}
	tasks := []swarm.Task{
		task("n1", 0, swarm.TaskStateRunning, swarm.TaskStateRunning, ""),
		task("n2", 0, swarm.TaskStateRunning, swarm.TaskStateRunning, ""),
	}
	rep := buildPlaceReport(globalSvc("web"), nodes, tasks)

	if rep.Running != 2 || rep.Desired != 2 {
		t.Errorf("running/desired = %d/%d, want 2/2 (drained node excluded)", rep.Running, rep.Desired)
	}
	r, ok := rowFor(rep, "node-3")
	if !ok || r.OK || !strings.Contains(r.Detail, "availability is drain") {
		t.Errorf("node-3 row = %+v, want excluded/drain", r)
	}
	if r, _ := rowFor(rep, "node-1"); !r.OK {
		t.Errorf("node-1 should be running: %+v", r)
	}
}

func TestBuildPlaceReport_GlobalConstraint(t *testing.T) {
	nodes := []swarm.Node{
		mkNode("m1", "mgr-1", "manager", "active", "ready", true, nil),
		mkNode("w1", "wrk-1", "worker", "active", "ready", false, nil),
	}
	tasks := []swarm.Task{task("m1", 0, swarm.TaskStateRunning, swarm.TaskStateRunning, "")}
	rep := buildPlaceReport(globalSvc("mon", "node.role==manager"), nodes, tasks)

	if rep.Desired != 1 || rep.Running != 1 {
		t.Errorf("running/desired = %d/%d, want 1/1", rep.Running, rep.Desired)
	}
	r, ok := rowFor(rep, "wrk-1")
	if !ok || r.OK || !strings.Contains(r.Detail, "constraint not satisfied: node.role==manager") {
		t.Errorf("wrk-1 row = %+v, want excluded/constraint", r)
	}
}

// A global service whose node has only a retired (desired-shutdown) rejected
// task must surface that task's error — not the vague "eligible but no task
// scheduled" — and list the task in the docker-service-ps view.
func TestBuildPlaceReport_GlobalRetiredFailedTask(t *testing.T) {
	nodes := []swarm.Node{
		mkNode("n1", "docker2v2", "worker", "active", "ready", false, nil),
		mkNode("n2", "docker3v2", "worker", "active", "ready", false, nil),
		mkNode("n3", "generic-node-1", "worker", "active", "ready", false, nil),
	}
	tasks := []swarm.Task{
		task("n1", 0, swarm.TaskStateRunning, swarm.TaskStateRunning, ""),
		task("n2", 0, swarm.TaskStateRunning, swarm.TaskStateRunning, ""),
		// Repeatedly rejected on n3, so the scheduler retired it to shutdown.
		task("n3", 0, swarm.TaskStateShutdown, swarm.TaskStateRejected, "No such image: cr.fluentbit.io/fluent/fluent-bit:latest"),
		task("n3", 0, swarm.TaskStateShutdown, swarm.TaskStateRejected, "No such image: cr.fluentbit.io/fluent/fluent-bit:latest"),
	}
	rep := buildPlaceReport(globalSvc("fb_app"), nodes, tasks)

	r, ok := rowFor(rep, "generic-node-1")
	if !ok || r.OK {
		t.Fatalf("generic-node-1 row = %+v, want a non-running row", r)
	}
	if strings.Contains(r.Detail, "no task scheduled") {
		t.Errorf("generic-node-1 still shows the unhelpful message: %q", r.Detail)
	}
	if !strings.Contains(r.Detail, "No such image") {
		t.Errorf("generic-node-1 detail should carry the task error, got %q", r.Detail)
	}
	if !strings.Contains(r.Detail, "2 attempts") {
		t.Errorf("generic-node-1 detail should note the retry count, got %q", r.Detail)
	}
	// The docker-service-ps view lists every task, including the rejected ones.
	if len(rep.Tasks) != len(tasks) {
		t.Fatalf("Tasks = %d, want %d", len(rep.Tasks), len(tasks))
	}
	var sawRejected bool
	for _, tp := range rep.Tasks {
		if tp.Current == string(swarm.TaskStateRejected) && strings.Contains(tp.Err, "No such image") {
			sawRejected = true
			if tp.Node != "generic-node-1" {
				t.Errorf("rejected task node = %q, want generic-node-1", tp.Node)
			}
		}
	}
	if !sawRejected {
		t.Error("docker-service-ps view is missing the rejected task with its image error")
	}
}

func TestBuildPlaceReport_ReplicatedPending(t *testing.T) {
	nodes := []swarm.Node{mkNode("n1", "node-1", "worker", "active", "ready", false, nil)}
	tasks := []swarm.Task{
		task("n1", 1, swarm.TaskStateRunning, swarm.TaskStateRunning, ""),
		task("", 2, swarm.TaskStateRunning, swarm.TaskStatePending, "no suitable node (insufficient resources)"),
	}
	rep := buildPlaceReport(replicatedSvc("api", 2), nodes, tasks)

	if rep.Running != 1 || rep.Desired != 2 {
		t.Errorf("running/desired = %d/%d, want 1/2", rep.Running, rep.Desired)
	}
	// The pending task is surfaced with the scheduler's message; the running one is not.
	if len(rep.Rows) != 1 || !strings.Contains(rep.Rows[0].Detail, "insufficient resources") {
		t.Errorf("rows = %+v, want one pending row with the reason", rep.Rows)
	}
}
