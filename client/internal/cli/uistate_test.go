// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// An empty list is the moment an operator most needs to be told what would fill
// it. Every tab now answers that; they used to be dead ends, except forwards.
func TestEveryEmptyTabSaysWhatWouldFillIt(t *testing.T) {
	u := &ui{
		km:           defaultKeybinds(),
		clusterState: newClusterState("test", "test"),
		clusters:     map[string]*clusterState{},
		sectable:     newStateTable(),
		nettable:     newStateTable(),
		cfgtable:     newStateTable(),
		notable:      newStateTable(),
		vtable:       newStateTable(),
		ftable:       newStateTable(),
		cxtable:      newStateTable(),
		forwards:     newForwardRegistry(),
	}

	cases := []struct {
		tab    string
		render func()
		table  *tview.Table
		row    int
		want   string // the advice the empty state must carry
	}{
		{"secrets", u.renderSecrets, u.sectable, 1, string(u.km.SecNew)},
		{"networks", u.renderNetworks, u.nettable, 1, string(u.km.NetNew)},
		{"configs", u.renderConfigs, u.cfgtable, 1, "docker config create"},
		{"nodes", u.renderNodes, u.notable, 1, "the manager reported none"},
		{"volumes", u.renderVolumeTable, u.vtable, 1, string(u.km.VolNew)},
		{"forwards", u.renderForwards, u.ftable, 1, string(u.km.Forward)},
		{"contexts", u.renderContexts, u.cxtable, 0, string(u.km.CtxNew)},
	}
	for _, c := range cases {
		t.Run(c.tab, func(t *testing.T) {
			c.render()
			cell := c.table.GetCell(c.row, 0)
			if cell == nil {
				t.Fatalf("%s: no empty-state row at all", c.tab)
			}
			if !strings.HasPrefix(cell.Text, "(no ") {
				t.Errorf("%s: empty state = %q, want it to open with \"(no …\"", c.tab, cell.Text)
			}
			if !strings.Contains(cell.Text, c.want) {
				t.Errorf("%s: empty state = %q, want it to mention %q", c.tab, cell.Text, c.want)
			}
			// Nothing to act on, so nothing the cursor should land on.
			if !cell.NotSelectable {
				t.Errorf("%s: the empty state must not be selectable", c.tab)
			}
		})
	}
}

// The forwards hint used to name the key as a literal "p", which was quietly
// wrong for anyone who had remapped it.
func TestEmptyStateHintsFollowTheKeymap(t *testing.T) {
	u := &ui{
		km:           defaultKeybinds(),
		clusterState: newClusterState("test", "test"),
		clusters:     map[string]*clusterState{},
		ftable:       newStateTable(),
		forwards:     newForwardRegistry(),
	}
	u.km.Forward = 'F'
	u.renderForwards()
	if got := u.ftable.GetCell(1, 0).Text; !strings.Contains(got, "F on a container") {
		t.Errorf("empty forwards = %q, want the remapped key", got)
	}
}

func TestEmptyText(t *testing.T) {
	if got := emptyText("volumes", "n to create one"); got != "(no volumes — n to create one)" {
		t.Errorf("emptyText = %q", got)
	}
	// No hint when there is nothing to do from here.
	if got := emptyText("nodes", ""); got != "(no nodes)" {
		t.Errorf("emptyText without a hint = %q", got)
	}
}

// Red is a confirmed failure. "unknown" is the manager saying it does not know,
// which is not the same thing and should not look like it.
func TestNodeStateColor(t *testing.T) {
	cases := map[string]tcell.Color{
		"ready":        tcell.ColorGreen,
		"down":         tcell.ColorRed,
		"disconnected": tcell.ColorRed,
		"unknown":      tcell.ColorYellow,
		"":             tcell.ColorYellow,
	}
	for state, want := range cases {
		if got := nodeStateColor(state); got != want {
			t.Errorf("nodeStateColor(%q) = %v, want %v", state, got, want)
		}
	}
}

// Two grays a shade apart asked the eye to tell them apart for something the
// TYPE column already says in words.
func TestNetworkTypeColorHasOneDim(t *testing.T) {
	ingress := networkTypeColor(swarmNetwork{Ingress: true})
	plain := networkTypeColor(swarmNetwork{Driver: "bridge", Scope: "local"})
	if ingress != plain {
		t.Errorf("ingress %v vs local %v — one dim, not two", ingress, plain)
	}
	// The hues that carry meaning are untouched.
	if got := networkTypeColor(swarmNetwork{Attachable: true}); got != tcell.ColorGreen {
		t.Errorf("attachable = %v, want green", got)
	}
	if got := networkTypeColor(swarmNetwork{Internal: true}); got != tcell.ColorYellow {
		t.Errorf("internal = %v, want yellow", got)
	}
	if got := networkTypeColor(swarmNetwork{Driver: "overlay"}); got != tcell.ColorAqua {
		t.Errorf("overlay = %v, want aqua", got)
	}
}

func newStateTable() *tview.Table {
	return tview.NewTable().SetBorders(false).SetSelectable(true, false)
}
