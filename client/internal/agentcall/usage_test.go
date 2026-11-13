// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A cluster whose agents predate the Stats RPC must not cost a timeout per node
// on every refresh — measured at a full 10s per cycle before the gate existed.
func TestStatsGateBenchesFailingNodes(t *testing.T) {
	g := NewStatsGate()
	if !g.allowed("node-a") {
		t.Fatal("an unseen node should be probed")
	}

	// An agent too old to know the RPC will keep saying so until it is
	// redeployed, so it waits far longer than a node that was merely away.
	g.fail("node-a", status.Error(codes.Unimplemented, "unknown method Stats"))
	g.fail("node-b", context.DeadlineExceeded)
	if g.allowed("node-a") || g.allowed("node-b") {
		t.Error("a node that just failed must not be probed again immediately")
	}

	g.mu.Lock()
	unsupported, transient := g.skip["node-a"], g.skip["node-b"]
	g.mu.Unlock()
	if !unsupported.After(transient) {
		t.Errorf("an unsupported agent should wait longer than an unreachable one: %v vs %v", unsupported, transient)
	}

	// A node that starts answering again is taken off the bench.
	g.ok("node-b")
	if !g.allowed("node-b") {
		t.Error("a node that answered should be probed again")
	}

	// The bench is time-based, not permanent.
	g.mu.Lock()
	g.skip["node-a"] = time.Now().Add(-time.Second)
	g.mu.Unlock()
	if !g.allowed("node-a") {
		t.Error("an expired bench should let the node be probed again")
	}

	// A nil gate is the no-op case, so callers need no special handling.
	var none *StatsGate
	if !none.allowed("x") {
		t.Error("a nil gate should allow everything")
	}
	none.fail("x", nil)
	none.ok("x")
}
