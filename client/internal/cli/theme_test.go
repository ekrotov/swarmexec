package cli

import "testing"

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
