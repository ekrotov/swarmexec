// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"testing"

	"swarmexec/internal/pb"
)

// A context that only bounded the dial left the RPC itself unbounded: an agent
// that accepted the connection and then stalled would hang the caller for good.
// gRPC carries the client's deadline to the server, so the fake agent can see
// whether one was set.
func TestNodeCallsCarryADeadline(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background() // no deadline of its own
	n := fc.nodes[0]

	if _, _, _, err := ac.Version(ctx, n, testTimeout); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.listNodeContainers(ctx, n, testTimeout); err != nil {
		t.Fatal(err)
	}
	if err := ac.removeNodeVolume(ctx, n, "data", false, testTimeout); err != nil {
		t.Fatal(err)
	}
	if err := ac.createNodeVolume(ctx, n, &pb.CreateVolumeRequest{Name: "data"}, testTimeout); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.listNodeVolumesNow(ctx, n, false, testTimeout); err != nil {
		t.Fatal(err)
	}

	a := fc.agents["node-a"]
	a.failMu.Lock()
	defer a.failMu.Unlock()
	for _, rpc := range []string{"Version", "ListContainers", "RemoveVolume", "CreateVolume", "ListVolumes"} {
		if a.byRPC[rpc] != 1 {
			t.Errorf("%s: served %d times, want 1", rpc, a.byRPC[rpc])
		} else if !a.bounded[rpc] {
			t.Errorf("%s arrived without a deadline", rpc)
		}
	}
}
