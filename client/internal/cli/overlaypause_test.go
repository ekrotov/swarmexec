// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"

	"github.com/rivo/tview"
)

func overlayUI() *ui {
	u := &ui{
		app:      tview.NewApplication(),
		pages:    tview.NewPages(),
		help:     tview.NewTextView(),
		overlays: map[string]bool{},
	}
	u.pages.AddPage(pageMain, tview.NewBox(), true, true)
	return u
}

// The bug: a notice modal did not count as an open overlay, so the background
// tree refresh kept running underneath it — rebuilding and reselecting the tree
// while the operator was reading, and taking the modal's focus with it.
func TestNoticeCountsAsAnOpenOverlay(t *testing.T) {
	u := overlayUI()
	if u.anyOverlayOpen() || u.overlayOpen() {
		t.Fatal("nothing is open yet")
	}
	u.info("something happened")

	if !u.anyOverlayOpen() {
		t.Error("a notice must pause the background refresh (the cheap gate)")
	}
	if !u.overlayOpen() {
		t.Error("a notice must be visible to the derived gate too")
	}
}

// Same for a confirm, which is the one an operator has to answer — losing its
// focus mid-question is the worst version of this.
func TestConfirmCountsAsAnOpenOverlay(t *testing.T) {
	u := overlayUI()
	u.confirm("Remove it?", "Remove", nil, func() {})

	if !u.anyOverlayOpen() {
		t.Error("a confirm must pause the background refresh")
	}
	if !u.overlayOpen() {
		t.Error("a confirm must be visible to the derived gate")
	}
}

// The derived gate is the one that covers EVERY overlay, including the ones
// that register nothing: it asks the page stack rather than a bookkeeping
// counter, so an overlay nobody remembered to count still pauses the refresh.
func TestDerivedGateSeesAnyPageOverMain(t *testing.T) {
	u := overlayUI()
	if u.overlayOpen() {
		t.Fatal("only the main page is up")
	}
	// A page that registers nothing at all — the ~20 sites A5 is about.
	u.pages.AddPage("something-unregistered", tview.NewBox(), true, true)
	if !u.overlayOpen() {
		t.Error("an unregistered overlay must still pause the refresh")
	}
	if u.anyOverlayOpen() {
		t.Error("...and it is correctly absent from the set, which is best-effort")
	}
	u.pages.RemovePage("something-unregistered")
	if u.overlayOpen() {
		t.Error("closing it must resume the refresh")
	}
}

// Two notices racing from two background goroutines: the second AddPage
// replaces the first page, so only one modal is ever dismissed and only one
// release ever runs. With a counter that left the count at 1 forever and the
// tree stopped refreshing for the rest of the session. Keyed by page name, the
// second open is the same entry, and one release clears it.
func TestASecondNoticeDoesNotStrandTheEntry(t *testing.T) {
	u := overlayUI()
	u.info("first")
	release := u.markOverlay(pageInfo) // what the second info() does
	if len(u.overlays) != 1 {
		t.Fatalf("overlays = %v, want one entry for one page", u.overlays)
	}
	release()
	if u.anyOverlayOpen() {
		t.Errorf("one dismissal must clear the page's entry, got %v", u.overlays)
	}
}

// A release is idempotent: an overlay closed twice — a done func that fires
// again, a caller that also removes the page — must not take the entry away
// from someone else.
func TestOverlayReleaseIsIdempotent(t *testing.T) {
	u := overlayUI()
	releaseA := u.markOverlay("a")
	releaseB := u.markOverlay("b")

	releaseA()
	releaseA()
	if !u.anyOverlayOpen() {
		t.Error("b is still open")
	}
	releaseB()
	if u.anyOverlayOpen() {
		t.Errorf("both released, got %v", u.overlays)
	}
}

// Nested overlays that own the footer each get their own entry, so the outer
// one keeps the refresh paused after the inner one closes.
func TestNestedFooterOverlaysEachHoldTheirOwn(t *testing.T) {
	u := overlayUI()
	_, restoreOuter := u.pushOverlayHelp("outer")
	_, restoreInner := u.pushOverlayHelp("inner")

	restoreInner()
	if !u.anyOverlayOpen() {
		t.Error("the outer overlay is still open")
	}
	restoreOuter()
	if u.anyOverlayOpen() {
		t.Errorf("both closed, got %v", u.overlays)
	}
}
