// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"errors"
	"testing"

	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/fanout"
)

// Every node answers for itself, in node order; an agent that predates the RPC
// is an error that reads as too old — "unknown" — and is asked only once.
func TestNodesNetworkEndpoints(t *testing.T) {
	fc := newFakeCluster(t, "old", "new")
	fc.agents["old"].setUnimplemented("NetworkEndpoints")
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background()

	for range 3 {
		res := ac.NodesNetworkEndpoints(ctx, fc.nodes, []string{"net-a"}, testTimeout)
		if len(res) != 2 || res[0].Node.Name != "old" || res[1].Node.Name != "new" {
			t.Fatalf("results = %+v, want one per node in order", res)
		}
		if !agenterr.TooOld(res[0].Err) || !errors.Is(res[0].Err, agenterr.ErrAgentTooOld) || res[0].Resp != nil {
			t.Errorf("old node = %+v, want too old and no response", res[0])
		}
		if res[1].Err != nil || len(res[1].Resp.GetNetworks()) != 1 || res[1].Resp.GetNetworks()[0].GetId() != "net-a" {
			t.Errorf("new node = %+v, want its answer for net-a", res[1])
		}
	}
	if got := fc.agents["old"].count("NetworkEndpoints", 0); got != 1 {
		t.Errorf("old agent asked %d times, want once (gated)", got)
	}
	if got := fc.agents["new"].count("NetworkEndpoints", 0); got != 3 {
		t.Errorf("new agent asked %d times, want every round", got)
	}

	// A fan-out that never starts leaves every node skipped, not silently empty.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	for _, r := range ac.NodesNetworkEndpoints(cctx, fc.nodes, nil, testTimeout) {
		if !errors.Is(r.Err, fanout.ErrSkipped) || r.Resp != nil {
			t.Errorf("%s: %+v, want ErrSkipped from a cancelled fan-out", r.Node.Name, r)
		}
	}
}
