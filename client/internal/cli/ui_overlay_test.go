package cli

import (
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// centered() must never draw its box over the bottom overlayFooterReserve rows,
// so the shortcut footer (drawn by the page beneath) stays visible — including
// when the terminal is too short for the overlay's natural height.
func TestCenteredReservesFooter(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name             string
		screenW, screenH int
	}{
		{"roomy", 120, 50}, // box fits with space to spare
		{"tight", 110, 22}, // box (110x40) taller than the screen
		{"tiny", 80, 8},    // barely any room
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scr.SetSize(tc.screenW, tc.screenH)
			inner := tview.NewBox().SetBorder(true)
			ov := centered(inner, 110, 40) // e.g. the inspect overlay's size
			ov.SetRect(0, 0, tc.screenW, tc.screenH)
			ov.Draw(scr)

			_, iy, _, ih := inner.GetRect()
			bottom := iy + ih
			limit := tc.screenH - overlayFooterReserve
			if bottom > limit {
				t.Errorf("%s: overlay bottom row %d covers the footer (limit %d, screenH %d)",
					tc.name, bottom, limit, tc.screenH)
			}
			if ih <= 0 {
				t.Errorf("%s: overlay collapsed to height %d", tc.name, ih)
			}
		})
	}
}

// centered() must forward focus and input to its inner primitive, since callers
// focus the inner directly and Pages routes events through the wrapper.
func TestCenteredDelegatesFocusAndInput(t *testing.T) {
	inner := tview.NewInputField()
	ov := centered(inner, 40, 3)

	app := tview.NewApplication()
	app.SetFocus(inner)
	if !ov.HasFocus() {
		t.Error("centered wrapper should report focus when inner is focused")
	}
	if ov.InputHandler() == nil {
		t.Error("centered wrapper should expose the inner input handler")
	}
}

// A left-click inside the overlay area but BESIDE the centred box must be
// swallowed and keep focus on the dialog — not fall through to the page beneath
// (which would steal focus and leave the dialog's buttons unresponsive).
func TestCenteredBoxSwallowsOutsideClick(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	scr.SetSize(100, 30)

	pages := tview.NewPages()
	main := tview.NewList().AddItem("item", "", 0, nil) // grabs focus on a click
	pages.AddPage("main", main, true, true)
	form := tview.NewForm().AddButton("OK", nil)
	pages.AddPage("dlg", centered(form, 40, 7), true, true)

	pages.SetRect(0, 0, 100, 30)
	pages.Draw(scr) // sizes the pages (full-screen) and the inner centred box

	focused := tview.Primitive(form)
	setFocus := func(p tview.Primitive) { focused = p }
	// (2,2): on-screen, well outside the 40x7 box centred around the middle.
	ev := tcell.NewEventMouse(2, 2, tcell.Button1, 0)
	consumed, _ := pages.MouseHandler()(tview.MouseLeftDown, ev, setFocus)

	if !consumed {
		t.Error("outside click was not consumed by the overlay (falls through to the page beneath)")
	}
	if focused == tview.Primitive(main) {
		t.Error("outside click stole focus to the underlying page — the dialog would become unresponsive")
	}
}

// A modal overlay must swallow EVERY mouse action that lands in its (full-screen)
// area but misses the dialog — not just the left-press — or scrolling/clicking in
// the margin drives the main view beneath it (tview.Pages routes any unconsumed
// event down to the next visible page). Covers both overlay wrappers.
func TestOverlaySwallowsAllMouseActions(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	scr.SetSize(100, 30)

	overlays := []struct {
		name string
		make func() tview.Primitive
	}{
		{"centered", func() tview.Primitive { return centered(tview.NewForm().AddButton("OK", nil), 40, 7) }},
		{"scrim", func() tview.Primitive {
			return newScrim(tview.NewModal().SetText("confirm?").AddButtons([]string{"OK", "Cancel"}))
		}},
	}
	actions := []struct {
		name   string
		action tview.MouseAction
	}{
		{"scroll-up", tview.MouseScrollUp},
		{"scroll-down", tview.MouseScrollDown},
		{"left-click", tview.MouseLeftClick},
		{"right-down", tview.MouseRightDown},
		{"move", tview.MouseMove},
	}

	for _, ov := range overlays {
		for _, act := range actions {
			t.Run(ov.name+"/"+act.name, func(t *testing.T) {
				pages := tview.NewPages()
				// A recorder as the main page: it flags any mouse event that
				// reaches it. If the overlay is properly modal, it never does.
				main := &touchRecorder{Box: tview.NewBox()}
				pages.AddPage("main", main, true, true)
				pages.AddPage("dlg", ov.make(), true, true)
				pages.SetRect(0, 0, 100, 30)
				pages.Draw(scr) // full-screen the pages and position the inner box

				setFocus := func(p tview.Primitive) {}
				// (2,2): on-screen, in the margin outside any centred dialog box.
				ev := tcell.NewEventMouse(2, 2, tcell.Button1, 0)
				pages.MouseHandler()(act.action, ev, setFocus)
				if main.touched {
					t.Errorf("%s/%s: event fell through to the main view beneath the modal", ov.name, act.name)
				}
			})
		}
	}
}

// touchRecorder is a stand-in main page that records whether any mouse event
// reached it — i.e. fell through the overlay above it. It "consumes" so routing
// stops here, exactly as a real focused main view would.
type touchRecorder struct {
	*tview.Box
	touched bool
}

func (r *touchRecorder) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(p tview.Primitive)) (bool, tview.Primitive) {
	return func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(p tview.Primitive)) (bool, tview.Primitive) {
		r.touched = true
		return true, nil
	}
}
