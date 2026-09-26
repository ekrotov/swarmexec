// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"sync"

	"github.com/rivo/tview"
)

// An overlay is one floating page, and everything that has to be undone when it
// goes: the page itself, the entry that pauses the background refreshers, the
// footer it borrowed, and the focus it has to hand back.
//
// It exists because those four were a convention rather than a type. Fifty-odd
// call sites each opened a page and each wrote their own close — and a close
// has more exits than it looks: a button, Esc, an error return, a parent
// closing, a second overlay opening on top. Every exit had to remember all four
// steps in the right order, and the ones that forgot are the bug class this
// replaces: a footer still showing an overlay's keys after it closed, focus
// landing on the widget behind the one on screen, an entry never released.
//
// The type does not make overlays cleverer. It makes forgetting impossible.
//
// It comes in two steps because the call sites need it that way: the close is
// wired into buttons and input captures while the widgets are still being
// built, so the overlay has to exist before the thing it will show.
type overlay struct {
	u       *ui
	name    string
	back    tview.Primitive
	release func() // the refresh entry
	restore func() // the footer, if this overlay took it
	once    sync.Once
}

// overlayFor declares an overlay: its page name, what gets focus when it
// closes, and the footer keys it shows while open (empty markup leaves the
// footer alone). Nothing is on screen yet — call show.
//
// back is what Close focuses afterwards. Pass nil when the overlay hands over
// to another one that takes focus itself, so Close does not yank focus back for
// an instant on the way.
func (u *ui) overlayFor(name string, back tview.Primitive, help string) *overlay {
	o := &overlay{u: u, name: name, back: back, release: u.markOverlay(name)}
	if help != "" {
		_, o.restore = u.footerTakeover(help)
	}
	return o
}

// show puts p on screen and focuses focus (nil: leave focus where it is).
func (o *overlay) show(p tview.Primitive, focus tview.Primitive) *overlay {
	o.u.pages.AddPage(o.name, p, true, true)
	if focus != nil {
		o.u.app.SetFocus(focus)
	}
	return o
}

// setHelp updates the footer keys while the overlay is open — for the few whose
// keys change with what is selected.
func (o *overlay) setHelp(markup string) { o.u.setFooter(markup) }

// Close undoes the four steps in the order that keeps the screen honest: give
// the footer back before the page goes, take the page away before moving focus
// to whatever was behind it.
//
// Idempotent, and that is not politeness: a modal's done func and the caller
// that also tidies up both reach for it, and a second close used to pop a
// footer belonging to somebody else.
func (o *overlay) Close() {
	o.once.Do(func() {
		if o.restore != nil {
			o.restore()
		}
		o.u.pages.RemovePage(o.name)
		o.release()
		if o.back != nil {
			o.u.app.SetFocus(o.back)
		}
	})
}

// footerTakeover points the footer at markup and returns a setter (to update it
// while open) and a restore (to call on close). Each call captures the
// then-current baseline, so nested overlays unwind to the right owner.
//
// The baseline, not the widget's current text: opening an overlay while a flash
// is showing must not adopt the flash message as what to restore.
//
// It does NOT register an overlay entry — overlayFor does. The two were one
// call for as long as every overlay that took the footer also added a page,
// which is exactly the assumption that left modals uncounted.
func (u *ui) footerTakeover(markup string) (func(string), func()) {
	prev := u.footerBase
	u.setFooter(markup)
	var once sync.Once
	return u.setFooter, func() {
		once.Do(func() { u.setFooter(prev) })
	}
}
