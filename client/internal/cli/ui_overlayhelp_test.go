// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/internal/pb"
)

// pressViaApp delivers r the way the event loop does with the app's input
// capture in place: appKeys first, then the focused widget.
func pressViaApp(t *testing.T, u *ui, r rune) {
	t.Helper()
	onLoop(t, u, func() {
		if ev := u.appKeys(rune_(r)); ev != nil {
			u.pages.InputHandler()(ev, func(p tview.Primitive) { u.app.SetFocus(p) })
		}
	})
}

// openHelpText is the open help's unwrapped text ("" when it is not open).
func openHelpText(t *testing.T, u *ui) string {
	t.Helper()
	var text string
	onLoop(t, u, func() {
		if box, ok := u.pages.GetPage(pageHelp).(*centeredBox); ok {
			if hv, ok := box.inner.(*helpView); ok {
				text = hv.h.String()
			}
		}
	})
	return text
}

// ? in an overlay opens the help with that overlay's own keys first — read
// off its footer, danger keys still red — and Global last. ? again closes the
// help and hands the keyboard back to the overlay.
func TestQuestionMarkInAnOverlayListsItsKeysFirst(t *testing.T) {
	u, _, _ := editUIWith(t, editSwarm(), map[string]pb.AgentServer{"node-a": &pruneAgent{stats: statsFixture()}})
	tv := openNodeDetail(t, u)

	pressViaApp(t, u, '?')
	waitFront(t, u, pageHelp)
	text := openHelpText(t, u)
	if !strings.HasPrefix(text, "[aqua]This view — node node-a[-]\n") {
		t.Fatalf("the help must open on the detail's own keys:\n%s", text)
	}
	own := text[:strings.Index(text, "\n\n")]
	for _, want := range []string{"[yellow]Esc[white]", "back", "[yellow]M[white]", "measure now", "[red]P[white]", "prune…", "edit labels"} {
		if !strings.Contains(own, want) {
			t.Errorf("own section lacks %q:\n%s", want, own)
		}
	}
	if g := strings.LastIndex(text, "[aqua]"); !strings.HasPrefix(text[g:], "[aqua]Global") {
		t.Errorf("Global must be the last section:\n%s", text)
	}

	pressViaApp(t, u, '?')
	waitPageGone(t, u, pageHelp)
	if f := focusedAs[*tview.TextView](t, u); f != tv {
		t.Error("closing the help must give the keyboard back to the detail")
	}
	if !strings.Contains(footerText(u), "measure now") {
		t.Errorf("the detail's footer must come back, got %q", footerText(u))
	}
}

// Where the focus takes text, ? is a character, not a request for help: an
// input field in an overlay keeps it. The help and the vim help close on ?
// themselves, so the router leaves them alone.
func TestQuestionMarkIsTextInAnInputField(t *testing.T) {
	u := covChromeUI(t)
	in := tview.NewInputField()
	onLoop(t, u, func() {
		ov := u.overlayFor(pageSvcPrompt, nil, footerKeys("Enter", "ok", "Esc", "cancel"))
		ov.show(centeredPrompt(in, 40), in)
	})
	pressViaApp(t, u, '?')
	var got string
	var help bool
	onLoop(t, u, func() { got, help = in.GetText(), u.pages.HasPage(pageHelp) })
	if help || got != "?" {
		t.Errorf("? in an input field: text %q, help open %v", got, help)
	}

	for _, page := range []string{pageHelp, pageVimHelp} {
		var ev *tcell.EventKey
		onLoop(t, u, func() {
			ov := u.overlayFor(page, nil, footerKeys("Esc", "back"))
			tv := tview.NewTextView()
			ov.show(centered(tv, 20, 5), tv)
			ev = u.overlayHelpKey(rune_('?'))
			ov.Close()
		})
		if ev == nil {
			t.Errorf("%s: the router must leave ? to the page", page)
		}
	}
}

// Inspect wires ? itself; it lands in the same help, its own keys first.
func TestInspectQuestionMarkListsItsKeysFirst(t *testing.T) {
	u := covChromeUI(t)
	onLoop(t, u, func() {
		ov := u.overlayFor(pageInspect, nil, footerKeys("Esc", "back", "t", "view", "a", "actions"))
		tv := tview.NewTextView()
		tv.SetTitle(" inspect web ")
		ov.show(centered(tv, 40, 10), tv)
		u.showHelp()
	})
	text := openHelpText(t, u)
	if !strings.HasPrefix(text, "[aqua]This view — inspect web[-]\n") || !strings.Contains(text, "[yellow]a[white]") {
		t.Errorf("inspect's help:\n%s", text)
	}
}

func TestFooterPairs(t *testing.T) {
	got := footerPairs(" [green]✓ copied[white] · " + footerKeys("a", "attach", dangerKey("d"), "detach", "Esc", "back"))
	want := []helpEntry{{keys: "Esc", desc: "back"}, {keys: "a", desc: "attach"}, {keys: "d", desc: "detach", danger: true}}
	if len(got) != len(want) {
		t.Fatalf("pairs %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pair %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A description too long for the box wraps under itself (a hanging indent),
// not under the key column; a key wider than the column pushes its
// description right, and the continuation follows it.
func TestHelpTextHangingIndent(t *testing.T) {
	h := &helpWriter{}
	h.sec("S")
	h.line("p", "port-forward the task under the cursor and then some more words")
	h.line("CONTAINERS", "running tasks and the addresses the nodes know")
	got := h.text(40)
	want := "[aqua]S[-]\n" +
		"  [yellow]p[white]         port-forward the task under\n" +
		"            the cursor and then some\n" +
		"            more words\n" +
		"  [yellow]CONTAINERS[white] running tasks and the\n" +
		"             addresses the nodes know\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if h.String() != h.text(0) || strings.Count(h.text(0), "\n") != 3 {
		t.Errorf("unwrapped: %q", h.String())
	}
}

// The help grows with the terminal, unlike the old fixed 60×24 box, and
// still leaves the footer rows free; it re-wraps to the width it gets.
func TestHelpGrowsWithTheTerminal(t *testing.T) {
	u := tabsUI()
	u.active = "containers"
	u.showHelp()
	_, front := u.pages.GetFrontPage()
	box := front.(*centeredBox)
	hv := box.inner.(*helpView)
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	for _, size := range [][2]int{{160, 45}, {80, 24}} {
		scr.SetSize(size[0], size[1])
		box.SetRect(0, 0, size[0], size[1])
		box.Draw(scr)
		_, y, w, h := hv.GetRect()
		if size[0] == 160 && (w < 140 || h < 38) {
			t.Errorf("at 160×45 the help is %d×%d, want ~90%%", w, h)
		}
		if y+h > size[1]-overlayFooterReserve {
			t.Errorf("at %v the help covers the footer (y %d, h %d)", size, y, h)
		}
		if hv.width != w-2 {
			t.Errorf("at %v the help wrapped for %d columns, the box has %d", size, hv.width, w-2)
		}
	}
	if !strings.Contains(hv.GetText(false), "port-forward the task under the cursor\n") {
		t.Error("the p line should be the short one")
	}
}
