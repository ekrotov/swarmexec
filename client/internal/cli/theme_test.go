package cli

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestClampUnit(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{-1, 0},
		{0, 0},
		{0.6, 0.6},
		{1, 1},
		{1.5, 1},
	}
	for _, c := range cases {
		if got := clampUnit(c.in); got != c.want {
			t.Errorf("clampUnit(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// applyTheme must clamp the configured dim into [0,1] before the overlay
// wrappers read it, so a stray out-of-range config value can't produce nonsense.
func TestApplyThemeClampsDim(t *testing.T) {
	orig := backdropDim
	defer func() { backdropDim = orig }()

	applyTheme(2.0)
	if backdropDim != 1 {
		t.Errorf("backdropDim = %v after applyTheme(2.0), want 1", backdropDim)
	}
	applyTheme(-0.5)
	if backdropDim != 0 {
		t.Errorf("backdropDim = %v after applyTheme(-0.5), want 0", backdropDim)
	}
}

// The backdrop dim re-styles what is already drawn and must leave the content
// alone — including a wide rune (CJK, most emoji), which spans two cells.
func TestDimBehindRestylesButKeepsContent(t *testing.T) {
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	s.SetSize(6, 1)
	plain := tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorBlack)
	s.Put(0, 0, "漢", plain)
	s.Put(2, 0, "a", plain)

	dimBehind(s, 0, 0, 6, 1)

	if str, style, w := s.Get(0, 0); str != "漢" || w != 2 || style == plain {
		t.Errorf("wide cell = %q width %d restyled=%v, want 漢, 2, true", str, w, style != plain)
	}
	if str, style, _ := s.Get(2, 0); str != "a" || style == plain {
		t.Errorf("narrow cell = %q restyled=%v, want a, true", str, style != plain)
	}
	s.Show()
	cells, _, _ := s.GetContents()
	if got := string(cells[0].Runes); got != "漢" {
		t.Errorf("rendered wide rune = %q, want 漢", got)
	}
}
