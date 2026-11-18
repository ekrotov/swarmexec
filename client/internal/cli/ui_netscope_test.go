// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/network"
	"github.com/rivo/tview"

	"swarmexec/client/internal/clusterread"
	"swarmexec/client/internal/portfwd"
	"swarmexec/client/internal/swarmops"
)

// localNet is a node-local network as the manager lists it.
func localNet(name, driver string) network.Summary {
	var n network.Summary
	n.ID, n.Name, n.Driver, n.Scope = "id-"+name, name, driver, "local"
	return n
}

// scopeUI is a running UI on the Networks tab of a manager that lists two
// swarm networks next to its own local ones.
func scopeUI(t *testing.T) (*ui, *networksTab) {
	t.Helper()
	m := &listManager{networks: []network.Summary{
		netSummary("ingress"), localNet("bridge", "bridge"), localNet("host", "host"),
		netSummary("backend"), localNet("projects_default", "bridge"),
	}}
	u := listUI(t, m)
	onLoop(t, u, func() {
		u.status = tview.NewTextView().SetDynamicColors(true)
		u.forwards = portfwd.NewRegistry()
		u.active = "networks"
	})
	return u, u.netTab()
}

func statusText(t *testing.T, u *ui) string {
	t.Helper()
	var s string
	onLoop(t, u, func() { s = u.status.GetText(true) })
	return s
}

// The tab lists the swarm's networks by default; a shows the manager's local
// ones too and says whose they are, a again hides them.
func TestNetworksTabScopeToggle(t *testing.T) {
	u, nt := scopeUI(t)
	onLoop(t, u, func() { nt.enter() })
	text := waitForTable(t, u, nt.table, "backend", "ingress")
	for _, local := range []string{"bridge", "host", "projects_default"} {
		if strings.Contains(text, local) {
			t.Errorf("default list shows the local %s:\n%s", local, text)
		}
	}
	onLoop(t, u, func() {
		if f := nt.footer(); !strings.Contains(f, "[yellow]a[white] all") {
			t.Errorf("footer %q lacks the a toggle", f)
		}
		u.updateStatus()
	})
	if s := statusText(t, u); strings.Contains(s, "local") {
		t.Errorf("status %q mentions local networks while they are hidden", s)
	}

	onLoop(t, u, func() { nt.keys(rune_('a')) })
	waitForTable(t, u, nt.table, "backend", "bridge", "host", "projects_default", "ingress")
	if s := statusText(t, u); !strings.Contains(s, "+ local networks of the manager node only") {
		t.Errorf("status %q lacks the local-networks hint", s)
	}
	waitFooter(t, u, "local ones (bridge, host, …) are the manager node's only")
	onLoop(t, u, func() {
		if f := nt.footer(); !strings.Contains(f, "swarm only") {
			t.Errorf("footer %q does not offer the way back", f)
		}
	})

	onLoop(t, u, func() { nt.keys(rune_('a')) })
	waitFooter(t, u, "swarm networks only")
	text = waitForTable(t, u, nt.table, "backend")
	if strings.Contains(text, "bridge") {
		t.Errorf("toggling back still shows bridge:\n%s", text)
	}
	if s := statusText(t, u); strings.Contains(s, "local") {
		t.Errorf("status %q still mentions local networks", s)
	}
}

// With no swarm network at all, the empty state points at both keys.
func TestNetworksTabEmptySwarmScope(t *testing.T) {
	u := listUI(t, &listManager{networks: []network.Summary{localNet("bridge", "bridge")}})
	nt := u.netTab()
	onLoop(t, u, func() { nt.enter() })
	waitForTable(t, u, nt.table, "(no swarm networks — n to create one, a for the manager's local ones)")
}

func TestSwarmScoped(t *testing.T) {
	in := []swarmops.Network{{Name: "ingress", Scope: "swarm"}, {Name: "bridge", Scope: "local"}, {Name: "x", Scope: ""}, {Name: "app", Scope: "swarm"}}
	var names []string
	for _, n := range swarmScoped(in) {
		names = append(names, n.Name)
	}
	if got := strings.Join(names, ","); got != "ingress,app" {
		t.Errorf("swarmScoped = %s", got)
	}
}

// "n/a" in CONTAINERS is explained on the status line whenever some node's
// view is missing.
func TestNetworksStatusNoteExplainsNA(t *testing.T) {
	u := listUI(t, &listManager{})
	nt := u.netTab()
	cases := []struct {
		name string
		st   netEndpointsState
		want bool
	}{
		{"not loaded", netEndpointsState{}, false},
		{"all answered", netEndpointsState{loaded: true, byNet: map[string]*clusterread.NetEndpoints{"id": {Answered: 2}}}, false},
		{"load failed", netEndpointsState{loaded: true, err: errors.New("x")}, true},
		{"a node failed", netEndpointsState{loaded: true, failed: 1}, true},
		{"unknown on a network", netEndpointsState{loaded: true, byNet: map[string]*clusterread.NetEndpoints{"id": {Answered: 1, Unknown: 1}}}, true},
	}
	for _, c := range cases {
		var note string
		onLoop(t, u, func() {
			*nt.endpoints() = c.st
			note = nt.statusNote()
		})
		if got := strings.Contains(note, "n/a: node agent not reachable"); got != c.want {
			t.Errorf("%s: note %q, want the n/a legend %v", c.name, note, c.want)
		}
	}
}

// AGE past 30 days drops the hours: months, then years.
func TestObjectAge(t *testing.T) {
	const day = 24 * time.Hour
	for d, want := range map[time.Duration]string{
		0:                          "-",
		90 * time.Minute:           "1h30m",
		3*day + 4*time.Hour:        "3d4h",
		29*day + 23*time.Hour:      "29d23h",
		30 * day:                   "1mo",
		95 * day:                   "3mo",
		364 * day:                  "12mo",
		365 * day:                  "1y",
		(1588*24 + 21) * time.Hour: "4y",
	} {
		if got := objectAge(d); got != want {
			t.Errorf("objectAge(%v) = %q, want %q", d, got, want)
		}
	}
	if got := volumeAge(time.Time{}); got != "-" {
		t.Errorf("volumeAge(zero) = %q", got)
	}
	if got := volumeAge(time.Now().Add(-400 * day)); got != "1y" {
		t.Errorf("volumeAge(400 days ago) = %q, want 1y", got)
	}
}
