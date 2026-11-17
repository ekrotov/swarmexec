// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logstream

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmfake"
)

// webSwarm is a one-replica service "web" whose slot-1 task is in the given
// state with container c1.
func webSwarm(state swarm.TaskState) *swarmfake.Swarm {
	svc := swarm.Service{ID: "s-web"}
	svc.Spec.Name = "web"
	one := uint64(1)
	svc.Spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &one}
	task := swarm.Task{ID: "t1", ServiceID: "s-web", Slot: 1, NodeID: "n1", DesiredState: swarm.TaskStateRunning}
	task.Status.State = state
	task.Status.Timestamp = time.Now().Add(-time.Hour)
	task.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "c1"}
	node := swarm.Node{ID: "n1"}
	node.Description.Hostname = "node-a"
	node.Status.State = swarm.NodeStateReady
	return &swarmfake.Swarm{Services: []swarm.Service{svc}, Tasks: []swarm.Task{task}, Nodes: []swarm.Node{node}}
}

// A container whose stream ended while the manager still lists its task as
// running must not be re-read in a tight loop, and never from the start: the
// reconnect pauses and asks only for what came after the last stream (KOR-02).
func TestFollowSameContainerResumesWithSinceAndPauses(t *testing.T) {
	var out bytes.Buffer
	run, agent := testRunSwarm(t, Params{Follow: true, Tail: 10, ReconnectDelay: 100 * time.Millisecond},
		webSwarm(swarm.TaskStateRunning), &out, io.Discard)

	ctx, cancel := context.WithTimeout(context.Background(), 550*time.Millisecond)
	defer cancel()
	if err := Follow(ctx, run, resolve.FollowTarget{Service: "web", Slot: 1}, ep("c1"), nil); err != nil {
		t.Fatal(err)
	}

	calls := agent.Docker.LogCalls()
	if len(calls) < 2 {
		t.Fatalf("got %d ContainerLogs calls, want a reconnect", len(calls))
	}
	if len(calls) > 8 { // ~5 expected at one per delay; the bug made >1000 per second
		t.Errorf("got %d ContainerLogs calls in 550ms, want a paused loop", len(calls))
	}
	if calls[0].Tail != "10" {
		t.Errorf("first call tail = %q, want 10", calls[0].Tail)
	}
	for i, o := range calls[1:] {
		if o.Since == "" {
			t.Errorf("reconnect %d asked for the whole log (tail %q, no since)", i+1, o.Tail)
		}
	}
}

// Only a task the node reports as running counts as a successor (KOR-02).
func TestSuccessorIgnoresTasksNotYetRunning(t *testing.T) {
	run, _ := testRunSwarm(t, Params{}, webSwarm(swarm.TaskStateStarting), io.Discard, io.Discard)

	if _, ok, err := run.Resolver.Successor(context.Background(), resolve.FollowTarget{Service: "web", Slot: 1}); err != nil || ok {
		t.Fatalf("Successor = ok %v, err %v; want not found for a starting task", ok, err)
	}
}
