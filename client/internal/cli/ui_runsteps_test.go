// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// The setup steps run is made of (review Q-01 split it): the window
// buildChrome assembles, the search bar wireSearch wires, and what
// startSession loads and shows. runScreen takes a real terminal and is left
// to the binary.

// chromeUI is a running UI over the shop cluster with the window built and
// the search bar and context list wired, as run leaves it before startSession.
func chromeUI(t *testing.T) *ui {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir()) // no stored contexts but "default"
	u, _, _ := editUI(t, shopSwarm())
	onLoop(t, u, func() {
		u.content = tview.NewPages()
		u.service = "shop_db"
		u.selStyle = tcell.StyleDefault
		u.buildChrome()
		u.wireSearch()
		u.active = "containers"
		u.wireContexts()
	})
	return u
}

func TestBuildChromeLaysOutTheWindow(t *testing.T) {
	u := chromeUI(t)
	onLoop(t, u, func() {
		if name, p := u.pages.GetFrontPage(); name != pageMain || p != u.root {
			t.Errorf("front page %q (%T), want main = root", name, p)
		}
		want := []tview.Primitive{u.tabBar, u.body, u.search, u.footer}
		if n := u.root.GetItemCount(); n != len(want) {
			t.Fatalf("root has %d items", n)
		}
		for i, w := range want {
			if u.root.GetItem(i) != w {
				t.Errorf("root item %d is %T", i, u.root.GetItem(i))
			}
		}
		if u.body.GetItem(0) != u.content || u.body.GetItem(2) != u.sidebar || u.sidebar.GetItem(0) != u.cxtable {
			t.Error("body is not content | gap | sidebar(contexts)")
		}
		if u.footer.GetItem(0) != u.help || u.footer.GetItem(1) != u.statusRow ||
			u.statusRow.GetItem(0) != u.status || u.statusRow.GetItem(1) != u.statusRight {
			t.Error("footer is not help over status | statusRight")
		}
		for i, tab := range uiTabList {
			if !u.content.HasPage(tab.key) {
				t.Errorf("no content page for %s", tab.key)
			}
			if name, _ := u.content.GetFrontPage(); i == 0 && name != tab.key {
				t.Errorf("content opens on %q, want %q", name, tab.key)
			}
		}
		if f := u.treeTab().st().filter; f != "shop_db" {
			t.Errorf("tree filter %q, want the service argument", f)
		}
		if u.clusterText == "" || u.cxtable.GetInputCapture() == nil {
			t.Error("cluster text or context keys not set")
		}
	})
}

// The bar filters the tab it was opened on as it is typed into; Enter keeps
// the filter and gives the keyboard back to the tab, Esc clears it.
func TestSearchBarFiltersAndCloses(t *testing.T) {
	u := chromeUI(t)
	onLoop(t, u, func() { u.treeTab().load() })
	waitForTree(t, u, "shop_db")
	onLoop(t, u, func() {
		u.startSearch("containers")
		u.search.SetText("loose")
	})
	if got := treeText(t, u); strings.Contains(got, "shop_web") || !strings.Contains(got, "loose") {
		t.Errorf("filtered tree:\n%s", got)
	}
	onLoop(t, u, func() {
		u.search.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(tview.Primitive) {})
	})
	onLoop(t, u, func() {
		if s := u.treeTab().searchText(); s != "loose" {
			t.Errorf("Enter dropped the filter (%q)", s)
		}
		if u.app.GetFocus() != u.treeTab().widget() {
			t.Errorf("Enter left focus on %T", u.app.GetFocus())
		}
	})

	onLoop(t, u, func() {
		u.startSearch("containers")
		u.search.InputHandler()(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone), func(tview.Primitive) {})
	})
	onLoop(t, u, func() {
		if s := u.treeTab().searchText(); s != "" {
			t.Errorf("Esc kept the filter %q", s)
		}
	})
	if got := treeText(t, u); !strings.Contains(got, "shop_web") {
		t.Errorf("tree after Esc:\n%s", got)
	}
}

// startSession loads the first frame — contexts, the tree — and shows the
// keys.yaml warnings over it.
func TestStartSessionLoadsAndWarns(t *testing.T) {
	u := chromeUI(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	onLoop(t, u, func() {
		u.runCtx = ctx
		u.g = &globalFlags{}
		u.startSession([]string{`unknown action "nope"`})
	})
	waitFront(t, u, pageInfo)
	if text := drawFront(t, u, 200, 40); !hasAll(text, "keys.yaml:", `unknown action "nope"`) {
		t.Errorf("warning:\n%s", text)
	}
	if got := treeText(t, u); !strings.Contains(got, "shop_db") {
		t.Errorf("the tree was not loaded:\n%s", got)
	}
	onLoop(t, u, func() {
		if u.active != "containers" || u.cxtable.GetRowCount() == 0 {
			t.Errorf("active %q, %d context rows", u.active, u.cxtable.GetRowCount())
		}
	})
}
