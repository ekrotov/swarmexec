// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
)

// The pieces the tree render is made of, one at a time. The render tests in
// ui_cluster_test.go and ui_columnwidth_test.go go through the whole widget;
// these pin down the rules each piece decides.

func treeCands() []resolve.Candidate {
	return []resolve.Candidate{
		{Service: "shop_web", ContainerID: "aaa111", NodeName: "node-a", Slot: 1},
		{Service: "shop_web", ContainerID: "bbb222", NodeName: "node-b", Slot: 2},
		{Service: "shop_db", ContainerID: "ccc333", NodeName: "node-b", Slot: 1},
	}
}

func shownNames(rows []shownService) string {
	var out []string
	for _, r := range rows {
		var ids []string
		for _, c := range r.cands {
			ids = append(ids, c.ContainerID)
		}
		out = append(out, r.svc.Name+"["+strings.Join(ids, " ")+"]")
	}
	return strings.Join(out, ",")
}

func TestFilterTree(t *testing.T) {
	svcs := svcList("idle", "shop_db", "shop_web") // idle runs no task
	cands := treeCands()
	for _, c := range []struct{ filter, want string }{
		// Unfiltered, a service without a running task still shows.
		{"", "idle[],shop_db[ccc333],shop_web[aaa111 bbb222]"},
		// A name match lists every container of the service.
		{"web", "shop_web[aaa111 bbb222]"},
		// A node match lists only the containers on that node, under their
		// services; the services with none there are hidden.
		{"node-b", "shop_db[ccc333],shop_web[bbb222]"},
		// By container id, and trimmed / case-insensitive as typed.
		{" AAA1 ", "shop_web[aaa111]"},
		{"shop", "shop_db[ccc333],shop_web[aaa111 bbb222]"},
		{"nothing-like-it", ""},
	} {
		if got := shownNames(filterTree(svcs, cands, c.filter)); got != c.want {
			t.Errorf("filter %q = %s, want %s", c.filter, got, c.want)
		}
	}
}

func TestContainersByServiceKeepsTheFetchOrder(t *testing.T) {
	got := containersByService(treeCands())
	if len(got) != 2 || len(got["shop_web"]) != 2 || got["shop_web"][0].ContainerID != "aaa111" {
		t.Errorf("grouped = %+v", got)
	}
}

// Every container row is the same width in front of "up", whatever its node
// is called — and the slot column only exists when a task has a slot.
func TestLeafColumnsAlignTheRows(t *testing.T) {
	cands := []resolve.Candidate{
		{ContainerID: "aaa111", NodeName: "n1", Slot: 1},
		{ContainerID: "bbb222", NodeName: "a-much-longer-node", Slot: 12},
	}
	w := leafColumnsOf(cands)
	if w.node != len("a-much-longer-node") || w.slot != len("slot 12") {
		t.Fatalf("widths = %+v", w)
	}
	a, b := w.label(cands[0]), w.label(cands[1])
	if strings.Index(a, " up ") != strings.Index(b, " up ") {
		t.Errorf("rows do not line up:\n%q\n%q", a, b)
	}
	if !strings.Contains(a, "slot 1") {
		t.Errorf("label %q lacks its slot", a)
	}

	global := resolve.Candidate{ContainerID: "ccc333", NodeName: "n1"}
	if got := leafColumnsOf([]resolve.Candidate{global}).label(global); strings.Contains(got, "slot") || strings.Contains(got, "  -  ") {
		t.Errorf("a global task's row = %q, want no slot column", got)
	}
}

func TestServiceWidthsMeasureTheSuffixedImage(t *testing.T) {
	svcs := []resolve.Service{
		{Name: "web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1.25"},
		{Name: "a-longer-name", Mode: "global", Running: 10, Desired: 12, Image: "redis"},
	}
	suffix := func(s resolve.Service) string {
		if s.Name == "web" {
			return " ↑ 1.27"
		}
		return ""
	}
	w := serviceWidths(svcs, suffix)
	want := svcColumns{name: len("a-longer-name"), mode: len("replicated"), repl: len(progressCount(svcs[1])), image: len("nginx:1.25") + len(" ↑ 1.27")}
	if w != want {
		t.Errorf("widths = %+v, want %+v", w, want)
	}
}

// measure only ever widens — until an explicit reload asks to start over.
func TestMeasureWidensUntilAReset(t *testing.T) {
	u := treeUI("a", nil, nil)
	tt, st := u.treeTab(), u.treeTab().st()
	st.svcs = []resolve.Service{{Name: "a-long-service-name"}}
	tt.measure(st)
	wide := st.cols.name
	st.svcs = []resolve.Service{{Name: "web"}}
	tt.measure(st)
	if st.cols.name != wide {
		t.Errorf("a poll narrowed the name column to %d", st.cols.name)
	}
	if _, ok := st.byName["web"]; !ok || len(st.byName) != 1 {
		t.Errorf("byName = %v, want just the current service", st.byName)
	}
	st.resetCols = true
	tt.measure(st)
	if st.cols.name >= wide || st.resetCols {
		t.Errorf("after a reset: name width %d (was %d), resetCols %v", st.cols.name, wide, st.resetCols)
	}
}

// The cursor goes to the most specific row it stood on that is still there.
func TestTreeCursorPick(t *testing.T) {
	root := tview.NewTreeNode("")
	stack, svc, leaf, first := tview.NewTreeNode("stack"), tview.NewTreeNode("svc"), tview.NewTreeNode("leaf"), tview.NewTreeNode("first")
	root.AddChild(stack)
	for _, c := range []struct {
		cur  treeCursor
		want *tview.TreeNode
	}{
		{treeCursor{leaf: leaf, service: svc, stack: stack, first: first}, leaf},
		{treeCursor{service: svc, stack: stack, first: first}, svc},
		{treeCursor{stack: stack, first: first}, stack},
		{treeCursor{first: first}, first},
		{treeCursor{}, stack}, // every stack collapsed: the first row at all
	} {
		if got := c.cur.pick(root); got != c.want {
			t.Errorf("%+v picked %q, want %q", c.cur, got.GetText(), c.want.GetText())
		}
	}
	if (treeCursor{}).pick(tview.NewTreeNode("")) != nil {
		t.Error("an empty tree has no row to pick")
	}
}

// A stack nobody touched starts open; one the operator folded stays folded;
// the stack the cursor stood on is remembered as a target.
func TestAddStackNodes(t *testing.T) {
	svcs := []resolve.Service{
		{Name: "shop_web", Stack: "shop"}, {Name: "blog_web", Stack: "blog"}, {Name: "loose"},
	}
	u := treeUI("a", svcs, nil)
	tt := u.treeTab()
	var cur treeCursor
	nodes := tt.addStackNodes(svcs, treePos{stack: "shop", stackFold: map[string]bool{"shop": false}}, &cur)

	var order []string
	for _, n := range tt.root.GetChildren() {
		order = append(order, n.GetReference().(stackRef).name)
	}
	if strings.Join(order, ",") != "blog,shop,"+noStackLabel {
		t.Errorf("stack order = %v, want alphabetical with the unstacked last", order)
	}
	if !nodes["blog"].IsExpanded() || nodes["shop"].IsExpanded() {
		t.Error("blog (untouched) must be open, shop (folded) closed")
	}
	if cur.stack != nodes["shop"] {
		t.Error("the stack the cursor stood on is the fallback target")
	}
}

// A filter that empties a stack drops its row, and the rows that stay count
// only what the filter left under them.
func TestRenderPrunesStacksTheFilterEmptied(t *testing.T) {
	svcs := []resolve.Service{
		{Name: "shop_web", Stack: "shop", Mode: "replicated", Running: 1, Desired: 1},
		{Name: "shop_db", Stack: "shop", Mode: "replicated", Running: 1, Desired: 1},
		{Name: "blog_web", Stack: "blog", Mode: "replicated", Running: 1, Desired: 1},
	}
	u := treeUI("a", svcs, nil)
	tt := u.treeTab()
	tt.st().filter = "shop_w"
	tt.render()
	kids := tt.root.GetChildren()
	if len(kids) != 1 || kids[0].GetReference().(stackRef).name != "shop" {
		t.Fatalf("rows = %d, want only the shop stack", len(kids))
	}
	if got := kids[0].GetText(); !strings.Contains(got, "(1 svc") {
		t.Errorf("stack row %q should count the one service the filter left", got)
	}
	if ref, ok := currentRef(u).(svcRef); !ok || ref.name != "shop_web" {
		t.Errorf("cursor on %#v, want the first shown service", currentRef(u))
	}
}

// The empty states say which empty it is, and are not a row to act on.
func TestRenderEmptyStates(t *testing.T) {
	u := treeUI("a", nil, nil)
	tt := u.treeTab()
	tt.render()
	only := tt.root.GetChildren()
	if len(only) != 1 || !strings.Contains(only[0].GetText(), "nothing is deployed") || only[0].GetReference() != nil {
		t.Fatalf("empty swarm = %q", only[0].GetText())
	}

	tt.st().svcs = svcList("web")
	tt.st().filter = "nope"
	tt.render()
	if got := tt.root.GetChildren()[0].GetText(); got != `(no matches for "nope")` {
		t.Errorf("no match = %q", got)
	}
}

// A cursor that stood on a container comes back to it, unfolding its service
// to show it — a refresh must not hide the row the operator is on.
func TestRenderRevealsTheSelectedContainer(t *testing.T) {
	u := treeUI("a", svcList("shop_db", "shop_web"), treeCands())
	tt := u.treeTab()
	tt.groupByStack = false
	tt.st().pos = treePos{container: "bbb222"}
	tt.render()
	c, ok := currentRef(u).(resolve.Candidate)
	if !ok || c.ContainerID != "bbb222" {
		t.Fatalf("cursor on %#v, want container bbb222", currentRef(u))
	}
	if web := parentOf(tt.root, u.ctree.GetCurrentNode()); web == nil || !web.IsExpanded() || !strings.HasPrefix(web.GetText(), "▾") {
		t.Error("the service holding the cursor must be unfolded and marked so")
	}
	if base := tt.st().leafBase["bbb222"]; !strings.Contains(base, "node-b") {
		t.Errorf("leafBase = %q, want the badge-free row", base)
	}
}
