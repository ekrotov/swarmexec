// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// The activity view: what the manager reported about this cluster, newest
// first. An overlay rather than a ninth tab, on purpose — a new tab would
// shift the digit keys of every tab after it, which breaks muscle memory and
// every line of the docs that names one, for a view most sessions open twice.
// The security report made the same call for the same reason.
//
// It shows what the event stream already delivered (see activity.go); nothing
// here asks the manager for anything.

// showActivity opens the activity overlay for the visible cluster.
func (u *ui) showActivity() {
	entries, dropped := u.clusterState.activity.list()

	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" activity — %s ", u.clusterState.name))
	tv.SetText(activityText(entries, dropped, u.eventsLive.Load(), time.Now()))

	prev := u.app.GetFocus()
	ov := u.overlayFor(pageActivity, prev, footerKeys("j/k", "scroll", "Esc", "back"))
	closeIt := ov.Close
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape ||
			(ev.Key() == tcell.KeyRune && (ev.Rune() == u.km.Quit || ev.Rune() == u.km.Activity)):
			closeIt()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	})
	ov.show(centered(tv, 88, 26), tv)
}

// activityText renders the ring. now is passed in so the test does not race
// the clock.
func activityText(entries []activityEntry, dropped int, live bool, now time.Time) string {
	var b strings.Builder
	b.WriteString(activityHeader(len(entries), dropped, live))
	if len(entries) == 0 {
		return b.String()
	}
	b.WriteString("\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "  [%s]%-11s[white]  %-7s %-9s %s",
			activityColor(e.Action), sinceText(now.Sub(e.At)), tview.Escape(e.Kind), tview.Escape(e.Action), tview.Escape(e.Name))
		if e.Detail != "" {
			fmt.Fprintf(&b, "  [gray]%s[white]", tview.Escape(e.Detail))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// activityHeader says what this is and, above all, what it is not: it starts
// when the UI connects and ends with the session. An operator who takes it for
// a log would draw conclusions from a gap that only means "we were not
// watching".
func activityHeader(n, dropped int, live bool) string {
	var b strings.Builder
	switch {
	case n == 0 && live:
		b.WriteString("  [gray]Nothing yet. The manager's event stream is connected; changes show up here as they happen.[white]\n")
	case n == 0:
		b.WriteString("  [yellow]No events, and the manager's event stream is not connected.[white]\n" +
			"  [gray]An older daemon or a blocked endpoint; the tree still refreshes on its timer.[white]\n")
	default:
		fmt.Fprintf(&b, "  [gray]%d event(s) since this session connected to the cluster", n)
		if dropped > 0 {
			fmt.Fprintf(&b, ", %d older one(s) dropped", dropped)
		}
		b.WriteString(".[white]\n")
		if !live {
			b.WriteString("  [yellow]The event stream is not connected right now — newer changes may be missing.[white]\n")
		}
	}
	b.WriteString("  [gray]Kept in memory for this session only: not a log, and empty again after a restart.[white]\n")
	return b.String()
}

// activityColor tints by what the action did, not by how loud it is: removal
// red, creation green, everything else plain. A rolling update is normal.
func activityColor(action string) string {
	switch action {
	case "remove", "destroy", "delete":
		return "red"
	case "create":
		return "green"
	default:
		return "white"
	}
}

