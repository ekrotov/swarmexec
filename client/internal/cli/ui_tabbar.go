package cli

import (
	"strconv"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// tabStrip is the top-of-screen tab bar. It replaces the plain TextView the tab
// row used to be, adding two things the TextView could not do:
//
//   - Mouse: clicking a tab switches to it, so the tabs are interactive like the
//     rows beneath them (previously only the number keys / Tab switched tabs).
//   - A modern look: a thin accent underline marks the active tab, over a faint
//     baseline that separates the strip from the content — instead of the dated
//     filled black-on-teal block.
//
// It occupies two rows: the labels on top and the indicator line below. Each tab
// reads "Label N" where N is its 1-based shortcut digit, dimmed.
type tabStrip struct {
	*tview.Box
	tabs     []uiTab
	active   string
	onSelect func(key string)
	spans    []tabSpan // per-tab click hit-boxes on the label row, recomputed each Draw
}

// tabSpan is one tab's clickable column range (inclusive) on the label row.
type tabSpan struct {
	key    string
	x0, x1 int
}

func newTabStrip(tabs []uiTab, onSelect func(key string)) *tabStrip {
	return &tabStrip{Box: tview.NewBox(), tabs: tabs, onSelect: onSelect}
}

// tabCellWidth is the chrome around a tab's text: a leading space, a space
// before the shortcut digit, the digit, and a trailing space.
const tabCellWidth = 4

// stripWidth is the number of columns the strip needs to render labels in full,
// including the leading inset and the gap between tabs. Measuring and drawing
// share it so the fit test cannot drift from what Draw actually paints.
func stripWidth(labels []string) int {
	w := 1 // left inset
	for _, l := range labels {
		w += len(l) + tabCellWidth + 1 // + the gap that follows each tab
	}
	return w
}

// labelsFor picks the label set that fits the given width: the full names when
// there is room, otherwise the short forms. The short set is used even when it
// does not fit either — it is still the best available, and Draw clips.
func (t *tabStrip) labelsFor(width int) []string {
	full := make([]string, len(t.tabs))
	short := make([]string, len(t.tabs))
	for i, tab := range t.tabs {
		full[i] = tab.label
		s := tab.short
		if s == "" {
			s = tab.label
		}
		short[i] = s
	}
	if stripWidth(full) <= width {
		return full
	}
	return short
}

// setActive marks which tab is current; the strip redraws with it highlighted.
func (t *tabStrip) setActive(key string) { t.active = key }

func (t *tabStrip) Draw(screen tcell.Screen) {
	x, y, w, h := t.GetRect()
	if w <= 0 || h <= 0 {
		return
	}

	// Paint our own background so gaps and trailing space match the theme.
	base := tcell.StyleDefault.Background(palette.bg)
	fill := base.Foreground(palette.bg)
	for cy := y; cy < y+h; cy++ {
		for cx := x; cx < x+w; cx++ {
			screen.SetContent(cx, cy, ' ', nil, fill)
		}
	}

	// A faint baseline under the whole strip, separating it from the content.
	underY := y + 1
	hasUnderline := h >= 2
	if hasUnderline {
		line := base.Foreground(palette.border)
		for cx := x; cx < x+w; cx++ {
			screen.SetContent(cx, underY, tview.Borders.Horizontal, nil, line)
		}
	}

	t.spans = t.spans[:0]
	labels := t.labelsFor(w)
	col := x + 1 // small left inset
	for i, tab := range t.tabs {
		active := tab.key == t.active
		lblFg, numFg := palette.textDim, palette.textFade
		if active {
			lblFg, numFg = palette.accent, palette.accent
		}
		start := col
		col = drawRunes(screen, col, y, " ", base)
		col = drawRunes(screen, col, y, labels[i], base.Foreground(lblFg).Bold(active))
		col = drawRunes(screen, col, y, " ", base)
		col = drawRunes(screen, col, y, strconv.Itoa(i+1), base.Foreground(numFg))
		col = drawRunes(screen, col, y, " ", base)
		end := col - 1
		t.spans = append(t.spans, tabSpan{key: tab.key, x0: start, x1: end})

		if active && hasUnderline {
			acc := base.Foreground(palette.accent)
			for cx := start; cx <= end && cx < x+w; cx++ {
				screen.SetContent(cx, underY, '━', nil, acc)
			}
		}

		col++ // gap before the next tab
		if col >= x+w {
			break
		}
	}
}

// drawRunes writes single-width runes left-to-right and returns the next column.
// The tab labels and shortcut digits are ASCII, so one rune is one cell.
func drawRunes(screen tcell.Screen, x, y int, s string, style tcell.Style) int {
	for _, r := range s {
		screen.SetContent(x, y, r, nil, style)
		x++
	}
	return x
}

// MouseHandler makes the tabs clickable: a left click on a tab's label span
// switches to it. Any stroke landing on the strip is swallowed so it doesn't
// reach the content beneath. The strip never takes keyboard focus.
func (t *tabStrip) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(p tview.Primitive)) (bool, tview.Primitive) {
	return t.WrapMouseHandler(func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(p tview.Primitive)) (bool, tview.Primitive) {
		mx, my := event.Position()
		if !t.InRect(mx, my) {
			return false, nil
		}
		if action == tview.MouseLeftDown || action == tview.MouseLeftClick {
			for _, sp := range t.spans {
				if mx >= sp.x0 && mx <= sp.x1 {
					if t.onSelect != nil && sp.key != t.active {
						t.onSelect(sp.key)
					}
					return true, nil
				}
			}
		}
		return true, nil
	})
}
