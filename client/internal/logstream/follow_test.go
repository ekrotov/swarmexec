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

	"swarmexec/agent/agenttest"
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

// A follow that is stopped WHILE A STREAM IS OPEN ends cleanly. The caller
// asked to stop — Ctrl-C, a closed log view, an expired deadline — so whatever
// the stream reports on the way out is a consequence of that, not something to
// show the operator. This failed intermittently in CI: on a loaded runner the
// deadline landed inside the stream rather than between two of them, and
// Follow returned DeadlineExceeded to its caller.
//
// The container's log stream is held open here so the cancellation reliably
// lands mid-stream; letting it end on its own is the case the other tests
// already cover.
func TestFollowStoppedMidStreamEndsCleanly(t *testing.T) {
	var out bytes.Buffer
	d := agenttest.NewDocker()
	d.AddContainer(agenttest.Container{ID: "c1", Service: "web"})
	blocking := make(chan struct{})
	t.Cleanup(func() { close(blocking) })
	d.SetLogStream("c1", func() io.ReadCloser {
		// One line, then the stream stays open — a following container that
		// is simply quiet.
		return io.NopCloser(io.MultiReader(
			bytes.NewReader(agenttest.Frame(1, []byte("hello\n"))),
			blockingReader{blocking},
		))
	})
	run, _ := testRunWith(t, Params{Follow: true, Tail: 10, ReconnectDelay: 10 * time.Millisecond},
		webSwarm(swarm.TaskStateRunning), d, &out, io.Discard)

	// A DEADLINE, not a cancel: cancellation was already treated as a clean
	// end, a deadline was not, and a deadline is what CI hit.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := Follow(ctx, run, resolve.FollowTarget{Service: "web", Slot: 1}, ep("c1"), nil); err != nil {
		t.Fatalf("Follow = %v, want nil: a stopped follow is an end, not a failure", err)
	}
}

// The same for a plain Stream, which is what the `logs` command uses: a
// bounded or interrupted read must not exit non-zero.
func TestStreamStoppedMidStreamEndsCleanly(t *testing.T) {
	var out bytes.Buffer
	d := agenttest.NewDocker()
	d.AddContainer(agenttest.Container{ID: "c1", Service: "web"})
	blocking := make(chan struct{})
	t.Cleanup(func() { close(blocking) })
	d.SetLogStream("c1", func() io.ReadCloser {
		return io.NopCloser(io.MultiReader(
			bytes.NewReader(agenttest.Frame(1, []byte("hello\n"))),
			blockingReader{blocking},
		))
	})
	run, _ := testRunWith(t, Params{Follow: true, Tail: 10}, webSwarm(swarm.TaskStateRunning), d, &out, io.Discard)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := Stream(ctx, run, ep("c1")); err != nil {
		t.Fatalf("Stream = %v, want nil", err)
	}
}

// blockingReader never returns data and never ends, until its channel closes.
type blockingReader struct{ done <-chan struct{} }

func (b blockingReader) Read([]byte) (int, error) {
	<-b.done
	return 0, io.EOF
}
