// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"github.com/rivo/tview"
)

// The contexts used to be a tab, and that was the wrong shape for them.
//
// A tab is a destination: you go there, you do something, you come back. That
// fitted while switching cluster meant tearing the session down and rebuilding
// it — an expensive, occasional act worth a trip. Since the switch became cheap
// and keeps your place (F14), it is something you do *from* wherever you are,
// several times in a session, and a destination for it reads as a detour. It
// also put the list of clusters — the thing that tells you where you are — on a
// screen you are by definition not looking at.
//
// So it is a column on the right instead: always in view, always says which
// cluster is live and which others are warm, and takes focus only when asked.

// Sidebar geometry. The width follows the longest context name rather than
// being fixed, because these are names the operator chose and truncating them
// defeats the point of showing the list at all — but it is clamped, because one
// unusually long name must not eat the tree.
const (
	sidebarMinWidth = 16
	sidebarMaxWidth = 30
	// sidebarChrome is the border (2) plus the marker column and its space (2).
	sidebarChrome = 4
	// sidebarKeepsContent is how many columns the content must be left with for
	// the sidebar to be worth its space. Below that the tree's own columns start
	// being clipped, and a narrower view of the cluster you ARE on is a bad
	// trade for a list of the ones you are not. The sidebar then hides itself
	// and reappears only while it has focus.
	sidebarKeepsContent = 76
)

// sidebarWidth is the column count the context list wants, or 0 when it should
// not be shown at this terminal size.
//
// focused overrides the size rule: pressing the context key on a narrow
// terminal has to produce a list, not nothing. It then goes away again the
// moment focus leaves, so no state has to be remembered and no second way of
// showing the same list has to exist.
func sidebarWidth(total int, names []string, focused bool) int {
	want := sidebarMinWidth
	for _, n := range names {
		if w := len(n) + sidebarChrome; w > want {
			want = w
		}
	}
	if want > sidebarMaxWidth {
		want = sidebarMaxWidth
	}
	if focused {
		// Never take so much that the content disappears entirely; on a very
		// narrow terminal the list is still the thing that was asked for.
		if want > total/2 {
			want = total / 2
		}
		return want
	}
	if total-want < sidebarKeepsContent {
		return 0
	}
	return want
}

// contextNames is what the sidebar has to fit.
func (u *ui) contextNames() []string {
	out := make([]string, 0, len(u.ctxs))
	for _, c := range u.ctxs {
		out = append(out, c.Name)
	}
	return out
}

// sidebarFocused reports whether the context list currently has the keyboard.
//
// It reads a flag we maintain rather than asking the application, and that is
// not a shortcut — it is required. tview's draw path holds the application's
// write lock for the whole frame, and Application.GetFocus takes the read lock;
// sync.RWMutex is not reentrant, so asking from inside a draw function
// deadlocks the UI on its very first frame. (It did: the terminal stayed
// blank.) The flag is only ever written on the UI goroutine, which is also the
// one that draws, so it is always the truth by the time it is read.
func (u *ui) sidebarFocused() bool { return u.sidebarHasFocus }

// resizeSidebar is called from the body's draw function, so the width tracks
// the terminal without anything having to listen for resize events: tview hands
// us the space we have, and the Flex lays its items out immediately after.
func (u *ui) resizeSidebar(width int) {
	if u.body == nil {
		return
	}
	w := sidebarWidth(width, u.contextNames(), u.sidebarFocused())
	u.body.ResizeItem(u.sidebar, w, 0)
	// The gap goes away with the sidebar; a stray blank column on a terminal
	// too narrow to show the list at all is exactly the column that terminal
	// cannot spare. ResizeItem(nil) addresses it because the spacer is the only
	// item in this Flex without a primitive.
	gap := 0
	if w > 0 {
		gap = 1
	}
	u.body.ResizeItem(nil, gap, 0)
}

// focusSidebar moves the keyboard to the context list and points the footer at
// its keys.
//
// Deliberately not an overlay: one would count as open and pause the tree's
// background refresh. The sidebar is not an overlay — the cluster behind it is
// still on screen and must keep updating, which is why this writes the footer
// with setFooter directly instead of going through overlayFor.
func (u *ui) focusSidebar() {
	if u.sidebarFocused() {
		u.blurSidebar()
		return
	}
	u.sidebarReturn = u.footerBase
	u.sidebarHasFocus = true
	u.setFooter(u.helpFor("contexts"))
	u.sidebar.SetBorderColor(palette.accent)
	u.app.SetFocus(u.cxtable)
}

// blurSidebar hands the keyboard back to the visible tab.
func (u *ui) blurSidebar() {
	if !u.sidebarFocused() {
		return
	}
	u.sidebarHasFocus = false
	u.sidebar.SetBorderColor(palette.border)
	if u.sidebarReturn != "" {
		u.setFooter(u.sidebarReturn)
		u.sidebarReturn = ""
	} else {
		u.setFooter(u.helpFor(u.active))
	}
	u.focusActiveTab()
}

// focusActiveTab puts the keyboard back on whatever the visible tab's widget is.
func (u *ui) focusActiveTab() {
	var p tview.Primitive
	switch u.active {
	case "containers":
		p = u.ctree
	case "volumes":
		p = u.vtable
	case "forwards":
		p = u.ftable
	case "networks":
		p = u.nettable
	case "secrets":
		p = u.sectable
	case "nodes":
		p = u.notable
	case "configs":
		p = u.cfgtable
	}
	if p != nil {
		u.app.SetFocus(p)
	}
}
