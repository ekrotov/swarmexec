// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// G jumps the selection to the last row and g back to the first — not just the
// view. tview's tree only scrolls on them, which left the cursor on a row
// scrolled out of sight (reported from use).
func TestTreeGMovesTheCursor(t *testing.T) {
	names := make([]string, 40)
	for i := range names {
		names[i] = fmt.Sprintf("svc%02d", i)
	}
	u := treeUI("a", svcList(names...), nil)
	u.km = defaultKeybinds()
	u.groupByStack = false
	u.renderContainers()

	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(60, 10)
	u.ctree.SetRect(0, 0, 60, 10)
	u.ctree.Draw(scr)

	press := func(ev *tcell.EventKey) {
		if out := u.containerTreeKeys(ev); out != nil {
			u.ctree.InputHandler()(out, func(tview.Primitive) {})
		}
		u.ctree.Draw(scr)
	}
	selected := func() string {
		ref, _ := currentRef(u).(svcRef)
		return ref.name
	}
	visible := func(name string) bool {
		for y := 0; y < 10; y++ {
			var row strings.Builder
			for x := 0; x < 60; x++ {
				s, _, _ := scr.Get(x, y)
				row.WriteString(s)
			}
			if strings.Contains(row.String(), name) {
				return true
			}
		}
		return false
	}

	press(tcell.NewEventKey(tcell.KeyRune, 'G', tcell.ModNone))
	if got := selected(); got != "svc39" || !visible("svc39") {
		t.Fatalf("after G: selected %q (visible %v), want svc39 on screen", got, visible("svc39"))
	}
	press(tcell.NewEventKey(tcell.KeyRune, 'g', tcell.ModNone))
	if got := selected(); got != "svc00" || !visible("svc00") {
		t.Fatalf("after g: selected %q, want svc00 on screen", got)
	}
	press(tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone))
	if got := selected(); got != "svc39" {
		t.Fatalf("after End: selected %q, want svc39", got)
	}
	press(tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone))
	if got := selected(); got != "svc00" {
		t.Fatalf("after Home: selected %q, want svc00", got)
	}
}

// The last row is the deepest last node along expanded branches; a collapsed
// node's children are not rows.
func TestTreeEdgeNodeFollowsExpandedBranches(t *testing.T) {
	root := tview.NewTreeNode("")
	a := tview.NewTreeNode("a")
	b := tview.NewTreeNode("b")
	b1, b2 := tview.NewTreeNode("b1"), tview.NewTreeNode("b2")
	b.AddChild(b1).AddChild(b2)
	root.AddChild(a).AddChild(b)

	if got := treeEdgeNode(root, false); got != b2 {
		t.Fatalf("expanded: last row %q, want b2", got.GetText())
	}
	b.SetExpanded(false)
	if got := treeEdgeNode(root, false); got != b {
		t.Fatalf("collapsed: last row %q, want b", got.GetText())
	}
	if got := treeEdgeNode(root, true); got != a {
		t.Fatalf("first row %q, want a", got.GetText())
	}
	if treeEdgeNode(tview.NewTreeNode(""), true) != nil {
		t.Fatal("an empty tree has no first row")
	}
}
