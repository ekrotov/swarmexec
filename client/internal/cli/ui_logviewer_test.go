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
