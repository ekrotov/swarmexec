// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/hinshun/vt10x"
	"github.com/rivo/tview"
)

func TestCtrlCCapture(t *testing.T) {
	ctrlC := tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	other := tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModNone)

	// Shell focused: forwarded as a FRESH Ctrl-C event (a different pointer, so
	// tview skips its Stop()), still Ctrl-C so encodeKey turns it into ^C.
	if got := ctrlCCapture(ctrlC, &terminalView{}); got == nil || got == ctrlC || got.Key() != tcell.KeyCtrlC {
		t.Errorf("in shell: want a fresh Ctrl-C event, got %v", got)
	}
	// Not in a shell: swallowed (nil → tview does not stop the app).
	if got := ctrlCCapture(ctrlC, tview.NewBox()); got != nil {
		t.Errorf("outside shell: want nil (swallowed), got %v", got)
	}
	// No focus at all is treated as "not a shell" → swallowed.
	if got := ctrlCCapture(ctrlC, nil); got != nil {
		t.Errorf("nil focus: want nil, got %v", got)
	}
	// Any non-Ctrl-C event passes through unchanged.
	if got := ctrlCCapture(other, tview.NewBox()); got != other {
		t.Errorf("non-ctrl-c should pass through unchanged, got %v", got)
	}
}

func TestEncodeKey(t *testing.T) {
	cases := []struct {
		name string
		ev   *tcell.EventKey
		want []byte
	}{
		{"rune", tcell.NewEventKey(tcell.KeyRune, 'a', tcell.ModNone), []byte("a")},
		{"alt-rune", tcell.NewEventKey(tcell.KeyRune, 'a', tcell.ModAlt), []byte{0x1b, 'a'}},
		{"enter", tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), []byte{'\r'}},
		{"tab", tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone), []byte{'\t'}},
		{"esc", tcell.NewEventKey(tcell.KeyEsc, 0, tcell.ModNone), []byte{0x1b}},
		{"backspace", tcell.NewEventKey(tcell.KeyBackspace2, 0, tcell.ModNone), []byte{0x7f}},
		{"up", tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone), []byte("\x1b[A")},
		{"left", tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModNone), []byte("\x1b[D")},
		{"home", tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone), []byte("\x1b[H")},
		{"pgup", tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone), []byte("\x1b[5~")},
		{"ctrl-c", tcell.NewEventKey(tcell.KeyCtrlC, 0, tcell.ModCtrl), []byte{0x03}},
		{"ctrl-d", tcell.NewEventKey(tcell.KeyCtrlD, 0, tcell.ModCtrl), []byte{0x04}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := encodeKey(c.ev); !bytes.Equal(got, c.want) {
				t.Errorf("encodeKey = %v, want %v", got, c.want)
			}
		})
	}
}

func TestVTColor(t *testing.T) {
	if vtColor(vt10x.DefaultFG) != tcell.ColorDefault {
		t.Error("DefaultFG should map to ColorDefault")
	}
	if vtColor(vt10x.DefaultBG) != tcell.ColorDefault {
		t.Error("DefaultBG should map to ColorDefault")
	}
	if got, want := vtColor(vt10x.Color(1)), tcell.PaletteColor(1); got != want {
		t.Errorf("palette color = %v, want %v", got, want)
	}
	if got, want := vtColor(vt10x.Color(0xFF8800)), tcell.NewRGBColor(0xFF, 0x88, 0x00); got != want {
		t.Errorf("rgb color = %v, want %v", got, want)
	}
}
