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

	tabs := []uiTab{
		{"containers", "Containers", "Cont"},
		{"volumes", "Volumes", "Vol"},
		{"forwards", "Forwards", "Fwd"},
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

	tabs := []uiTab{{"containers", "Containers", "Cont"}, {"volumes", "Volumes", "Vol"}}
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

// The strip falls back to short labels when the full set does not fit, so a
// narrow terminal still shows every tab instead of clipping the last ones away.
func TestTabStripLabelsAdaptToWidth(t *testing.T) {
	strip := newTabStrip(uiTabList, nil)

	full := make([]string, len(uiTabList))
	short := make([]string, len(uiTabList))
	for i, tab := range uiTabList {
		full[i], short[i] = tab.label, tab.short
	}
	needFull, needShort := stripWidth(full), stripWidth(short)
	if needShort >= needFull {
		t.Fatalf("short labels (%d cols) are not shorter than full (%d) — the fallback buys nothing", needShort, needFull)
	}

	// Roomy: full labels.
	if got := strip.labelsFor(needFull); got[0] != uiTabList[0].label {
		t.Errorf("at %d cols got %q, want the full label %q", needFull, got[0], uiTabList[0].label)
	}
	// One column short of fitting: fall back.
	if got := strip.labelsFor(needFull - 1); got[0] != uiTabList[0].short {
		t.Errorf("at %d cols got %q, want the short label %q", needFull-1, got[0], uiTabList[0].short)
	}
	// The common 80-column terminal must show every tab in short form.
	if needShort > 80 {
		t.Errorf("short labels need %d cols — they do not fit an 80-column terminal", needShort)
	}
	if got := strip.labelsFor(80); len(got) != len(uiTabList) {
		t.Errorf("got %d labels, want %d", len(got), len(uiTabList))
	}

	// Short forms must stay distinguishable — initials collide across tabs.
	seen := map[string]string{}
	for _, tab := range uiTabList {
		if tab.short == "" {
			t.Errorf("tab %q has no short label", tab.key)
			continue
		}
		if other, dup := seen[tab.short]; dup {
			t.Errorf("short label %q is used by both %q and %q", tab.short, other, tab.key)
		}
		seen[tab.short] = tab.key
	}
}

// A click must hit the right tab in the narrow layout too — the hit-boxes are
// computed from whichever label set was drawn.
func TestTabStripClickWithShortLabels(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	scr.SetSize(80, 6)

	var got string
	strip := newTabStrip(uiTabList, func(key string) { got = key })
	strip.SetRect(0, 0, 80, 2)
	strip.Draw(scr)

	if len(strip.spans) != len(uiTabList) {
		t.Fatalf("got %d hit-boxes, want %d — a tab is unreachable by mouse", len(strip.spans), len(uiTabList))
	}
	target := len(uiTabList) - 1 // the last tab, the first to be clipped when tight
	sp := strip.spans[target]
	ev := tcell.NewEventMouse((sp.x0+sp.x1)/2, 0, tcell.Button1, 0)
	strip.MouseHandler()(tview.MouseLeftClick, ev, func(tview.Primitive) {})
	if got != uiTabList[target].key {
		t.Errorf("click on the last tab selected %q, want %q", got, uiTabList[target].key)
	}
}
