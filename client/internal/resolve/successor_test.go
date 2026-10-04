// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
)

// A replacement task in the same slot (new container id, possibly a new node) is
// what Successor returns, so a followed log stream reconnects to the new
// container.
func TestSuccessor_ReplacementInSameSlot(t *testing.T) {
	f := newFake()
	// The old task is gone (desired-state != running); only the replacement,
	// rescheduled onto node-b, is running in slot 1.
	f.tasks = []swarm.Task{task("t-new", "svc-web", "node-b", 1, "cNEW")}
	r := New(f, AddrHostname)

	c, ok, err := r.Successor(context.Background(), FollowTarget{Service: "web", Slot: 1})
	if err != nil || !ok {
		t.Fatalf("want a successor, got ok=%v err=%v", ok, err)
	}
	if c.ContainerID != "cNEW" || c.NodeName != "host-b" {
		t.Errorf("successor = %s on %s, want cNEW on host-b", c.ContainerID, c.NodeName)
	}
}

// When a slot briefly holds both the old and the new task during a rolling
// update, the newest (smallest uptime) wins.
func TestSuccessor_NewestWinsInSlot(t *testing.T) {
	f := newFake()
	old := task("t-old", "svc-web", "node-a", 1, "cOLD")
	old.Status.Timestamp = time.Now().Add(-30 * time.Minute) // older
	fresh := task("t-new", "svc-web", "node-b", 1, "cNEW")
	fresh.Status.Timestamp = time.Now().Add(-10 * time.Second) // newer
	f.tasks = []swarm.Task{old, fresh}
	r := New(f, AddrHostname)

	c, ok, err := r.Successor(context.Background(), FollowTarget{Service: "web", Slot: 1})
	if err != nil || !ok {
		t.Fatalf("want a successor, got ok=%v err=%v", ok, err)
	}
	if c.ContainerID != "cNEW" {
		t.Errorf("successor = %s, want the newer cNEW", c.ContainerID)
	}
}

// A global service (slot 0) is followed by node.
func TestSuccessor_GlobalMatchesByNode(t *testing.T) {
	f := newFake()
	f.tasks = []swarm.Task{
		task("t-a", "svc-web", "node-a", 0, "cA"),
		task("t-b", "svc-web", "node-b", 0, "cB"),
	}
	r := New(f, AddrHostname)

	c, ok, err := r.Successor(context.Background(), FollowTarget{Service: "web", NodeID: "node-b"})
	if err != nil || !ok {
		t.Fatalf("want a successor, got ok=%v err=%v", ok, err)
	}
	if c.ContainerID != "cB" {
		t.Errorf("successor = %s, want cB (node-b)", c.ContainerID)
	}
}

// No matching running task yet (replacement still scheduling) is a clean
// not-found, not an error — the caller keeps waiting.
func TestSuccessor_PendingWhenNoRunningTask(t *testing.T) {
	f := newFake()
	f.tasks = nil // service exists, no running task
	r := New(f, AddrHostname)

	_, ok, err := r.Successor(context.Background(), FollowTarget{Service: "web", Slot: 1})
	if ok || err != nil {
		t.Fatalf("want ok=false err=nil (pending), got ok=%v err=%v", ok, err)
	}
}

// A removed service reports ErrTargetGone so the caller stops following.
func TestSuccessor_TargetGoneWhenServiceMissing(t *testing.T) {
	f := newFake()
	f.services = nil
	r := New(f, AddrHostname)

	_, ok, err := r.Successor(context.Background(), FollowTarget{Service: "web", Slot: 1})
	if ok || !errors.Is(err, ErrTargetGone) {
		t.Fatalf("want ErrTargetGone, got ok=%v err=%v", ok, err)
	}
}
