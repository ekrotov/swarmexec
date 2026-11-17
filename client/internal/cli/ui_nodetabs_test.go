// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/rivo/tview"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmops"
)

// nodeManager is a listManager that also answers the task list the Nodes tab
// counts running tasks from.
type nodeManager struct{ listManager }

func (m *nodeManager) TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error) {
	return client.TaskListResult{}, nil
}

// nodesUI is a running UI whose cluster has the two nodes of mgr and, behind
// them, fake agents: node-a holds two volumes, node-b one.
func nodesUI(t *testing.T) (*ui, *nodeManager) {
	t.Helper()
	m := &nodeManager{}
	m.nodes = []swarm.Node{readyNode("n1", "node-a"), readyNode("n2", "node-b")}
	m.nodes[0].Spec.Role = swarm.NodeRoleManager
	m.nodes[0].ManagerStatus = &swarm.ManagerStatus{Leader: true}
	cfg := startVolumeAgents(t, map[string]*volumeAgent{
		"node-a": {vols: []string{"shop_data", "cache"}},
		"node-b": {vols: []string{"shop_data"}},
	})
	u := listUI(t, &m.listManager)
	onLoop(t, u, func() {
		u.dcli = m
		u.r = resolve.New(m, resolve.AddrHostname)
		u.cfg = cfg
		u.agents = agentcall.New(cfg)
		u.f = &uiFlags{connectTimeout: 3 * time.Second}
	})
	return u, m
}

// The Nodes tab lists the nodes from the manager, the leader starred, and
// fills the VOLS column from the agents once their counts arrive.
func TestNodesTabLoadsNodesAndVolumeCounts(t *testing.T) {
	u, _ := nodesUI(t)
	onLoop(t, u, func() { u.tab("nodes").enter() })
	waitForTable(t, u, u.nodeTab().table, "★ node-a", "node-b", "manager")
	deadline := time.Now().Add(10 * time.Second)
	for {
		var a, b string
		onLoop(t, u, func() {
			a = u.nodeTab().table.GetCell(1, 6).Text
			b = u.nodeTab().table.GetCell(2, 6).Text
		})
		if a == "2" && b == "1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("VOLS = %q/%q, want 2/1", a, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var n swarmops.NodeInfo
	onLoop(t, u, func() {
		u.nodeTab().table.Select(2, 0)
		n, _ = u.nodeTab().selected()
	})
	if n.Hostname != "node-b" {
		t.Errorf("selected() = %q, want node-b", n.Hostname)
	}
}

// The node keys open their dialogs over the selected node, and keys that are
// not the tab's own still reach the shared ones.
func TestNodesTabKeys(t *testing.T) {
	u := tabsUI()
	nt := u.nodeTab()
	nt.st().rows = []swarmops.NodeInfo{{Hostname: "node-a", Labels: map[string]string{"zone": "a"}}}
	nt.render()

	if nt.keys(rune_(u.km.NodeLabels)) != nil || !u.pages.HasPage(pageNodeLabels) {
		t.Fatal("the labels key must open the label editor")
	}
	u.pages.RemovePage(pageNodeLabels)
	if nt.keys(rune_(u.km.NodeAvail)) != nil || !u.pages.HasPage(pageNodeAvail) {
		t.Fatal("the availability key must open the availability menu")
	}
	if ev := nt.keys(rune_('k')); ev == nil || ev.Key() != tcell.KeyUp {
		t.Error("k must reach tabKeys and come back as Up")
	}
}

// Forwards: the table lists the session's forwards whatever cluster they
// belong to, the stop key stops the one under the cursor, and Enter shows it.
func TestForwardsTabStopKey(t *testing.T) {
	u := tabsUI()
	stopped := map[string]bool{}
	for _, name := range []string{"api", "web"} {
		e := u.forwards.addAt("other-cluster", "127.0.0.1", resolve.Candidate{Service: name, ContainerID: "c-" + name, NodeName: "n1"}, 0, 80, func() { stopped[name] = true })
		u.forwards.markActive(e.id, "127.0.0.1:8080")
	}
	ft := u.fwdTab()
	ft.render()
	text := tableColumnText(ft.table, 3)
	if text != "api,web" {
		t.Fatalf("forwards listed = %q", text)
	}
	if got := ft.table.GetCell(1, 5).Text; got != "other-cluster" {
		t.Errorf("CLUSTER column = %q", got)
	}

	ft.table.Select(2, 0)
	if ft.keys(rune_(u.km.FwdStop)) != nil {
		t.Fatal("the stop key must be consumed")
	}
	if !stopped["web"] || stopped["api"] {
		t.Errorf("stopped = %v, want only web", stopped)
	}
	if got := tableColumnText(ft.table, 3); got != "api" {
		t.Errorf("after stop the table lists %q", got)
	}

	ft.table.Select(1, 0)
	ft.keys(rune_('i'))
	if !u.pages.HasPage(pageFwdDetail) {
		t.Error("i must open the forward's detail")
	}
}

func tableColumnText(tbl *tview.Table, col int) string {
	var out []string
	for r := 1; r < tbl.GetRowCount(); r++ {
		if c := tbl.GetCell(r, col); c != nil && c.Text != "" {
			out = append(out, c.Text)
		}
	}
	return strings.Join(out, ",")
}

// The schedule editor (split into a type, review Q-01): space toggles the row
// under the cursor, +/- change the runs per day within bounds, w asks before
// saving, and Esc closes.
func TestScheduleEditorKeys(t *testing.T) {
	u := tabsUI()
	svc := &swarm.Service{}
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{}
	u.showScheduleEditor(svc, u.nodeTab().table)
	if !u.pages.HasPage(pageNodeSchedule) {
		t.Fatal("the editor did not open")
	}
	_, front := u.pages.GetFrontPage()
	// The editor is reachable through its table's input capture only; drive
	// it the way the keyboard does.
	table := findTable(front)
	if table == nil {
		t.Fatal("no table in the editor")
	}
	send := func(ev *tcell.EventKey) { table.GetInputCapture()(ev) }
	cell := func(row int) string { return table.GetCell(row, 1).Text }

	before := cell(0) // the "enabled" row
	send(rune_(' '))
	if cell(0) == before {
		t.Errorf("space did not toggle the enabled row (%q)", before)
	}

	timesRow := -1
	for i, r := range schedRows() {
		if r.kind == rowTimes {
			timesRow = i
		}
	}
	t0 := cell(timesRow)
	send(rune_('+'))
	if cell(timesRow) == t0 {
		t.Errorf("+ did not change the runs per day (%q)", t0)
	}
	send(rune_('-'))
	if cell(timesRow) != t0 {
		t.Errorf("- did not undo + (%q, want %q)", cell(timesRow), t0)
	}

	send(rune_('w'))
	if !u.pages.HasPage(pageConfirm) && !u.pages.HasPage(pageInfo) {
		t.Error("w must ask before saving (or say why it cannot)")
	}
	u.pages.RemovePage(pageConfirm)
	u.pages.RemovePage(pageInfo)
	// Ctrl-S is the same apply (T-12).
	send(tcell.NewEventKey(tcell.KeyCtrlS, 0, tcell.ModCtrl))
	if !u.pages.HasPage(pageConfirm) && !u.pages.HasPage(pageInfo) {
		t.Error("Ctrl-S must ask before saving (or say why it cannot)")
	}
	u.pages.RemovePage(pageConfirm)
	u.pages.RemovePage(pageInfo)
	send(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if u.pages.HasPage(pageNodeSchedule) {
		t.Error("Esc must close the editor")
	}
}

// findTable finds the first table inside an overlay's primitive tree.
func findTable(p tview.Primitive) *tview.Table {
	switch w := p.(type) {
	case *tview.Table:
		return w
	case *centeredBox:
		return findTable(w.inner)
	case *tview.Flex:
		for i := 0; i < w.GetItemCount(); i++ {
			if tb := findTable(w.GetItem(i)); tb != nil {
				return tb
			}
		}
	}
	return nil
}
