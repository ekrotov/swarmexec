package cli

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/config"
)

// This file centralises the TUI's visual theme: the border glyphs and the
// global tview palette. It is a deliberate, single-place styling layer so the
// whole app shares one look instead of each overlay inheriting tview's stock
// black/white/double-border defaults.
//
// A terminal draws on a fixed cell grid, so this can only go so far: no soft
// shadows, no anti-aliased curves, no sub-cell padding. What it *can* do —
// rounded thin borders, a calm dark palette, an accent colour — is what a
// terminal UI has to work with, and it already moves the feel a long way.

// Palette holds the handful of colours the theme is built from. Kept as named
// values so the look is tweakable in one spot.
var palette = struct {
	bg       tcell.Color // window background
	panel    tcell.Color // inputs, buttons, contrasting panels
	panelHi  tcell.Color // even more contrast (e.g. selected dropdown row)
	border   tcell.Color // box borders — muted, not stark white
	accent   tcell.Color // titles and highlights (the teal/cyan)
	text     tcell.Color // primary text — soft white, not pure #fff
	textDim  tcell.Color // secondary/label text
	textFade tcell.Color // tertiary/notes text
}{
	bg:       tcell.NewRGBColor(13, 17, 23),    // #0d1117
	panel:    tcell.NewRGBColor(31, 41, 55),    // #1f2937
	panelHi:  tcell.NewRGBColor(45, 55, 72),    // #2d3748
	border:   tcell.NewRGBColor(63, 74, 90),    // #3f4a5a — soft slate
	accent:   tcell.NewRGBColor(45, 212, 191),  // #2dd4bf — teal
	text:     tcell.NewRGBColor(230, 237, 243), // #e6edf3
	textDim:  tcell.NewRGBColor(148, 163, 184), // #94a3b8
	textFade: tcell.NewRGBColor(100, 116, 139), // #64748b
}

// backdropDim is the active backdrop fade amount (see dimBehind), set by
// applyTheme from config. It defaults to the built-in value so the overlay
// wrappers behave sensibly even if applyTheme has not run (e.g. in tests).
var backdropDim = config.DefaultUIDim

// applyTheme installs the global theme and the configured backdrop-dim amount.
// It mutates process-global tview state (tview.Styles is read when primitives
// are constructed; tview.Borders is read on every draw), so it must run BEFORE
// any primitive is created — i.e. at the very top of the UI entrypoint. It is
// idempotent, so a context-switch restart calling it again is harmless. dim is
// clamped to [0,1].
func applyTheme(dim float64) {
	backdropDim = clampUnit(dim)

	tview.Styles = tview.Theme{
		PrimitiveBackgroundColor:    palette.bg,
		ContrastBackgroundColor:     palette.panel,
		MoreContrastBackgroundColor: palette.panelHi,
		BorderColor:                 palette.border,
		TitleColor:                  palette.accent,
		GraphicsColor:               palette.border,
		PrimaryTextColor:            palette.text,
		SecondaryTextColor:          palette.textDim,
		TertiaryTextColor:           palette.textFade,
		InverseTextColor:            palette.bg,
		ContrastSecondaryTextColor:  palette.accent,
	}

	// Rounded, thin borders everywhere. Crucially the *focus* variants use the
	// same thin rounded glyphs instead of tview's stock double lines — that
	// heavy ═║ box around the focused overlay is the single ugliest thing in
	// the current look.
	tview.Borders.Horizontal = '─'
	tview.Borders.Vertical = '│'
	tview.Borders.TopLeft = '╭'
	tview.Borders.TopRight = '╮'
	tview.Borders.BottomLeft = '╰'
	tview.Borders.BottomRight = '╯'
	tview.Borders.LeftT = '├'
	tview.Borders.RightT = '┤'
	tview.Borders.TopT = '┬'
	tview.Borders.BottomT = '┴'
	tview.Borders.Cross = '┼'

	tview.Borders.HorizontalFocus = '─'
	tview.Borders.VerticalFocus = '│'
	tview.Borders.TopLeftFocus = '╭'
	tview.Borders.TopRightFocus = '╮'
	tview.Borders.BottomLeftFocus = '╰'
	tview.Borders.BottomRightFocus = '╯'
}

// dimBehind darkens every cell already drawn in the given rect by blending its
// foreground and background toward the theme background. Overlay wrappers call
// it before drawing their dialog, so the focused window stands out against a
// receded backdrop. A terminal has no real alpha; this simply re-styles the
// cells the page beneath already painted (tview.Pages draws that page before the
// overlay on top), which is why it must run inside the overlay's Draw.
func dimBehind(screen tcell.Screen, x, y, w, h int) {
	for cy := y; cy < y+h; cy++ {
		for cx := x; cx < x+w; cx++ {
			r, comb, style, cw := screen.GetContent(cx, cy)
			if cw == 0 {
				continue // continuation cell of a wide rune; its primary cell restyles it
			}
			fg, bg, _ := style.Decompose()
			dimmed := tcell.StyleDefault.
				Foreground(blendToward(fg, palette.text, palette.bg)).
				Background(blendToward(bg, palette.bg, palette.bg))
			screen.SetContent(cx, cy, r, comb, dimmed)
		}
	}
}

// blendToward mixes c toward target by backdropDim. A ColorDefault input (no
// known RGB) is treated as fallback first, since the terminal's real default is
// opaque to us but is, by construction, our own theme fg/bg.
func blendToward(c, fallback, target tcell.Color) tcell.Color {
	if c == tcell.ColorDefault {
		c = fallback
	}
	cr, cg, cb := c.TrueColor().RGB()
	tr, tg, tb := target.TrueColor().RGB()
	lerp := func(a, b int32) int32 { return a + int32(float64(b-a)*backdropDim) }
	return tcell.NewRGBColor(lerp(cr, tr), lerp(cg, tg), lerp(cb, tb))
}

// clampUnit clamps x to the [0,1] range.
func clampUnit(x float64) float64 {
	switch {
	case x < 0:
		return 0
	case x > 1:
		return 1
	default:
		return x
	}
}
