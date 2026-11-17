// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/rivo/tview"
)

// footerUI is enough of a ui to exercise the footer ownership rules: they touch
// only the help widget and the baseline field.
func footerUI() *ui { return &ui{help: tview.NewTextView(), overlays: map[string]bool{}} }

func footerText(u *ui) string { return u.help.GetText(false) }

// The footer has one owner at a time — the tab, or an overlay — and a flash is
// NOT an owner: it borrows the widget and must hand it back to whoever had it.
//
// Regression: flash used to snapshot the widget's text as what to restore.
// Pressing a flashing key twice inside the 1.5s window meant the second flash
// captured the FIRST flash's message, then restored that — so the key hints
// disappeared until the next tab switch. Reproduced from the TUI with two
// quick presses of the stack-grouping key.
func TestFlashDoesNotBecomeTheFooterBaseline(t *testing.T) {
	u := footerUI()
	u.setFooter("tab keys")

	// What flash does to the widget: write directly, leaving ownership alone.
	u.help.SetText("grouped by stack")
	if u.footerBase != "tab keys" {
		t.Fatalf("a flash must not take ownership: baseline = %q", u.footerBase)
	}

	// A second flash inside the window — this is where the old code went wrong.
	u.help.SetText("flat service list")
	if u.footerBase != "tab keys" {
		t.Errorf("a second flash must not take ownership either: baseline = %q", u.footerBase)
	}

	// Whichever flash restores last puts back the owner's text, not a message.
	u.help.SetText(u.footerBase)
	if got := footerText(u); got != "tab keys" {
		t.Errorf("footer = %q, want the tab's keys back", got)
	}
}

// The same flaw hit overlays: one opened while a flash was showing captured the
// flash message and restored it on close. (The counting half is tested in
// overlaypause_test.go; this is the footer half.)
func TestOverlayHelpRestoresTheOwnerNotAFlash(t *testing.T) {
	u := footerUI()
	u.setFooter("tab keys")
	u.help.SetText("grouped by stack") // a flash is up

	_, restore := u.footerTakeover("overlay keys")
	if got := footerText(u); got != "overlay keys" {
		t.Errorf("overlay should own the footer: %q", got)
	}

	restore()
	if got := footerText(u); got != "tab keys" {
		t.Errorf("closing the overlay restored %q, want the tab's keys", got)
	}
}

// Nested overlays still unwind to the right owner, and a double restore is a
// no-op rather than popping someone else's footer.
func TestOverlayHelpNests(t *testing.T) {
	u := footerUI()
	u.setFooter("tab keys")

	setOuter, restoreOuter := u.footerTakeover("outer")
	_, restoreInner := u.footerTakeover("inner")
	if got := footerText(u); got != "inner" {
		t.Errorf("innermost overlay should own the footer: %q", got)
	}

	restoreInner()
	if got := footerText(u); got != "outer" {
		t.Errorf("after the inner closes: %q, want outer", got)
	}
	// An overlay updating its own footer while open stays the owner.
	setOuter("outer updated")
	if u.footerBase != "outer updated" {
		t.Errorf("an overlay's own update should move the baseline: %q", u.footerBase)
	}

	restoreOuter()
	restoreOuter() // idempotent
	if got := footerText(u); got != "tab keys" {
		t.Errorf("after both close: %q, want the tab's keys", got)
	}
}

// The log footer must say WHICH follow state is in force, not merely that f
// toggles one. A paused log view and a container that has gone quiet look
// identical on screen, and the footer is where an operator goes to tell them
// apart — so a fixed "follow on/off" answered the question wrongly half the
// time.
func TestLogFooterNamesTheFollowState(t *testing.T) {
	on, off := logViewHelp(true, 'q'), logViewHelp(false, 'q')
	if on == off {
		t.Fatal("the follow hint reads the same whether following or not")
	}
	// Both halves stay present, so the key still reads as a toggle.
	for _, s := range []string{on, off} {
		if !strings.Contains(s, "on") || !strings.Contains(s, "off") {
			t.Errorf("hint %q dropped one half of the toggle", s)
		}
	}
	// The active half is marked twice over — colour and underline — because one
	// word in a dense single-line footer is easy to miss by colour alone.
	if !strings.Contains(on, "[green::bu]on[white::BU]") {
		t.Errorf("following: %q does not mark \"on\" as active", on)
	}
	if !strings.Contains(off, "[gray::bu]off[white::BU]") {
		t.Errorf("not following: %q does not mark \"off\" as active", off)
	}
	// Marking "off" as active must not also mark "on".
	if strings.Contains(off, "[green::bu]on") {
		t.Errorf("not following, yet %q still highlights \"on\"", off)
	}
}

// The way out leads every overlay footer, whatever order the call site listed
// it in: a footer too long for the terminal is clipped from the right, and
// "Esc" used to be the first thing to go (observed at 80 columns on the node
// detail). The rest keep their order.
func TestOverlayFooterPutsEscFirst(t *testing.T) {
	got := colorTags.ReplaceAllString(footerKeys("j/k", "scroll", "w", "write", "Esc", "back"), "")
	if want := " Esc back  j/k scroll  w write "; got != want {
		t.Errorf("footerKeys = %q, want %q", got, want)
	}
	shared := colorTags.ReplaceAllString(footerKeys("t", "toggle", "Esc/q", "close"), "")
	if !strings.HasPrefix(shared, " Esc/q close") {
		t.Errorf("an Esc shared with quit must lead too: %q", shared)
	}
	if lv := colorTags.ReplaceAllString(logViewHelp(true, 'q'), ""); !strings.HasPrefix(lv, " Esc/q close") {
		t.Errorf("log view footer must lead with its way out: %q", lv)
	}
	// A quit key remapped in keys.yaml closes the log view, so its footer names it.
	if lv := colorTags.ReplaceAllString(logViewHelp(true, 'x'), ""); !strings.HasPrefix(lv, " Esc/x close") {
		t.Errorf("log view footer must name the configured quit key: %q", lv)
	}
}
