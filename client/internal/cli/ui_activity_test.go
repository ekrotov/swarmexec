// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/events"
	"github.com/rivo/tview"
)

func TestActivityTextShowsWhatHappenedAndWhen(t *testing.T) {
	now := time.Now()
	entries := []activityEntry{
		{At: now.Add(-30 * time.Second), Kind: "service", Action: "update", Name: "shop_web", Detail: "updatestate: completed"},
		{At: now.Add(-20 * time.Minute), Kind: "secret", Action: "create", Name: "db_password"},
		{At: now.Add(-3 * time.Hour), Kind: "node", Action: "update", Name: "worker-1", Detail: "availability: drain"},
	}
	got := activityText(entries, 0, true, now)

	for _, want := range []string{
		"shop_web", "updatestate: completed", "just now",
		"db_password", "20 min ago",
		"worker-1", "availability: drain", "3h ago",
		"not a log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the view does not mention %q:\n%s", want, got)
		}
	}
}

// Names come from the daemon, which got them from whoever deployed the
// service. tview reads square brackets as colour tags, so a name must arrive
// escaped — the mistake !178 found in the security overlay.
func TestActivityTextEscapesNamesFromTheDaemon(t *testing.T) {
	now := time.Now()
	got := activityText([]activityEntry{
		{At: now, Kind: "service", Action: "create", Name: "[red]gotcha[white]"},
	}, 0, true, now)

	if !strings.Contains(got, "[red[]gotcha") {
		t.Errorf("the name was not escaped:\n%s", got)
	}
}

func TestActivityHeaderSaysWhyItIsEmpty(t *testing.T) {
	now := time.Now()
	live := activityText(nil, 0, true, now)
	if !strings.Contains(live, "Nothing yet") || !strings.Contains(live, "connected") {
		t.Errorf("an empty but live view must say so:\n%s", live)
	}
	dead := activityText(nil, 0, false, now)
	if !strings.Contains(dead, "not connected") {
		t.Errorf("an empty view with no stream must say THAT:\n%s", dead)
	}
	// A stream that died with entries already in the ring is the dangerous
	// case: what is shown is right, and incomplete.
	stale := activityText([]activityEntry{{At: now, Kind: "service", Action: "update", Name: "web"}}, 12, false, now)
	if !strings.Contains(stale, "12 older one(s) dropped") || !strings.Contains(stale, "may be missing") {
		t.Errorf("a stale view must say both things:\n%s", stale)
	}
}

// The overlay, opened the way an operator opens it: the configured key, from
// the tree, and closed with the same key.
func TestActivityOverlayOpensOnItsKeyAndShowsTheRing(t *testing.T) {
	u := treeKeysUI(t)
	onLoop(t, u, func() {
		u.clusterState.activity.record(ev(events.ServiceEventType, events.ActionUpdate,
			map[string]string{"name": "shop_web", "updatestate.new": "completed"}))
	})

	if out := treeKey(t, u, rune_(u.km.Activity)); out != nil {
		t.Fatalf("the activity key was passed on")
	}
	waitFront(t, u, pageActivity)
	waitFor(t, "the recorded event", func() bool {
		var txt string
		onLoop(t, u, func() {
			if tv, ok := u.app.GetFocus().(*tview.TextView); ok {
				txt = tv.GetText(true)
			}
		})
		return strings.Contains(txt, "shop_web") && strings.Contains(txt, "updatestate: completed")
	})

	keyRune(t, u, u.km.Activity)
	waitFor(t, "the overlay to close", func() bool { return !pageOpen(t, u, pageActivity) })
}

func TestActivityOverlayClosesOnEscape(t *testing.T) {
	u := treeKeysUI(t)
	treeKey(t, u, rune_(u.km.Activity))
	waitFront(t, u, pageActivity)
	keyCode(t, u, tcell.KeyEscape)
	waitFor(t, "the overlay to close", func() bool { return !pageOpen(t, u, pageActivity) })
}
