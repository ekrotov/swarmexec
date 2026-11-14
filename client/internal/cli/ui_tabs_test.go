// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmops"
)

// tabsUI is a bare session with every shared widget the tab registry's
// adapters read, on cluster "a". No event loop: the tab methods under test
// run synchronously.
func tabsUI() *ui {
	u := overlayUI()
	u.km = defaultKeybinds()
	u.clusterState = newClusterState("a", "a")
	u.clusters = map[string]*clusterState{"a": u.clusterState}
	u.ctree, u.croot = tview.NewTreeView(), tview.NewTreeNode("")
	u.ctree.SetRoot(u.croot)
	u.cxtable = newStateTable()
	u.status = tview.NewTextView()
	u.forwards = newForwardRegistry()
	return u
}

// The registry and the tab bar are two lists; a tab in one and not the other
// is either unreachable or a key that does nothing. Each tab must also answer
// every question the session asks of it.
func TestEveryTabIsRegistered(t *testing.T) {
	u := tabsUI()
	reg := newTabViews(u)
	if len(reg) != len(uiTabList) {
		t.Errorf("registry has %d tabs, the tab bar %d", len(reg), len(uiTabList))
	}
	for _, tab := range uiTabList {
		v := u.tab(tab.key)
		if v == nil {
			t.Errorf("%s: not in the registry", tab.key)
			continue
		}
		if v.widget() == nil {
			t.Errorf("%s: no widget", tab.key)
		}
		if strings.TrimSpace(v.footer()) == "" {
			t.Errorf("%s: empty footer", tab.key)
		}
		h := &helpWriter{}
		v.helpSection(h)
		if !strings.Contains(h.b.String(), "[aqua]") {
			t.Errorf("%s: no section in the help overlay", tab.key)
		}
	}
	if u.tab("contexts") != nil {
		t.Error("contexts is the sidebar, not a tab")
	}
}

// The "?" overlay lists the tabs in tab-bar order, each once.
func TestHelpOverlayFollowsTheTabOrder(t *testing.T) {
	u := tabsUI()
	u.showHelp()
	_, front := u.pages.GetFrontPage()
	var text string
	var find func(p tview.Primitive)
	find = func(p tview.Primitive) {
		switch w := p.(type) {
		case *tview.TextView:
			text = w.GetText(false)
		case *centeredBox:
			find(w.inner)
		}
	}
	find(front)
	if text == "" {
		t.Fatal("no help text found")
	}
	last := -1
	for _, title := range []string{"Global", "Stacks/Services", "Service inspect", "Volumes", "Forwards", "Networks", "Secrets", "Nodes", "Configs", "Contexts (sidebar", "Log view"} {
		i := strings.Index(text, "[aqua]"+title)
		if i < 0 {
			t.Fatalf("help lacks section %q", title)
		}
		if i < last {
			t.Errorf("section %q is out of order", title)
		}
		last = i
	}
	if !strings.Contains(text, "1–7") {
		t.Error("the tab-number range must still read 1–7")
	}
}

func vol(name string, nodes ...string) swarmVolume {
	v := swarmVolume{Name: name, Driver: "local"}
	for _, n := range nodes {
		v.Nodes = append(v.Nodes, resolve.Node{Name: n})
	}
	return v
}

func rune_(r rune) *tcell.EventKey { return tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone) }

// tableColumn is column 0 of every data row, selection marker stripped.
func tableColumn(tbl *tview.Table) []string {
	var out []string
	for r := 1; r < tbl.GetRowCount(); r++ {
		if c := tbl.GetCell(r, 0); c != nil {
			out = append(out, strings.TrimLeft(c.Text, "▣ "))
		}
	}
	return out
}

func cursorName(tbl *tview.Table) string {
	row, _ := tbl.GetSelection()
	return strings.TrimLeft(tbl.GetCell(row, 0).Text, "▣ ")
}

// The "/" filter narrows the rows locally, and delete/select act on what is
// shown — the shown slice and the table must agree.
func TestVolumesTabFilter(t *testing.T) {
	u := tabsUI()
	vt := u.volTab()
	vt.st().vols = []swarmVolume{vol("shop_data", "node-a"), vol("db_data", "node-b"), vol("cache", "node-b")}
	vt.render()
	if got := tableColumn(vt.table); len(got) != 3 {
		t.Fatalf("unfiltered rows = %v", got)
	}

	var s tabSearcher = vt
	s.setSearch("node-b") // matches by node, too
	if got := tableColumn(vt.table); strings.Join(got, ",") != "cache,db_data" {
		t.Errorf("filtered by node = %v, want cache,db_data (name order)", got)
	}
	if v, ok := vt.selected(); !ok || v.Name != "cache" {
		t.Errorf("selected() = %v, want the first SHOWN row", v.Name)
	}

	s.setSearch("nothing-like-it")
	if got := vt.table.GetCell(1, 0).Text; !strings.Contains(got, `no matches for "nothing-like-it"`) {
		t.Errorf("no-match state = %q", got)
	}
	if s.searchText() != "nothing-like-it" {
		t.Error("the filter must be kept for the bar to show again")
	}
}

// Space marks, a selects all shown, the status line counts them — and only
// while the volumes tab is the visible one.
func TestVolumesTabSelectKeys(t *testing.T) {
	u := tabsUI()
	u.active = "volumes"
	vt := u.volTab()
	vt.st().vols = []swarmVolume{vol("a1"), vol("a2"), vol("a3")}
	vt.render()

	if vt.keys(rune_(u.km.VolSelect)) != nil {
		t.Fatal("the select key must be consumed")
	}
	if !vt.st().selected["a1"] {
		t.Fatal("space did not mark the volume under the cursor")
	}
	if got := u.status.GetText(true); !strings.Contains(got, "1 selected") {
		t.Errorf("status = %q, want the selection count", got)
	}
	vt.keys(rune_(u.km.VolSelectAll))
	if len(vt.st().selected) != 3 {
		t.Errorf("select all marked %d, want 3", len(vt.st().selected))
	}
	vt.keys(rune_(u.km.VolSelectAll))
	if len(vt.st().selected) != 0 {
		t.Errorf("select all twice should clear, left %d", len(vt.st().selected))
	}

	vt.keys(rune_(u.km.VolSelect))
	u.active = "networks"
	u.updateStatus()
	if got := u.status.GetText(true); strings.Contains(got, "selected") {
		t.Errorf("status on another tab = %q, want no volume count", got)
	}
}

// The sort key cycles the field and marks the header; S reverses.
func TestVolumesTabSortKeys(t *testing.T) {
	u := tabsUI()
	vt := u.volTab()
	a, b := vol("a", "n1"), vol("b", "n1", "n2")
	vt.st().vols = []swarmVolume{a, b}
	vt.render()
	if got := vt.table.GetCell(0, 0).Text; got != "VOLUME ▲" {
		t.Errorf("header = %q, want name ascending", got)
	}
	vt.keys(rune_(u.km.VolSort)) // → nodes, descending
	if got := vt.table.GetCell(0, 2).Text; got != "NODES ▼" {
		t.Errorf("header = %q, want nodes descending", got)
	}
	if got := tableColumn(vt.table); got[0] != "b" {
		t.Errorf("rows = %v, want the volume on more nodes first", got)
	}
	vt.keys(rune_(u.km.VolSortRev))
	if got := tableColumn(vt.table); got[0] != "a" {
		t.Errorf("reversed rows = %v", got)
	}
	// Keys that are not the tab's own fall through to the shared ones.
	if ev := vt.keys(rune_('j')); ev == nil || ev.Key() != tcell.KeyDown {
		t.Error("j must reach tabKeys and come back as Down")
	}
}

// A cluster switch keeps the cursor, the filter and the selection per
// cluster, and the sort order — a preference of the operator's — across them.
func TestVolumesTabCursorIsPerCluster(t *testing.T) {
	u := tabsUI()
	vt := u.volTab()
	a := u.clusterState
	b := newClusterState("b", "b")

	// Cursors away from the top row, and where a reversed sort does not put
	// them on the top row by chance: a2 of four, b2 of three.
	vt.st().vols = []swarmVolume{vol("a1"), vol("a2"), vol("a3"), vol("a4")}
	vt.render()
	vt.table.Select(2, 0) // a2
	vt.st().selected["a3"] = true

	u.clusterState = b
	vt.st().vols = []swarmVolume{vol("b1"), vol("b2"), vol("b3")}
	vt.render()
	if got := cursorName(vt.table); got != "b1" {
		t.Fatalf("first visit to b: cursor on %q, want the top", got)
	}
	vt.table.Select(2, 0) // b2
	vt.keys(rune_(u.km.VolSortRev))
	if got := cursorName(vt.table); got != "b2" {
		t.Fatalf("sorting moved the cursor off b2 to %q", got)
	}
	vt.st().filter = "b"

	u.clusterState = a
	vt.render()
	if got := cursorName(vt.table); got != "a2" {
		t.Errorf("back on a: cursor on %q, want a2", got)
	}
	if got := tableColumn(vt.table); got[0] != "a4" {
		t.Errorf("back on a: rows %v, want the reversed sort kept", got)
	}
	if !vt.st().selected["a3"] || vt.st().filter != "" {
		t.Error("a's selection must survive, and b's filter must not leak into it")
	}
	vt.table.Select(4, 0) // a1, moved while on a

	u.clusterState = b
	vt.render()
	if got := cursorName(vt.table); got != "b2" {
		t.Errorf("back on b: cursor on %q, want b2", got)
	}

	// A reload's loading placeholder does not lose the cursor either.
	vt.placeholder(loadingCell())
	vt.render()
	if got := cursorName(vt.table); got != "b2" {
		t.Errorf("after a reload: cursor on %q, want b2", got)
	}

	u.clusterState = a
	vt.render()
	if got := cursorName(vt.table); got != "a1" {
		t.Errorf("a again: cursor on %q, want a1", got)
	}
}

// The tabs that used to fall into the switches' default branches: Configs'
// copy key copied the volume list, Nodes' too. Each copies its own list now.
func TestEveryTabYanksItsOwnList(t *testing.T) {
	u := tabsUI()
	u.cfgTab().st().rows = []swarmops.Config{{Name: "nginx.conf", Size: 2048}}
	u.nodeTab().st().rows = []swarmops.NodeInfo{{Hostname: "mgr-1", Role: "manager", Leader: true}}
	u.netTab().st().rows = []swarmops.Network{{Name: "edge"}}
	u.secTab().st().rows = []swarmops.Secret{{Name: "db_pw"}}
	u.volTab().st().vols = []swarmVolume{vol("shop_data", "node-a")}
	u.volTab().render()
	u.croot.AddChild(tview.NewTreeNode("web"))

	for key, want := range map[string]string{
		"configs":    "nginx.conf\t0\t2.048kB",
		"nodes":      "mgr-1 (leader)\tmanager",
		"networks":   "edge\t",
		"secrets":    "db_pw\t",
		"volumes":    "shop_data\tlocal\t1: node-a",
		"forwards":   "LOCAL\tREMOTE",
		"containers": "web",
	} {
		var b strings.Builder
		u.tab(key).yank(&b)
		if !strings.Contains(b.String(), want) {
			t.Errorf("%s copies %q, want it to contain %q", key, b.String(), want)
		}
	}
}

// startSearch only opens over a tab that can be filtered, and the bar is
// pointed at that tab's own query.
func TestSearchOpensOverSearchableTabsOnly(t *testing.T) {
	u := tabsUI()
	u.search = tview.NewInputField()
	u.root = tview.NewFlex().AddItem(u.search, 0, 0, false)
	u.volTab().st().filter = "shop"

	u.startSearch("networks")
	if u.searchMode != "" {
		t.Fatalf("networks has no search, yet the bar opened for %q", u.searchMode)
	}
	u.startSearch("volumes")
	if u.search.GetText() != "shop" || u.searchTab() == nil {
		t.Errorf("bar = %q, want the volumes filter", u.search.GetText())
	}
	u.filter = "web"
	u.startSearch("containers")
	if u.search.GetText() != "web" {
		t.Errorf("bar = %q, want the tree filter", u.search.GetText())
	}
}
