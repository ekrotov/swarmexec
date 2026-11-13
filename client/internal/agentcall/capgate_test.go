// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// An agent that predates an RPC is asked once; after that the answer is
// remembered for that node and that RPC only — not for the node's other
// RPCs, and not for the other nodes.
func TestUnimplementedBenchesThatRPCOnThatNode(t *testing.T) {
	fc := newFakeCluster(t, "old", "new")
	fc.agents["old"].setUnimplemented("Stats", "ListImages", "ReclaimStats", "ListVolumes", "WatchContainerEvents")
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background()
	old, cur := fc.nodes[0], fc.nodes[1]

	for i := 0; i < 5; i++ {
		_, byNode := ac.Usage(ctx, fc.nodes, testTimeout)
		if _, ok := byNode["new"]; !ok {
			t.Fatal("the upgraded node must keep reporting")
		}
		_, err := ac.Images(ctx, old, testTimeout)
		if status.Code(err) != codes.Unimplemented || !errors.Is(err, agenterr.ErrAgentTooOld) {
			t.Fatalf("benched answer = %v, want the agent's own Unimplemented, explained", err)
		}
		_, _ = ac.ReclaimStats(ctx, old, false, testTimeout)
		_, errs := ac.Volumes(ctx, fc.nodes, testTimeout)
		if status.Code(errs["old"]) != codes.Unimplemented || errs["new"] != nil {
			t.Fatalf("volume errors = %v", errs)
		}
		_ = ac.ContainerEvents(ctx, resolve.Endpoint{DialHost: "old"}, testTimeout, func(*pb.ContainerEvent) {})
	}
	for _, rpc := range []string{"Stats", "ListImages", "ReclaimStats", "ListVolumes", "WatchContainerEvents"} {
		if got := fc.agents["old"].count(rpc, 0); got != 1 {
			t.Errorf("old agent asked %s %d times in five rounds, want once", rpc, got)
		}
	}
	if got := fc.agents["new"].count("Stats", 0); got != 5 {
		t.Errorf("new agent asked for stats %d times, want every round", got)
	}
	// The old node's other RPCs are untouched by the bench.
	version(t, ac, old)
	if _, err := ac.Images(ctx, cur, testTimeout); err != nil {
		t.Errorf("images on the new node: %v", err)
	}
}

// An explicit operator action is never gated: Unimplemented is fast, and the
// operator should hear the agent's own answer every time.
func TestOperatorActionsAreNotGated(t *testing.T) {
	fc := newFakeCluster(t, "old")
	fc.agents["old"].setUnimplemented("PruneImages", "ReclaimStats")
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := ac.PruneImages(ctx, fc.nodes[0], false, testTimeout); status.Code(err) != codes.Unimplemented {
			t.Fatalf("prune = %v", err)
		}
		// measure_now is the operator asking: sent despite the bench.
		if _, err := ac.ReclaimStats(ctx, fc.nodes[0], true, testTimeout); status.Code(err) != codes.Unimplemented {
			t.Fatalf("reclaim = %v", err)
		}
	}
	if got := fc.agents["old"].count("PruneImages", 0); got != 3 {
		t.Errorf("prune sent %d times, want 3", got)
	}
	if got := fc.agents["old"].count("ReclaimStats", 0); got != 3 {
		t.Errorf("measure-now reclaim sent %d times, want 3", got)
	}
}

// A poll that fails for an ordinary reason — the node is down — is benched
// briefly; an operator's listing is not benched for that at all.
func TestOrdinaryFailuresBenchOnlyThePolls(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	fc.setDown("node-a", true)
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background()
	n := fc.nodes[0]

	for i := 0; i < 3; i++ {
		ac.Usage(ctx, fc.nodes, testTimeout)
		_, _ = ac.Images(ctx, n, testTimeout)
	}
	if got := fc.dials.Load(); got != 2 {
		t.Errorf("%d dials for three rounds of stats + images on a down node, want one each", got)
	}
	before := fc.dials.Load()
	for i := 0; i < 3; i++ {
		_, _ = ac.Volumes(ctx, fc.nodes, testTimeout)
	}
	if got := fc.dials.Load() - before; got != 3 {
		t.Errorf("%d dials for three volume listings, want every one tried", got)
	}

	// The bench is short and time-based...
	g := ac.gate
	g.mu.Lock()
	until := g.bench[gateKey{"node-a", rpcStats}].until
	g.mu.Unlock()
	if d := time.Until(until); d <= 0 || d > pollRetryAfter {
		t.Errorf("stats benched for %v, want at most %v", d, pollRetryAfter)
	}
	g.mu.Lock()
	for k, b := range g.bench {
		b.until = time.Now().Add(-time.Second)
		g.bench[k] = b
	}
	g.mu.Unlock()
	fc.setDown("node-a", false)
	// ...and once it is over the node is asked again, and answers.
	if _, byNode := ac.Usage(ctx, fc.nodes, testTimeout); len(byNode) != 1 {
		t.Error("a node back from the bench should report again")
	}
}

// Redeploying the agent is how it learns a new RPC. A new connection to the
// node — the old one died with the old agent — forgets what was benched.
func TestANewConnectionForgetsTheBench(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	fc.agents["node-a"].setUnimplemented("Stats")
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background()
	n := fc.nodes[0]

	if _, err := ac.Stats(ctx, n, testTimeout); status.Code(err) != codes.Unimplemented {
		t.Fatalf("stats on an old agent = %v", err)
	}
	conn, _ := ac.Conn(ctx, "node-a")
	cc := conn.cc
	conn.Close()

	// `init --force`: the agent restarts as a newer version.
	fc.stop("node-a")
	waitState(t, cc, connectivity.Ready, false)
	fc.start(t, "node-a") // knows Stats

	// Still benched: nothing has reconnected yet.
	if _, err := ac.Stats(ctx, n, testTimeout); status.Code(err) != codes.Unimplemented {
		t.Fatalf("before reconnecting, stats = %v; want the remembered answer", err)
	}
	version(t, ac, n) // any call reconnects
	if _, err := ac.Stats(ctx, n, testTimeout); err != nil {
		t.Errorf("after the new connection, stats = %v; want it asked again", err)
	}
}

// A call cut short by its caller (a cluster switch, a closed view) says
// nothing about the agent and benches nothing.
func TestCancelledCallsAreNotRecorded(t *testing.T) {
	g := newCapGate()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.record(ctx, "h", rpcStats, context.Canceled)
	if err := g.check("h", rpcStats); err != nil {
		t.Errorf("a cancelled call was benched: %v", err)
	}
	g.record(context.Background(), "h", rpcStats, status.Error(codes.Unavailable, "down"))
	if g.check("h", rpcStats) == nil {
		t.Error("a failed poll should be benched")
	}
	g.record(context.Background(), "h", rpcStats, nil)
	if err := g.check("h", rpcStats); err != nil {
		t.Errorf("a success should clear the bench: %v", err)
	}
	g.record(context.Background(), "h", rpcListVolumes, status.Error(codes.Unavailable, "down"))
	if err := g.check("h", rpcListVolumes); err != nil {
		t.Errorf("an ordinary failure of a non-poll RPC was benched: %v", err)
	}
}

// The container-events side channel streams through ContainerEvents.
func TestContainerEventsStream(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	ac := New(fc.cfg)
	defer ac.Close()
	var got []string
	err := ac.ContainerEvents(context.Background(), resolve.Endpoint{DialHost: "node-a", ContainerID: "abc"}, testTimeout,
		func(ev *pb.ContainerEvent) { got = append(got, ev.GetAction()) })
	if err != nil || len(got) != 2 || got[0] != "start" || got[1] != "die" {
		t.Errorf("events = %v, err = %v", got, err)
	}
	fc.setDown("node-a", true)
	if err := ac.ContainerEvents(context.Background(), resolve.Endpoint{DialHost: "node-x"}, testTimeout, func(*pb.ContainerEvent) {}); err == nil {
		t.Error("events from an unreachable node should fail")
	}
}
