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
	focus   tview.Primitive // what show focused, to give it back after a switch
	release func()          // the refresh entry
	restore func()          // the footer, if this overlay took it
	once    sync.Once
}

// sessionPages are the overlays that belong to the operator's session rather
// than to the cluster on screen, and so stay open across a cluster switch:
// ssh's questions (one may be for the very connection being switched to), the
// client's own log, the key help, the docker-context dialogs and a forward's
// detail (forwards outlive a switch by design). Everything else shows or edits
// ONE cluster and is closed when the operator leaves it — see
// closeClusterOverlays.
var sessionPages = map[string]bool{
	pageAskpass:   true,
	pageSSHAdd:    true,
	pageLogView:   true,
	pageHelp:      true,
	pageCtxForm:   true,
	pageCtxDetail: true,
	pageFwdDetail: true,
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
	o.focus = focus
	o.u.trackOverlay(o)
	if focus != nil {
		o.u.app.SetFocus(focus)
	}
	return o
}

// trackOverlay records o as on screen, newest last. A page of the same name
// that was already up has just been replaced by AddPage, so its entry goes.
// UI goroutine only, like the pages themselves.
func (u *ui) trackOverlay(o *overlay) {
	kept := u.shown[:0]
	for _, s := range u.shown {
		if s.name != o.name && s != o {
			kept = append(kept, s)
		}
	}
	u.shown = append(kept, o)
}

func (u *ui) untrackOverlay(o *overlay) {
	for i, s := range u.shown {
		if s == o {
			u.shown = append(u.shown[:i], u.shown[i+1:]...)
			return
		}
	}
}

// closeClusterOverlays closes every open overlay that belongs to the cluster
// being left, newest first so each footer unwinds to its owner, and reports
// how many it closed. UI goroutine only.
//
// This is the other half of async's staleness rule. A background result for
// the old cluster is dropped after a switch; if the dialog or progress overlay
// it would have closed were still up, it would sit over the new cluster with
// nothing left to take it down — and while up it would go on editing the old
// cluster from the new one's screen. Closing them at the switch removes both.
// Close does not hand focus back here: the switch focuses the new tab itself.
func (u *ui) closeClusterOverlays() int {
	n := 0
	for i := len(u.shown) - 1; i >= 0; i-- {
		if i >= len(u.shown) {
			continue // a Close above closed more than itself
		}
		o := u.shown[i]
		if sessionPages[o.name] {
			continue
		}
		o.back = nil
		o.Close()
		n++
	}
	return n
}

// refocusTopOverlay gives focus back to the newest overlay still open (a
// session overlay a switch kept), after the switch focused the tab beneath it.
func (u *ui) refocusTopOverlay() {
	if len(u.shown) == 0 {
		return
	}
	if top := u.shown[len(u.shown)-1]; top.focus != nil {
		u.app.SetFocus(top.focus)
	}
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
		o.u.untrackOverlay(o)
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
