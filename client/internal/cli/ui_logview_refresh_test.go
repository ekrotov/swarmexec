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

func logLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

// drawn lays the view out the way a frame does, which is what gives it a
// wrapped line count to measure the scroll position against.
func drawn(tv *tview.TextView) {
	scr := tcell.NewSimulationScreen("UTF-8")
	_ = scr.Init()
	scr.SetSize(40, 10)
	tv.SetRect(0, 0, 40, 10)
	tv.Draw(scr)
	scr.Fini()
}

func TestLogViewRefreshKeepsTheOperatorsScrollPosition(t *testing.T) {
	text := logLines(50)
	tv := tview.NewTextView().SetScrollable(true)
	refresh := logViewRefresher(tv, func() string { return text })
	refresh()
	drawn(tv)
	if row, _ := tv.GetScrollOffset(); row == 0 {
		t.Fatalf("the first refresh should follow the end, offset %d", row)
	}

	// The operator scrolls up to read; a tick with new lines must not undo it.
	tv.ScrollTo(5, 0)
	text = logLines(60)
	refresh()
	drawn(tv)
	if row, _ := tv.GetScrollOffset(); row != 5 {
		t.Errorf("scroll offset %d after a tick, want the operator's 5", row)
	}

	// Back at the end, the view follows again.
	tv.ScrollToEnd()
	drawn(tv)
	text = logLines(70)
	refresh()
	drawn(tv)
	row, _ := tv.GetScrollOffset()
	if _, _, _, h := tv.GetInnerRect(); row+h < tv.GetWrappedLineCount() {
		t.Errorf("a view at the end stopped following: offset %d of %d lines", row, tv.GetWrappedLineCount())
	}
}

func TestLogViewRefreshSkipsUnchangedText(t *testing.T) {
	tv := tview.NewTextView().SetScrollable(true)
	n := 0
	refresh := logViewRefresher(tv, func() string { n++; return "same" })
	refresh()
	tv.SetText("marker")
	refresh() // unchanged ring: must not touch the view
	if got := tv.GetText(true); got != "marker" {
		t.Errorf("an unchanged ring rewrote the view: %q", got)
	}
}
