// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// killAgent serves the two container RPCs of the container menu (P-03) and
// records what it was asked.
type killAgent struct {
	pb.UnimplementedAgentServer
	mu        sync.Mutex
	killed    []string
	inspected []string
}

func (a *killAgent) KillContainer(_ context.Context, req *pb.KillContainerRequest) (*pb.KillContainerResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.killed = append(a.killed, req.GetContainerId())
	return &pb.KillContainerResponse{}, nil
}

func (a *killAgent) InspectContainer(_ context.Context, req *pb.InspectContainerRequest) (*pb.InspectContainerResponse, error) {
	a.mu.Lock()
	a.inspected = append(a.inspected, req.GetContainerId())
	a.mu.Unlock()
	return &pb.InspectContainerResponse{InspectJson: []byte(`{"Id":"` + req.GetContainerId() +
		`","Name":"/web.1","State":{"Status":"running","Running":true,"Pid":7},"Config":{"Env":["SECRET=hunter2"]}}`)}, nil
}

func (a *killAgent) calls() (killed, inspected []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.killed...), append([]string(nil), a.inspected...)
}

// menuUI is the tree harness over shopSwarm with agent a on both nodes and a
// simulation screen (for the clipboard), its cursor on web1's container.
func menuUI(t *testing.T, a pb.AgentServer) (*ui, resolve.Candidate) {
	t.Helper()
	u, _, _ := editUIWith(t, shopSwarm(), map[string]pb.AgentServer{"node-a": a, "node-b": a})
	onLoop(t, u, func() {
		u.search = tview.NewInputField()
		u.root = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.search, 0, 0, false)
		u.screen = tcell.NewSimulationScreen("UTF-8")
		u.treeTab().load()
	})
	waitForTree(t, u, "shop_web", "web1aaaaaaaa")
	cursorTo(t, u, "web1aaaaaaaa")
	var c resolve.Candidate
	onLoop(t, u, func() { c, _ = u.ctree.GetCurrentNode().GetReference().(resolve.Candidate) })
	if c.ContainerID == "" {
		t.Fatal("cursor is not on a container")
	}
	return u, c
}

// pickMenuItem opens the container menu for c and selects the item whose text
// contains label.
func pickMenuItem(t *testing.T, u *ui, c resolve.Candidate, label string) {
	t.Helper()
	onLoop(t, u, func() { u.containerMenu(c) })
	waitFront(t, u, pageMenu)
	found := false
	onLoop(t, u, func() {
		list, ok := u.app.GetFocus().(*tview.List)
		if !ok {
			return
		}
		for i := range list.GetItemCount() {
			if main, _ := list.GetItemText(i); strings.Contains(main, label) {
				list.SetCurrentItem(i)
				found = true
			}
		}
	})
	if !found {
		t.Fatalf("menu has no %q item", label)
	}
	keyCode(t, u, tcell.KeyEnter)
}

func TestContainerMenuKillTaskConfirmsThenKills(t *testing.T) {
	a := &killAgent{}
	u, c := menuUI(t, a)

	// Cancel first: nothing reaches the agent.
	pickMenuItem(t, u, c, "kill task")
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyTab) // to "Cancel"
	keyCode(t, u, tcell.KeyEnter)
	waitFor(t, "confirm closed", func() bool { return !pageOpen(t, u, pageConfirm) })
	if killed, _ := a.calls(); len(killed) != 0 {
		t.Fatalf("a cancelled kill reached the agent: %v", killed)
	}

	pickMenuItem(t, u, c, "kill task")
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyEnter) // "Kill" has the focus
	waitFor(t, "the kill", func() bool { k, _ := a.calls(); return len(k) == 1 })
	if killed, _ := a.calls(); killed[0] != c.ContainerID {
		t.Errorf("killed %v, want %s", killed, c.ContainerID)
	}
	waitFor(t, "the success flash", func() bool {
		var txt string
		onLoop(t, u, func() { txt = u.help.GetText(true) })
		return strings.Contains(txt, "swarm reschedules")
	})
}

// An agent that predates KillContainer: the operator is told it is too old.
func TestContainerMenuKillOnAnOldAgent(t *testing.T) {
	u, c := menuUI(t, &volumeAgent{})
	pickMenuItem(t, u, c, "kill task")
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageInfo)
}

func TestContainerMenuInspectContainer(t *testing.T) {
	a := &killAgent{}
	u, c := menuUI(t, a)
	pickMenuItem(t, u, c, "inspect container")
	waitFront(t, u, pageInspect)
	waitFor(t, "the inspect fetch", func() bool { _, in := a.calls(); return len(in) == 1 })
	waitFor(t, "the summary", func() bool {
		var txt string
		onLoop(t, u, func() {
			if tbl, ok := u.app.GetFocus().(*tview.Table); ok {
				for r := range tbl.GetRowCount() {
					if cell := tbl.GetCell(r, 0); cell != nil {
						txt += cell.Text + "\n"
					}
				}
			}
		})
		return strings.Contains(txt, "CONTAINER") && strings.Contains(txt, "pid") && !strings.Contains(txt, "hunter2")
	})
}

func TestContainerMenuCopyContainerID(t *testing.T) {
	u, c := menuUI(t, &killAgent{})
	pickMenuItem(t, u, c, "copy container id")
	waitFor(t, "the clipboard", func() bool {
		var got string
		onLoop(t, u, func() { got = string(u.screen.(tcell.SimulationScreen).GetClipboardData()) })
		return got == c.ContainerID
	})
	if pageOpen(t, u, pageMenu) {
		t.Error("the menu stays open after a copy")
	}
}
