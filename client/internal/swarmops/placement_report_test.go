// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// Characterization of buildPlaceReport's whole output for the branches the
// scenario tests in placement_test.go leave out.

func TestBuildPlaceReport_GlobalRowsAndUnevaluated(t *testing.T) {
	nodes := []swarm.Node{
		mkNode("n3", "c", "worker", "active", "ready", false, nil),
		mkNode("n1", "a", "worker", "active", "ready", false, nil),
		mkNode("n2", "b", "worker", "active", "ready", false, nil),
	}
	tasks := []swarm.Task{
		task("n1", 0, swarm.TaskStateRunning, swarm.TaskStateRunning, ""),
		task("n2", 0, swarm.TaskStateShutdown, swarm.TaskStateRejected, "no such image"),
		task("n2", 0, swarm.TaskStateShutdown, swarm.TaskStateRejected, "no such image"),
	}
	svc := globalSvc("agent", "unknown.zone==x", "unknown.az==1", "unknown.zone==x")
	rep := buildPlaceReport(svc, nodes, tasks)
	if rep.Mode != "global" || !rep.Global || rep.Running != 1 || rep.Desired != 3 {
		t.Errorf("mode=%q global=%v running=%d desired=%d", rep.Mode, rep.Global, rep.Running, rep.Desired)
	}
	want := []placeRow{
		{"a", true, "running"},
		{"b", false, taskDetail(tasks[1]) + " (2 attempts)"},
		{"c", false, "eligible but no task scheduled"},
	}
	if !reflect.DeepEqual(rep.Rows, want) {
		t.Errorf("rows =\n%+v\nwant\n%+v", rep.Rows, want)
	}
	if w := []string{"unknown.az==1", "unknown.zone==x"}; !reflect.DeepEqual(rep.Unevaluated, w) {
		t.Errorf("unevaluated = %q, want %q", rep.Unevaluated, w)
	}
	if len(rep.Tasks) != 3 {
		t.Errorf("tasks = %+v", rep.Tasks)
	}
}

func TestBuildPlaceReport_ReplicatedRows(t *testing.T) {
	nodes := []swarm.Node{mkNode("n1", "node-1", "worker", "active", "ready", false, nil)}
	tasks := []swarm.Task{
		task("n1", 3, swarm.TaskStateRunning, swarm.TaskStateFailed, "exit 1"),
		task("n1", 1, swarm.TaskStateRunning, swarm.TaskStateRunning, ""),
		task("n1", 2, swarm.TaskStateShutdown, swarm.TaskStateFailed, "old"), // retired: not surfaced
		task("gone", 2, swarm.TaskStateRunning, swarm.TaskStatePending, "no suitable node"),
	}
	rep := buildPlaceReport(replicatedSvc("api", 3), nodes, tasks)
	if rep.Mode != "replicated" || rep.Global || rep.Running != 1 || rep.Desired != 3 {
		t.Errorf("mode=%q running=%d desired=%d", rep.Mode, rep.Running, rep.Desired)
	}
	want := []placeRow{{"—", false, taskDetail(tasks[3])}, {"node-1", false, taskDetail(tasks[0])}}
	if !reflect.DeepEqual(rep.Rows, want) {
		t.Errorf("rows =\n%+v\nwant\n%+v", rep.Rows, want)
	}
	if rep.Unevaluated != nil {
		t.Errorf("unevaluated = %q", rep.Unevaluated)
	}
}

func TestBuildPlaceReport_ReplicatedNotYetScheduled(t *testing.T) {
	rep := buildPlaceReport(replicatedSvc("api", 3), nil, []swarm.Task{
		task("n1", 1, swarm.TaskStateRunning, swarm.TaskStateRunning, ""),
	})
	if want := []placeRow{{"—", false, "2 task(s) not yet scheduled"}}; !reflect.DeepEqual(rep.Rows, want) {
		t.Errorf("rows = %+v", rep.Rows)
	}
	// Replicated without a count (and nothing running): no desired, no rows.
	var s swarm.Service
	s.Spec.Mode.Replicated = &swarm.ReplicatedService{}
	if rep := buildPlaceReport(s, nil, nil); rep.Desired != 0 || rep.Rows != nil || len(rep.Tasks) != 0 {
		t.Errorf("no replicas = %+v", rep)
	}
}

// mkNode builds a swarm node for the placement tests. clusterread has its own
// copy for its own tests; a shared one would tie this package to that one.
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
