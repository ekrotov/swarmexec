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
