package cli

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// A left click on a tab's label must switch to that tab; a click in the empty
// area past the last tab must not select anything.
func TestTabStripClickSelectsTab(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	scr.SetSize(120, 6)

	tabs := []struct{ key, label string }{
		{"containers", "Containers"},
		{"volumes", "Volumes"},
		{"forwards", "Forwards"},
	}
	var got string
	strip := newTabStrip(tabs, func(key string) { got = key })
	strip.SetRect(0, 0, 120, 2)
	strip.Draw(scr) // computes the click hit-boxes

	if len(strip.spans) != len(tabs) {
		t.Fatalf("got %d spans, want %d", len(strip.spans), len(tabs))
	}

	click := func(x, y int) {
		ev := tcell.NewEventMouse(x, y, tcell.Button1, 0)
		strip.MouseHandler()(tview.MouseLeftClick, ev, func(tview.Primitive) {})
	}

	// Click the middle of the "Volumes" tab.
	sp := strip.spans[1]
	got = ""
	click((sp.x0+sp.x1)/2, 0)
	if got != "volumes" {
		t.Errorf("click on Volumes selected %q, want volumes", got)
	}

	// Click well past the last tab: no selection.
	got = ""
	click(strip.spans[len(strip.spans)-1].x1+20, 0)
	if got != "" {
		t.Errorf("click in empty strip area selected %q, want none", got)
	}
}

// Clicking the already-active tab must not fire onSelect (no needless re-switch).
func TestTabStripClickActiveTabIsNoop(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	scr.SetSize(120, 6)

	tabs := []struct{ key, label string }{{"containers", "Containers"}, {"volumes", "Volumes"}}
	fired := false
	strip := newTabStrip(tabs, func(string) { fired = true })
	strip.setActive("containers")
	strip.SetRect(0, 0, 120, 2)
	strip.Draw(scr)

	sp := strip.spans[0]
	ev := tcell.NewEventMouse((sp.x0+sp.x1)/2, 0, tcell.Button1, 0)
	strip.MouseHandler()(tview.MouseLeftClick, ev, func(tview.Primitive) {})
	if fired {
		t.Error("clicking the active tab fired onSelect; want a no-op")
	}
}
