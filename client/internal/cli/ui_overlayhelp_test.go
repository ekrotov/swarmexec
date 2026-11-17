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
			if tv, ok := box.inner.(*tview.TextView); ok {
				text = tv.GetText(false)
			}
		}
	})
	return text
}

// ? in an overlay opens the help with that overlay's own keys first — read
// off its footer, danger keys still red. ? again closes the
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
