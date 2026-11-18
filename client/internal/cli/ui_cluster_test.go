// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/client"
	"github.com/rivo/tview"

	"swarmexec/client/internal/portfwd"
	"swarmexec/client/internal/resolve"
)

// treeUI is the smallest ui that can render a container tree: a forward
// registry for the annotations and a cluster to hold the data; the tree tab
// builds its widgets on first use. Everything else the render touches is
// nil-tolerant.
func treeUI(cluster string, svcs []resolve.Service, cands []resolve.Candidate) *ui {
	u := &ui{
		clusterState: newClusterState(cluster, cluster),
		clusters:     map[string]*clusterState{},
		forwards:     portfwd.NewRegistry(),
		activeCtx:    cluster,
	}
	u.clusters[cluster] = u.clusterState
	st := u.treeTab().st()
	st.svcs, st.cands = svcs, cands
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

// selectService puts the tree cursor on the named service row.
func selectService(t *testing.T, u *ui, name string) {
	t.Helper()
	var found bool
	eachServiceNode(u.treeTab().root, func(n *tview.TreeNode) {
		if n.GetReference().(svcRef).name == name {
			u.ctree.SetCurrentNode(n)
			found = true
		}
	})
	if !found {
		t.Fatalf("service %q not in the tree", name)
	}
}

// The point of the whole feature: come back to a cluster and be where you were.
func TestSwitchingBackRestoresThePosition(t *testing.T) {
	u := treeUI("a", svcList("alpha", "beta", "gamma"), nil)
	tt := u.treeTab()
	tt.groupByStack = false
	tt.render()

	// Stand on "gamma" — not the first row, so a reset would be obvious.
	selectService(t, u, "gamma")

	// Switch away: the widget is rebuilt for a cluster that has no "gamma" at
	// all, and the render saves a's position before it does.
	u.clusterState = newClusterState("b", "b")
	tt.st().svcs = svcList("db", "web")
	tt.render()
	if ref, ok := currentRef(u).(svcRef); !ok || ref.name != "db" {
		t.Fatalf("the other cluster should start at its own first row, got %#v", currentRef(u))
	}

	// …and back.
	u.clusterState = u.clusters["a"]
	tt.render()

	ref, ok := currentRef(u).(svcRef)
	if !ok || ref.name != "gamma" {
		t.Errorf("cursor after switching back = %#v, want gamma", currentRef(u))
	}
}

// The saved position is for a switch only. A plain refresh must take the
// position from the tree as it stands, or every refresh would drag the
// operator back to wherever they were when they last arrived.
func TestSavedPositionAppliesToTheSwitchOnly(t *testing.T) {
	u := treeUI("a", svcList("alpha", "beta"), nil)
	tt := u.treeTab()
	tt.groupByStack = false
	tt.st().pos = treePos{service: "beta"} // as a switch away from a would leave it
	tt.render()
	if ref, _ := currentRef(u).(svcRef); ref.name != "beta" {
		t.Fatalf("the saved position did not take: %#v", currentRef(u))
	}

	// The operator moves, then a refresh happens.
	selectService(t, u, "alpha")
	tt.render()
	if ref, _ := currentRef(u).(svcRef); ref.name != "alpha" {
		t.Errorf("a refresh moved the cursor to %#v, want alpha", currentRef(u))
	}
}

// An error in between does not lose the position either: the tree shows the
// error, and the next good fetch puts the cursor back.
func TestAnErrorKeepsThePosition(t *testing.T) {
	u := treeUI("a", svcList("alpha", "beta", "gamma"), nil)
	tt := u.treeTab()
	tt.groupByStack = false
	tt.render()
	selectService(t, u, "beta")

	tt.apply(nil, nil, errors.New("manager unreachable"))
	if got := tt.root.GetChildren()[0].GetText(); !strings.Contains(got, "manager unreachable") {
		t.Fatalf("error state = %q", got)
	}
	if tt.st().svcs == nil || tt.st().fetchErr == nil {
		t.Error("a failed fetch keeps the last services, and says they are stale")
	}

	tt.apply(svcList("alpha", "beta", "gamma"), nil, nil)
	if ref, _ := currentRef(u).(svcRef); ref.name != "beta" {
		t.Errorf("cursor after the error cleared = %#v, want beta", currentRef(u))
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
	tt := u.treeTab()
	tt.render()

	var stack *tview.TreeNode
	for _, n := range tt.root.GetChildren() {
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

	// Away to a cluster with the same stack name, where it starts open…
	u.clusterState = newClusterState("b", "b")
	tt.st().svcs = svcs
	tt.render()
	if !tt.root.GetChildren()[0].IsExpanded() {
		t.Fatal("b's stack must not inherit a's fold")
	}
	// …and back, where it is still closed.
	u.clusterState = u.clusters["a"]
	tt.render()
	for _, n := range tt.root.GetChildren() {
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
	r := portfwd.NewRegistry()
	r.Add("prod", cand("abc123", "api", "n1"), 9090, 8080, func() {})
	r.Add("staging", cand("abc123", "api", "n1"), 9091, 8080, func() {})

	if got := r.ForContainer("prod", "abc123"); len(got) != 1 || got[0].Local != 9090 {
		t.Errorf("prod should see only its own forward, got %+v", got)
	}
	if got := r.ForContainer("staging", "abc123"); len(got) != 1 || got[0].Local != 9091 {
		t.Errorf("staging should see only its own forward, got %+v", got)
	}
	if got := r.ForContainer("other", "abc123"); len(got) != 0 {
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
