// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// The client log viewer (`) floats like every other overlay, so the footer
// with its keys stays on screen; it wraps by default, because the end of an
// error line is the cause; and w turns wrapping off and on again.
func TestClientLogViewerKeepsFooterAndWraps(t *testing.T) {
	u := covChromeUI(t)
	onLoop(t, u, func() {
		u.runCtx = t.Context()
		u.openLogView()
	})
	t.Cleanup(func() { onLoop(t, u, func() { u.closeLogView() }) })

	var tv *tview.TextView
	onLoop(t, u, func() {
		box, ok := u.pages.GetPage(pageLogView).(*centeredBox)
		if !ok {
			t.Errorf("log viewer page is %T, want the centered overlay box", u.pages.GetPage(pageLogView))
			return
		}
		tv, _ = box.inner.(*tview.TextView)
	})
	if tv == nil {
		t.Fatal("no log text view inside the overlay")
	}
	footer := helpLine(t, u)
	if !strings.HasPrefix(footer, " Esc/q close") || !strings.Contains(footer, "w wrap") {
		t.Errorf("footer %q: want Esc first and the wrap key", footer)
	}

	// Whether a line longer than the box shows its end: set and draw in one
	// turn of the loop, so the viewer's own refresh cannot replace the text
	// in between.
	wrap := func() bool {
		var drawn string
		onLoop(t, u, func() {
			tv.SetText(strings.Repeat("x", 150) + " CAUSE")
			drawn = drawnText(tv, 60, 10)
		})
		return strings.Contains(drawn, "CAUSE")
	}
	if !wrap() {
		t.Error("the log viewer must wrap by default")
	}
	sendRune(t, u, 'w')
	if wrap() {
		t.Error("w must turn wrapping off")
	}
	sendRune(t, u, 'w')
	if !wrap() {
		t.Error("w again must turn it back on")
	}

	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageLogView)
}

// Both log views toggle the mouse with the CONFIGURED key, not a literal m,
// and their footers name that key: with toggle_mouse remapped to z, z
// toggles, m does nothing, and the footer says z.
func TestLogViewsUseTheConfiguredMouseKey(t *testing.T) {
	mouse := func(u *ui) (on bool) { onLoop(t, u, func() { on = u.mouseEnabled }); return }

	u := covChromeUI(t)
	onLoop(t, u, func() {
		u.km.ToggleMouse = 'z'
		u.runCtx = t.Context()
		u.openLogView()
	})
	t.Cleanup(func() { onLoop(t, u, func() { u.closeLogView() }) })
	if footer := helpLine(t, u); !strings.Contains(footer, "z mouse") {
		t.Errorf("client log footer %q does not name the mouse key", footer)
	}
	before := mouse(u)
	sendRune(t, u, 'm')
	if mouse(u) != before {
		t.Error("m must not toggle the mouse once toggle_mouse is z")
	}
	sendRune(t, u, 'z')
	if mouse(u) == before {
		t.Error("z did not toggle the mouse in the client log viewer")
	}
	if mouse(u) {
		sendRune(t, u, 'z') // the hint comes with turning it off
	}
	waitFooter(t, u, "z to re-enable")

	a := &fakeAgent{logs: []string{"hello"}}
	lu := logsUI(t, a, &fakeAgent{})
	onLoop(t, lu, func() {
		lu.km.ToggleMouse = 'z'
		lu.showLogs(webCandidate())
	})
	waitLogText(t, lu, "hello")
	if page := pageText(t, lu, pageLogs); !strings.Contains(page, "z mouse:") {
		t.Errorf("log view footer does not name the mouse key:\n%s", page)
	}
	focusLog(t, lu)
	before = mouse(lu)
	sendRune(t, lu, 'm')
	if mouse(lu) != before {
		t.Error("m must not toggle the mouse in the log view once toggle_mouse is z")
	}
	sendRune(t, lu, 'z')
	if mouse(lu) == before {
		t.Error("z did not toggle the mouse in the log view")
	}
	want := "z mouse: off"
	if mouse(lu) {
		want = "z mouse: app"
	}
	if page := pageText(t, lu, pageLogs); !strings.Contains(page, want) {
		t.Errorf("log view footer does not follow the toggle:\n%s", page)
	}
}
