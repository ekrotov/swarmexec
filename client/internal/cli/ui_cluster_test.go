// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"testing"

	"github.com/moby/moby/client"
	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
)

// treeUI is the smallest ui that can render a container tree: the widgets it
// draws into, a forward registry for the annotations, and a cluster to hold the
// data. Everything else renderContainers touches is nil-tolerant.
func treeUI(cluster string, svcs []resolve.Service, cands []resolve.Candidate) *ui {
	ctree := tview.NewTreeView()
	croot := tview.NewTreeNode("")
	ctree.SetRoot(croot).SetTopLevel(1)
	u := &ui{
		clusterState: newClusterState(cluster, cluster),
		clusters:     map[string]*clusterState{},
		ctree:        ctree,
		croot:        croot,
		forwards:     newForwardRegistry(),
		activeCtx:    cluster,
	}
	u.clusters[cluster] = u.clusterState
	u.lastSvcs, u.lastCands = svcs, cands
	return u
}

func svcList(names ...string) []resolve.Service {
	out := make([]resolve.Service, 0, len(names))
	for _, n := range names {
		out = append(out, resolve.Service{Name: n, Mode: "replicated", Running: 1, Desired: 1})
	}
	return out
}

func currentRef(u *ui) any {
	n := u.ctree.GetCurrentNode()
	if n == nil {
		return nil
	}
	return n.GetReference()
}

// The point of the whole feature: come back to a cluster and be where you were.
func TestSwitchingBackRestoresThePosition(t *testing.T) {
	u := treeUI("a", svcList("alpha", "beta", "gamma"), nil)
	u.groupByStack = false
	u.renderContainers()

	// Stand on "gamma" — not the first row, so a reset would be obvious.
	for _, n := range u.croot.GetChildren() {
		if ref, ok := n.GetReference().(svcRef); ok && ref.name == "gamma" {
			u.ctree.SetCurrentNode(n)
		}
	}

	// Switch away: the position is captured, and the widget is rebuilt for a
	// cluster that has no "gamma" at all.
	u.pos = u.treePosFromWidget()
	other := newClusterState("b", "b")
	other.lastSvcs = svcList("db", "web")
	u.clusterState = other
	u.renderContainers()
	if ref, ok := currentRef(u).(svcRef); !ok || ref.name != "db" {
		t.Fatalf("the other cluster should start at its own first row, got %#v", currentRef(u))
	}

	// …and back.
	u.clusterState = u.clusters["a"]
	u.restorePos = true
	u.renderContainers()

	ref, ok := currentRef(u).(svcRef)
	if !ok || ref.name != "gamma" {
		t.Errorf("cursor after switching back = %#v, want gamma", currentRef(u))
	}
}

// The restore is for the switch only. A plain refresh must take the position
// from the tree as it stands, or every refresh would drag the operator back to
// wherever they were when they last arrived.
func TestRestoreAppliesToOneRenderOnly(t *testing.T) {
	u := treeUI("a", svcList("alpha", "beta"), nil)
	u.groupByStack = false
	u.pos = treePos{service: "alpha"}
	u.restorePos = true
	u.renderContainers()
	if ref, _ := currentRef(u).(svcRef); ref.name != "alpha" {
		t.Fatalf("restore did not take: %#v", currentRef(u))
	}
	if u.restorePos {
		t.Error("restorePos must be consumed by the render that used it")
	}

	// The operator moves, then a refresh happens.
	for _, n := range u.croot.GetChildren() {
		if ref, ok := n.GetReference().(svcRef); ok && ref.name == "beta" {
			u.ctree.SetCurrentNode(n)
		}
	}
	u.renderContainers()
	if ref, _ := currentRef(u).(svcRef); ref.name != "beta" {
		t.Errorf("a refresh moved the cursor to %#v, want beta", currentRef(u))
	}
}

// Fold state is part of "where you were": a tree that re-opens every stack is
// not the tree you left.
func TestFoldStateTravelsWithThePosition(t *testing.T) {
	svcs := []resolve.Service{
		{Name: "app_web", Mode: "replicated", Running: 1, Desired: 1, Stack: "app"},
		{Name: "app_db", Mode: "replicated", Running: 1, Desired: 1, Stack: "app"},
	}
	u := treeUI("a", svcs, nil)
	u.groupByStack = true
	u.renderContainers()

	var stack *tview.TreeNode
	for _, n := range u.croot.GetChildren() {
		if isStackNode(n) {
			stack = n
		}
	}
	if stack == nil {
		t.Fatal("expected a stack row")
	}
	if !stack.IsExpanded() {
		t.Fatal("a stack nobody has touched starts open")
	}
	stack.SetExpanded(false)

	pos := u.treePosFromWidget()
	u.pos, u.restorePos = pos, true
	u.renderContainers()

	for _, n := range u.croot.GetChildren() {
		if isStackNode(n) && n.IsExpanded() {
			t.Error("the collapsed stack came back open")
		}
	}
}

// A cluster is remembered by name, so arriving at the same context twice is one
// entry — not a second connection to the same daemon.
func TestClustersAreKeyedByName(t *testing.T) {
	u := &ui{clusters: map[string]*clusterState{}}
	first := u.cluster("prod", "")     // how the session started: resolve from the environment
	again := u.cluster("prod", "prod") // how a switch asks for it
	if first != again {
		t.Error("the same context must resolve to one cluster state")
	}
	if first.ctxOverride != "" {
		t.Errorf("the first visit decides how the cluster is resolved; got %q", first.ctxOverride)
	}
	if other := u.cluster("staging", "staging"); other == first {
		t.Error("different contexts must not share state")
	}
}

// Stale results are the hazard a live switch introduces: a fetch started against
// one cluster can land after the operator has moved to another.
func TestGenerationMovesOnSwitch(t *testing.T) {
	u := &ui{clusters: map[string]*clusterState{}}
	gen := u.generation()
	u.gen.Add(1)
	if u.generation() == gen {
		t.Fatal("the generation must change, or nothing can tell a stale result apart")
	}
}

// A forward outlives a switch now, so the tree annotation has to ask about a
// cluster as well as a container: ids are only unique within one daemon.
func TestForwardsAnnotateOnlyTheirOwnCluster(t *testing.T) {
	r := newForwardRegistry()
	r.add("prod", cand("abc123", "api", "n1"), 9090, 8080, func() {})
	r.add("staging", cand("abc123", "api", "n1"), 9091, 8080, func() {})

	if got := r.forContainer("prod", "abc123"); len(got) != 1 || got[0].local != 9090 {
		t.Errorf("prod should see only its own forward, got %+v", got)
	}
	if got := r.forContainer("staging", "abc123"); len(got) != 1 || got[0].local != 9091 {
		t.Errorf("staging should see only its own forward, got %+v", got)
	}
	if got := r.forContainer("other", "abc123"); len(got) != 0 {
		t.Errorf("a third cluster sharing the container id must see nothing, got %+v", got)
	}
}

// A refused switch must not leave the cluster looking connected: the next
// attempt checks `dcli != nil` to decide whether it can switch instantly, so a
// client left behind by a failed connect would send the operator straight into
// the dead end it just refused.
func TestARefusedClusterDoesNotLookConnected(t *testing.T) {
	c := newClusterState("down", "down")
	c.ctx, c.cancel = context.WithCancel(context.Background())
	dcli, err := client.New(client.WithHost("tcp://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	c.dcli, c.r = dcli, resolve.New(dcli, resolve.AddrHostname)

	c.close()

	if c.dcli != nil {
		t.Error("a closed cluster still holds a client — the next switch would take it for connected")
	}
	if c.cancel != nil || c.ctx != nil {
		t.Error("a closed cluster still holds its context; its background work would outlive it")
	}
}
