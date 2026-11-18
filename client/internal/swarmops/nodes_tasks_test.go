// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
)

// PRF-03: the node task counts come from the tasks the swarm wants running
// (not from the history), and a task list that cannot be read is "unknown",
// not "0".
func TestListNodesTaskCountFiltersHistoryAndFlagsFailure(t *testing.T) {
	running := mkTask("n1", swarm.TaskStateRunning)
	running.ID, running.DesiredState = "t-run", swarm.TaskStateRunning
	dead := mkTask("n1", swarm.TaskStateRunning)
	dead.ID, dead.DesiredState = "t-old", swarm.TaskStateShutdown
	f := &swarmfake.Swarm{
		Nodes: []swarm.Node{mkNode("n1", "node-1", "worker", "active", "ready", false, nil)},
		Tasks: []swarm.Task{running, dead},
	}
	dcli := fakeDocker(t, f)

	got, err := ListNodes(context.Background(), dcli)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListNodes = %+v, %v", got, err)
	}
	if !got[0].TasksKnown || got[0].Tasks != 1 || got[0].TaskCount() != "1" {
		t.Errorf("node = tasks %d known %v, want the one desired-running task", got[0].Tasks, got[0].TasksKnown)
	}

	f.Fail = map[string]bool{"GET /tasks": true}
	got, err = ListNodes(context.Background(), dcli)
	if err != nil || len(got) != 1 {
		t.Fatalf("ListNodes with a failing task list = %+v, %v (nodes must still list)", got, err)
	}
	if got[0].TasksKnown || got[0].TaskCount() != "?" {
		t.Errorf("failed task list shows %q (known=%v), want \"?\"", got[0].TaskCount(), got[0].TasksKnown)
	}
}
