// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"testing"

	"github.com/moby/moby/client"
	"github.com/rivo/tview"

	"swarmexec/client/internal/dockerctx"
)

// A cluster only has to be non-nil for the marker test; nothing dials it.
var dummyConnectedClient client.Client

var errRefused = errors.New("connection refused")

// sidebarUI is the smallest ui that can render the context list.
func sidebarUI(active string, names ...string) *ui {
	u := &ui{
		clusterState: newClusterState(active, active),
		clusters:     map[string]*clusterState{},
		cxtable:      tview.NewTable().SetBorders(false).SetSelectable(true, false),
		activeCtx:    active,
	}
	for _, n := range names {
		u.ctxs = append(u.ctxs, dockerctx.Context{Name: n})
	}
	return u
}

// The sidebar earns its width or it does not appear. A list of the clusters you
// are NOT on is not worth clipping the one you are.
func TestSidebarYieldsWhenTheTerminalIsNarrow(t *testing.T) {
	names := []string{"prod", "staging"}
	if w := sidebarWidth(160, names, false); w == 0 {
		t.Error("a wide terminal should show the sidebar")
	}
	if w := sidebarWidth(80, names, false); w != 0 {
		t.Errorf("a narrow terminal should hide it, got %d columns", w)
	}
}

// …but asking for it has to produce it. Otherwise the only way to switch
// cluster would be unavailable at exactly the size where the list is most
// useful, and a second mechanism would have to exist for that case.
func TestFocusOverridesTheWidthRule(t *testing.T) {
	names := []string{"prod", "staging"}
	if w := sidebarWidth(80, names, true); w == 0 {
		t.Fatal("focusing the sidebar must show it at any width")
	}
	if w := sidebarWidth(40, names, true); w > 20 {
		t.Errorf("on a tiny terminal it must still leave room for the content, got %d of 40", w)
	}
}

// The names are the operator's own; truncating them defeats the list. One
// absurd name must not eat the tree either.
func TestSidebarWidthFollowsTheNamesWithinLimits(t *testing.T) {
	short := sidebarWidth(200, []string{"a", "b"}, false)
	long := sidebarWidth(200, []string{"a", "production-eu-central-1"}, false)
	if long <= short {
		t.Errorf("a longer name should widen the sidebar: %d vs %d", long, short)
	}
	huge := sidebarWidth(200, []string{"this-name-is-far-too-long-to-be-reasonable-but-here-we-are"}, false)
	if huge > sidebarMaxWidth {
		t.Errorf("width must be clamped to %d, got %d", sidebarMaxWidth, huge)
	}
	if bare := sidebarWidth(200, nil, false); bare < sidebarMinWidth {
		t.Errorf("even with no contexts the frame needs its minimum, got %d", bare)
	}
}

// The sidebar has no header row, unlike every other table in this UI. The row
// mapping has to agree with that — reading it through the shared helper would
// return the context ABOVE the cursor and switch to the wrong cluster.
func TestCursorMapsToTheContextUnderIt(t *testing.T) {
	u := sidebarUI("prod", "prod", "staging", "dev")
	u.renderContexts()

	for row, want := range []string{"prod", "staging", "dev"} {
		u.cxtable.Select(row, 0)
		got, ok := u.selectedContext()
		if !ok {
			t.Fatalf("row %d selected nothing", row)
		}
		if got.Name != want {
			t.Errorf("row %d = %q, want %q", row, got.Name, want)
		}
	}

	u.cxtable.Select(99, 0)
	if _, ok := u.selectedContext(); ok {
		t.Error("a row past the end must select nothing, not the last entry")
	}
}

// The markers are the reason the list is on screen at all: they answer "where
// am I, and which of these is warm" without a keystroke.
func TestMarkersDistinguishActiveVisitedAndRefused(t *testing.T) {
	u := sidebarUI("prod", "prod", "staging", "dead", "untouched")
	u.clusters["staging"] = newClusterState("staging", "staging")
	u.clusters["staging"].dcli = &dummyConnectedClient
	u.clusters["dead"] = newClusterState("dead", "dead")
	u.clusters["dead"].err = errRefused
	u.renderContexts()

	want := map[int]string{0: "▶ prod", 1: "· staging", 2: "✗ dead", 3: "  untouched"}
	for row, text := range want {
		if got := u.cxtable.GetCell(row, 0).Text; got != text {
			t.Errorf("row %d = %q, want %q", row, got, text)
		}
	}
}

// The cursor must survive a repaint: the list is re-rendered on every switch and
// every refresh, and moving the operator's selection under them would make the
// next Enter land somewhere they did not choose.
func TestRepaintKeepsTheCursorOnTheSameContext(t *testing.T) {
	u := sidebarUI("prod", "prod", "staging", "dev")
	u.renderContexts()
	u.cxtable.Select(2, 0) // dev

	u.renderContexts()

	got, ok := u.selectedContext()
	if !ok || got.Name != "dev" {
		t.Errorf("cursor after a repaint = %#v, want dev", got)
	}
}
