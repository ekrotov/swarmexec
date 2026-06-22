package cli

import (
	"bytes"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/hinshun/vt10x"
)

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
