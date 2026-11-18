// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// KOR-07: "running" is the node's report, not the orchestrator's wish. A task
// the manager still wants running (desired-state=running) but whose container
// is starting or has failed is not a target: counting it made a one-replica
// service "ambiguous" during a restart and aimed exec at a dead container.
func TestRunningTasksAreRunningNotMerelyDesired(t *testing.T) {
	for _, state := range []swarm.TaskState{
		swarm.TaskStateNew, swarm.TaskStatePending, swarm.TaskStateStarting,
		swarm.TaskStateFailed, swarm.TaskStateRejected, swarm.TaskStateComplete,
	} {
		t.Run(string(state), func(t *testing.T) {
			f := newFake()
			dead := task("t-dead", "svc-web", "node-a", 1, "cDEAD")
			dead.Status.State = state // still desired-state=running
			live := task("t-live", "svc-web", "node-b", 2, "cLIVE")
			f.tasks = []swarm.Task{dead, live}
			r := New(f, AddrHostname)

			// Resolving the service: only the live task is a candidate, so the
			// service is not ambiguous.
			ep, err := r.Resolve(context.Background(), Request{Target: "web"})
			if err != nil {
				t.Fatalf("resolve web with one dead and one live task: %v", err)
			}
			if ep.ContainerID != "cLIVE" {
				t.Errorf("resolved %q, want the running container cLIVE", ep.ContainerID)
			}

			// The same for the listing, with and without a service.
			for _, svcName := range []string{"web", ""} {
				cands, err := r.Candidates(context.Background(), svcName)
				if err != nil {
					t.Fatal(err)
				}
				if len(cands) != 1 || cands[0].ContainerID != "cLIVE" {
					t.Errorf("Candidates(%q) = %+v, want only cLIVE", svcName, cands)
				}
			}

			// A container-ID lookup does not find the dead one either.
			if _, err := r.Resolve(context.Background(), Request{Target: "cDEAD-not-a-prefix"}); err == nil {
				t.Errorf("an unknown container resolved")
			}
		})
	}
}
