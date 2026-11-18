// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/client/internal/clusterread"
	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/pb"
)

// Pins for the review Q-01 round-4 splits: the node label editor, the
// global tab keys, the node detail and the network members list, driven
// before they were broken into helpers so the split is checked against them.

func escKey() *tcell.EventKey { return tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone) }

// listItems is every main text of l, in order.
func listItems(l *tview.List) []string {
	out := make([]string, l.GetItemCount())
	for i := range out {
		out[i], _ = l.GetItemText(i)
	}
	return out
}

// frontInner is the primitive the front overlay floats.
func frontInner(t *testing.T, u *ui) tview.Primitive {
	t.Helper()
	_, p := u.pages.GetFrontPage()
	box, ok := p.(*centeredBox)
	if !ok {
		t.Fatalf("front page is %T, not an overlay", p)
	}
	return box.inner
}

// answerLabelPrompt types txt into the open label prompt and ends it with k.
func answerLabelPrompt(t *testing.T, u *ui, txt string, k tcell.Key) {
	t.Helper()
	if !u.pages.HasPage(pageNodeLabelPrompt) {
		t.Fatal("the label prompt is not open")
	}
	in, ok := frontInner(t, u).(*tview.InputField)
	if !ok {
		t.Fatal("the label prompt has no input field")
	}
	in.SetText(txt)
	in.InputHandler()(tcell.NewEventKey(k, 0, tcell.ModNone), func(tview.Primitive) {})
}

// The label editor: a adds, e edits the row under the cursor, d deletes it,
// an empty, cancelled or malformed entry changes nothing, w asks before
// applying, and Esc closes.
func TestNodeLabelEditorKeys(t *testing.T) {
	u := tabsUI()
	u.editNodeLabels(clusterread.NodeInfo{Hostname: "node-a", Labels: map[string]string{"zone": "a"}}, u.nodeTab().table, nil)
	if !u.pages.HasPage(pageNodeLabels) {
		t.Fatal("the editor did not open")
	}
	list, ok := frontInner(t, u).(*tview.List)
	if !ok {
		t.Fatal("the editor is not a list")
	}
	send := func(ev *tcell.EventKey) *tcell.EventKey { return list.GetInputCapture()(ev) }
	want := func(items ...string) {
		t.Helper()
		if got := listItems(list); strings.Join(got, "|") != strings.Join(items, "|") {
			t.Errorf("rows = %q, want %q", got, items)
		}
	}
	want("zone=a")

	send(rune_('a'))
	answerLabelPrompt(t, u, "env=prod", tcell.KeyEnter)
	want("zone=a", "env=prod")

	send(rune_('a'))
	answerLabelPrompt(t, u, "  ", tcell.KeyEnter) // empty: nothing added
	send(rune_('a'))
	answerLabelPrompt(t, u, "x=y", tcell.KeyEscape) // cancelled
	want("zone=a", "env=prod")
	if u.pages.HasPage(pageNodeLabelPrompt) {
		t.Error("the prompt must close on Esc")
	}

	send(rune_('a'))
	answerLabelPrompt(t, u, "=bad", tcell.KeyEnter)
	if !u.pages.HasPage(pageInfo) {
		t.Error("a malformed label must say why")
	}
	u.pages.RemovePage(pageInfo)
	want("zone=a", "env=prod")

	list.SetCurrentItem(1)
	send(rune_('e'))
	answerLabelPrompt(t, u, "env=dev", tcell.KeyEnter)
	want("zone=a", "env=dev")

	list.SetCurrentItem(0)
	send(rune_('d'))
	want("env=dev")
	send(rune_('d'))
	want("[gray](no labels)[-]")
	send(rune_('d')) // on the placeholder: nothing to delete
	send(rune_('e')) // nor to edit
	if u.pages.HasPage(pageNodeLabelPrompt) {
		t.Error("e on the placeholder must not prompt")
	}
	want("[gray](no labels)[-]")

	if ev := send(rune_('j')); ev == nil || ev.Key() != tcell.KeyDown {
		t.Error("j must move down like the other lists")
	}

	send(rune_('w'))
	if !u.pages.HasPage(pageConfirm) {
		t.Error("w must ask before applying")
	}
	u.pages.RemovePage(pageConfirm)
	// Ctrl-S is the same apply (T-12).
	send(tcell.NewEventKey(tcell.KeyCtrlS, 0, tcell.ModCtrl))
	if !u.pages.HasPage(pageConfirm) {
		t.Error("Ctrl-S must ask before applying")
	}
	u.pages.RemovePage(pageConfirm)

	if send(escKey()) != nil || u.pages.HasPage(pageNodeLabels) {
		t.Error("Esc must close the editor")
	}
}

// Applying the labels goes through a confirm; a manager that refuses the
// update says so and leaves the editor open.
func TestNodeLabelEditorApplyFailure(t *testing.T) {
	fs := editSwarm()
	fs.Fail = map[string]bool{"POST /nodes/n1/update": true}
	u, _, _ := editUI(t, fs)
	onLoop(t, u, func() { u.editNodeLabels(clusterread.NodeInfo{ID: "n1", Hostname: "node-a"}, u.ctree, nil) })
	waitFront(t, u, pageNodeLabels)
	keyRune(t, u, 'w')
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageInfo)
	if text := drawFront(t, u, 200, 40); !strings.Contains(text, "update failed") {
		t.Errorf("notice:\n%s", text)
	}
	if !pageOpen(t, u, pageNodeLabels) {
		t.Error("a failed update must leave the editor open")
	}
}

// A confirmed apply writes the edited labels onto the node, from its current
// version, closes the editor, says so and runs the caller's refresh.
func TestNodeLabelEditorApplies(t *testing.T) {
	u, ls, _ := editUI(t, editSwarm())
	refreshed := make(chan struct{}, 1)
	onLoop(t, u, func() {
		u.editNodeLabels(clusterread.NodeInfo{ID: "n1", Hostname: "node-a", Labels: map[string]string{"zone": "a"}},
			u.ctree, func() { refreshed <- struct{}{} })
	})
	waitFront(t, u, pageNodeLabels)
	keyRune(t, u, 'd') // zone=a goes
	keyRune(t, u, 'a')
	waitFront(t, u, pageNodeLabelPrompt)
	onLoop(t, u, func() { frontInner(t, u).(*tview.InputField).SetText("rack=r7") })
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageNodeLabels)
	keyRune(t, u, 'w')
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageInfo)
	if text := drawFront(t, u, 200, 40); !strings.Contains(text, "node node-a labels updated") {
		t.Errorf("notice:\n%s", text)
	}
	if pageOpen(t, u, pageNodeLabels) {
		t.Error("a successful update must close the editor")
	}
	select {
	case <-refreshed:
	case <-time.After(5 * time.Second):
		t.Error("the caller's refresh did not run")
	}
	ls.mu.Lock()
	n1 := ls.fs.Nodes[0]
	ls.mu.Unlock()
	if len(n1.Spec.Labels) != 1 || n1.Spec.Labels["rack"] != "r7" || n1.Spec.Role != swarm.NodeRoleManager || n1.Version.Index != 1 {
		t.Errorf("node n1 = %+v (version %d)", n1.Spec, n1.Version.Index)
	}
}

// The keys every tab shares: help, the log viewer, Tab and the tab numbers,
// the keymap's global keys, j/k as arrows, and anything else passed on.
func TestTabKeysShared(t *testing.T) {
	u := chromeUI(t) // the window built, so a tab can be switched to
	var ev *tcell.EventKey
	send := func(e *tcell.EventKey) *tcell.EventKey {
		onLoop(t, u, func() { ev = u.tabKeys(e) })
		return ev
	}
	has := func(page string) (ok bool) { onLoop(t, u, func() { ok = u.pages.HasPage(page) }); return }
	active := func() (a string) { onLoop(t, u, func() { a = u.active }); return }
	onLoop(t, u, func() { u.runCtx = t.Context(); u.setTab(uiTabList[0].key) })

	if send(rune_('?')) != nil || !has(pageHelp) {
		t.Error("? must open the help")
	}
	onLoop(t, u, func() { u.pages.RemovePage(pageHelp) })

	if send(tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone)) != nil || active() != uiTabList[1].key {
		t.Errorf("Tab must go to the next tab, active = %q", active())
	}
	onLoop(t, u, func() { u.setTab(uiTabList[len(uiTabList)-1].key) })
	send(tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone))
	if active() != uiTabList[0].key {
		t.Errorf("Tab on the last tab must wrap to the first, active = %q", active())
	}
	if send(rune_('3')) != nil || active() != uiTabList[2].key {
		t.Errorf("3 must go to the third tab, active = %q", active())
	}
	if len(uiTabList) < 9 {
		if e := send(rune_('9')); e == nil || e.Rune() != '9' {
			t.Error("a number past the last tab must be passed on")
		}
	}

	var mouse, same bool
	onLoop(t, u, func() { mouse = u.mouseEnabled })
	send(rune_(u.km.ToggleMouse))
	onLoop(t, u, func() { same = u.mouseEnabled == mouse })
	if ev != nil || same {
		t.Error("the mouse key must toggle the mouse")
	}
	var side bool
	send(rune_(u.km.CtxFocus))
	onLoop(t, u, func() { side = u.sidebarFocused(); u.blurSidebar() })
	if ev != nil || !side {
		t.Error("the context key must focus the sidebar")
	}
	if send(rune_(u.km.Refresh)) != nil {
		t.Error("the refresh key must be handled")
	}

	if e := send(rune_('j')); e == nil || e.Key() != tcell.KeyDown {
		t.Error("j must come back as Down")
	}
	if e := send(rune_('k')); e == nil || e.Key() != tcell.KeyUp {
		t.Error("k must come back as Up")
	}
	for _, e := range []*tcell.EventKey{rune_('Z'), tcell.NewEventKey(tcell.KeyF5, 0, tcell.ModNone)} {
		if send(e) != e {
			t.Errorf("%v is not a shared key and must be passed on", e.Name())
		}
	}

	if send(rune_('`')) != nil || !has(pageLogView) {
		t.Error("` must open the log viewer")
	}
	send(rune_('`'))
	if has(pageLogView) {
		t.Error("` again must close the log viewer")
	}

	// Copy and quit on a session without a screen or a loop: both handled,
	// neither has anything to act on.
	b := tabsUI()
	if b.tabKeys(rune_(b.km.Copy)) != nil || b.tabKeys(rune_(b.km.Quit)) != nil {
		t.Error("the copy and quit keys must be handled")
	}
}

// openNodeDetail opens node-a's detail on a running UI whose agent answers
// the reclaim stats, and returns its text view.
func openNodeDetail(t *testing.T, u *ui) *tview.TextView {
	t.Helper()
	onLoop(t, u, func() {
		u.nodeTab().showDetail(clusterread.NodeInfo{Hostname: "node-a", Labels: map[string]string{"zone": "a"}})
	})
	waitFront(t, u, pageNodeDetail)
	var tv *tview.TextView
	onLoop(t, u, func() { tv, _ = frontInner(t, u).(*tview.TextView) })
	if tv == nil {
		t.Fatal("the node detail is not a text view")
	}
	return tv
}

// The node detail: the head and the reclaim block once the agent answers;
// j/k scroll, M measures again (m stays the mouse toggle), and the action keys close the detail and
// open their dialog. Esc, q and i close it.
func TestNodeDetailKeys(t *testing.T) {
	u, _, _ := editUIWith(t, editSwarm(), map[string]pb.AgentServer{"node-a": &pruneAgent{stats: statsFixture()}})
	tv := openNodeDetail(t, u)
	text := func() (s string) { onLoop(t, u, func() { s = tv.GetText(true) }); return }
	if !strings.Contains(text(), "hostname") || !strings.Contains(text(), "zone=a") {
		t.Errorf("detail head missing:\n%s", text())
	}
	var ev *tcell.EventKey
	onLoop(t, u, func() { ev = tv.GetInputCapture()(rune_('j')) })
	if ev == nil || ev.Key() != tcell.KeyDown {
		t.Error("j must scroll down")
	}
	onLoop(t, u, func() { ev = tv.GetInputCapture()(rune_('k')) })
	if ev == nil || ev.Key() != tcell.KeyUp {
		t.Error("k must scroll up")
	}
	for _, r := range []rune{'Z', 'm', 'r'} {
		other := rune_(r)
		onLoop(t, u, func() { ev = tv.GetInputCapture()(other) })
		if ev != other {
			t.Errorf("%c must be passed on", r)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for strings.Contains(text(), "asking the agent") {
		if time.Now().After(deadline) {
			t.Fatal("the reclaim stats never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(text(), "reclaimable") {
		t.Errorf("no reclaim block:\n%s", text())
	}
	keyRune(t, u, 'M')
	if !strings.Contains(text(), "reclaimable") {
		t.Errorf("M must keep the reclaim block while it measures:\n%s", text())
	}
	keyRune(t, u, 'M') // while measuring: ignored

	for _, c := range []struct {
		ev   *tcell.EventKey
		page string // the page it opens, "" for none
	}{
		{escKey(), ""},
		{rune_(u.km.Quit), ""},
		{rune_('i'), ""},
		{rune_('l'), pageNodeLabels},
		{rune_(u.km.NodePrune), pageNodePrune},
		{rune_(u.km.NodePruneSched), ""},
	} {
		openNodeDetail(t, u)
		key(t, u, c.ev)
		if c.page != "" {
			waitFront(t, u, c.page) // the prune dialog opens once its stats are in
		}
		var open bool
		onLoop(t, u, func() {
			open = u.pages.HasPage(pageNodeDetail)
			u.pages.RemovePage(pageNodeLabels)
			u.pages.RemovePage(pageNodePrune)
		})
		if open {
			t.Errorf("%s must close the detail", c.ev.Name())
		}
	}
}

// The members list, top to bottom: the network's labels, the services with
// their alias count (+ folded, - unfolded, the aliases under an unfolded
// one) and containers in aligned columns, then the per-node endpoint block.
// Each row maps to the service it belongs to.
func TestNetMembersRender(t *testing.T) {
	u := tabsUI()
	n := swarmops.Network{ID: "id", Name: "net", Labels: map[string]string{"team": "x"}}
	v := &netMembersView{u: u, n: n, list: tview.NewList(), expanded: map[string]bool{}}

	v.render()
	if got := listItems(v.list); strings.Join(got, "|") != "[gray]labels[-]|    team=x||"+loadingText {
		t.Errorf("before the lookup: %q", got)
	}
	v.loaded = true
	v.render()
	if got := listItems(v.list); got[len(got)-1] != "(no services attached)" {
		t.Errorf("nothing attached: %q", got)
	}

	v.n.Labels = nil
	// swarmops.netContainer is unexported; a struct of the same shape is
	// assignable to it.
	type ctr = struct{ ID, Node, IPv4 string }
	api := swarmops.NetService{Name: "api", Aliases: []string{"a1", "a2"}}
	api.Containers = append(api.Containers, ctr{ID: "c1", Node: "node-a", IPv4: "10.0.0.2"}, ctr{ID: "c22", IPv4: "10.0.0.3"})
	v.members = []swarmops.NetService{api, {Name: "db", Aliases: []string{"d"}}, {Name: "web"}}
	v.expanded["api"] = true
	v.loaded = false
	v.render()
	if got := listItems(v.list); got[0] != "api …" || got[1] != "    c1   node-a  10.0.0.2" {
		t.Errorf("before the lookup: %q", got)
	}
	v.loaded = true
	v.list.SetCurrentItem(2)
	v.render()
	want := []string{
		"api (2)  [aqua]- 2 aliases[-]",
		"        [gray]alias:[-] a1",
		"        [gray]alias:[-] a2",
		"    c1   node-a  10.0.0.2",
		"    c22  -       10.0.0.3",
		"db (0)  [aqua]+ 1 aliases[-]",
		"web (0)  [gray]no aliases[-]",
	}
	if got := listItems(v.list); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := strings.Join(v.rowSvc, ","); got != "api,api,api,api,api,db,web" {
		t.Errorf("rowSvc = %s", got)
	}
	if v.list.GetCurrentItem() != 2 {
		t.Errorf("the cursor moved to %d", v.list.GetCurrentItem())
	}

	v.swarmNet = true
	u.netTab().endpoints().byNet = map[string]*clusterread.NetEndpoints{"id": {Nodes: []clusterread.NodeEndpoints{{Node: "node-a", State: clusterread.NodeEndpointsOK}}}}
	v.render()
	got := listItems(v.list)
	if len(got) != len(want)+3 || !strings.Contains(got[len(got)-1], "node-a") || v.rowSvc[len(v.rowSvc)-1] != "" {
		t.Errorf("endpoint block: %q / %q", got, v.rowSvc)
	}
}

// openMembers opens the members overlay of the fake swarm's "front" network
// (services attached, one label) and waits for the member lookup.
func openMembers(t *testing.T, u *ui, services ...string) *netMembersView {
	t.Helper()
	n := swarmops.Network{ID: "net-front", Name: "front", Scope: "local", Services: services, Labels: map[string]string{"team": "x"}}
	onLoop(t, u, func() { u.netTab().showMembers(n) })
	waitFront(t, u, pageNetMembers)
	var v *netMembersView
	onLoop(t, u, func() { v = u.netTab().members })
	deadline := time.Now().Add(10 * time.Second)
	for loaded := false; !loaded; {
		onLoop(t, u, func() { loaded = v.loaded })
		if time.Now().After(deadline) {
			t.Fatal("the member lookup never landed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return v
}

// The members overlay's keys: Enter folds a service's aliases, A edits them
// (not on a row outside a service, not before the lookup), a and d prompt
// for a service (d only when one is attached), j moves, and Esc / q / i
// close.
func TestNetMembersKeys(t *testing.T) {
	u, _, _ := editUI(t, editSwarm())
	v := openMembers(t, u, "web")
	at := func(row int) { onLoop(t, u, func() { v.list.SetCurrentItem(row) }) }
	svcRow := 3 // labels header, label, blank, then web

	at(svcRow)
	keyCode(t, u, tcell.KeyEnter)
	var expanded bool
	onLoop(t, u, func() { expanded = v.expanded["web"] })
	if !expanded {
		t.Error("Enter on a service must unfold its aliases")
	}
	at(0)
	keyCode(t, u, tcell.KeyEnter) // the labels header: no service to fold
	keyRune(t, u, 'A')
	if frontPage(t, u) != pageNetMembers {
		t.Errorf("A outside a service opened %q", frontPage(t, u))
	}

	at(svcRow)
	onLoop(t, u, func() { v.loaded = false })
	keyRune(t, u, 'A')
	waitFront(t, u, pageInfo)
	onLoop(t, u, func() { u.pages.RemovePage(pageInfo); v.loaded = true; u.app.SetFocus(v.list) })
	keyRune(t, u, 'A')
	waitFront(t, u, pageListEdit)
	onLoop(t, u, func() { u.pages.RemovePage(pageListEdit); u.app.SetFocus(v.list) })

	for _, r := range []rune{'a', 'd'} {
		keyRune(t, u, r)
		waitFront(t, u, pageSvcPrompt)
		keyCode(t, u, tcell.KeyEscape)
		waitFront(t, u, pageNetMembers)
	}

	var ev *tcell.EventKey
	onLoop(t, u, func() { ev = v.handleKey(rune_('j')) })
	if ev == nil || ev.Key() != tcell.KeyDown {
		t.Error("j must move down")
	}
	keyCode(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageNetMembers)

	for _, r := range []rune{u.km.Quit, 'i'} {
		w := openMembers(t, u)
		keyRune(t, u, 'd') // nothing attached: says so
		waitFront(t, u, pageInfo)
		onLoop(t, u, func() { u.pages.RemovePage(pageInfo); u.app.SetFocus(w.list) })
		keyRune(t, u, r)
		waitPageGone(t, u, pageNetMembers)
	}
}
