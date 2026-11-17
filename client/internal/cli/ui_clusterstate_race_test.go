// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// gatedAgent holds its ListContainers (the second half of the volumes load's
// first pass) until released, and counts the with-size ListVolumes calls the
// sizes pass makes.
type gatedAgent struct {
	volumeAgent
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	sized   atomic.Int32
}

func (a *gatedAgent) ListContainers(ctx context.Context, r *pb.ListRequest) (*pb.ListResponse, error) {
	if a.entered != nil {
		a.once.Do(func() { close(a.entered) })
		<-a.release
	}
	return a.volumeAgent.ListContainers(ctx, r)
}

func (a *gatedAgent) ListVolumes(ctx context.Context, r *pb.ListVolumesRequest) (*pb.ListVolumesResponse, error) {
	if r.GetWithSize() {
		a.sized.Add(1)
	}
	return a.volumeAgent.ListVolumes(ctx, r)
}

// The volumes load of cluster A is mid-flight when the operator switches to B.
// The sizes pass that follows must still ask A's agents with A's pool — it
// used to read the visible cluster's pool at that point, so B's agents got a
// size scan meant for A's nodes. Deterministic: A's first pass is held at a
// gate until the switch has happened on the loop.
func TestVolumesLoadSizes_KeepsItsClusterAcrossASwitch(t *testing.T) {
	mgr := &fakeManager{nodes: []swarm.Node{readyNode("n1", "node-a")}}
	a := &gatedAgent{volumeAgent: volumeAgent{vols: []string{"shop_data"}}, entered: make(chan struct{}), release: make(chan struct{})}
	b := &gatedAgent{volumeAgent: volumeAgent{vols: []string{"other"}}}
	cfgA := startAgents(t, map[string]pb.AgentServer{"node-a": a})
	cfgB := startAgents(t, map[string]pb.AgentServer{"node-a": b})
	u := volumesUI(t, mgr, cfgA)
	poolB := agentcall.New(cfgB)
	t.Cleanup(poolB.Close)

	onLoop(t, u, func() { u.volTab().load() })
	select {
	case <-a.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("A's first pass never reached its agent")
	}
	onLoop(t, u, func() { // the switch, as activateCluster leaves it
		u.gen.Add(1)
		c := newClusterState("b", "b")
		c.dcli, c.r = mgr, resolve.New(mgr, resolve.AddrHostname)
		c.agents, c.cfg = poolB, cfgB
		c.ctx, c.cancel = context.WithCancel(context.Background())
		t.Cleanup(c.cancel)
		u.clusterState = c
		u.f = &uiFlags{connectTimeout: 3 * time.Second}
	})
	close(a.release)

	waitUntil(t, "A's sizes pass to reach A's agent", func() bool { return a.sized.Load() > 0 })
	time.Sleep(100 * time.Millisecond) // a misrouted call would be on B by now
	if n := b.sized.Load(); n != 0 {
		t.Errorf("cluster B's agent got %d size scan(s) meant for cluster A's nodes", n)
	}
}

// The poll tick reads the visible cluster's fields, so it must run on the UI
// loop. With a switch hammering the same fields from the loop, the race
// detector flags a tick body that runs anywhere else.
func TestPollTick_DoesNotRaceASwitch(t *testing.T) {
	u := treeHarness(t, map[string]http.Handler{"a": shopSwarm(), "b": shopSwarm()})
	cfg := startAgents(t, map[string]pb.AgentServer{"node-a": &volumeAgent{}, "node-b": &volumeAgent{}})
	onLoop(t, u, func() {
		for _, c := range u.clusters {
			c.cfg, c.agents = cfg, agentcall.New(cfg)
			t.Cleanup(c.agents.Close)
		}
		u.f = &uiFlags{connectTimeout: time.Second}
		u.treeTab().load()
	})
	waitForTree(t, u, "shop_db")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the timer goroutine
		defer wg.Done()
		for i := 0; i < 50; i++ {
			u.pollTick()
		}
	}()
	for i := 0; i < 50; i++ {
		toTreeCluster(t, u, []string{"b", "a"}[i%2])
	}
	wg.Wait()
	onLoop(t, u, func() {})
}
