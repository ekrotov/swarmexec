// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"testing"
)

// inspectViewFor builds an inspectView detached from the app, so the render half
// (populate) can be exercised without a screen.
func inspectViewFor(lines []inspLine) *inspectView {
	table, tabs, frame := newInspectWidgets()
	return &inspectView{
		table:      table,
		tabs:       tabs,
		frame:      frame,
		lines:      lines,
		loaded:     true,
		rowNet:     map[int]string{},
		rowUpgrade: map[int]string{},
		expanded:   map[string]bool{},
		setHelp:    func(string) {},
	}
}

// rendered returns the table's rows as plain text.
func rendered(iv *inspectView) string {
	var b strings.Builder
	for r := 0; r < iv.table.GetRowCount(); r++ {
		if c := iv.table.GetCell(r, 0); c != nil {
			b.WriteString(c.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// The NETWORKS section drills down in two steps: the collapsed row already
// answers "which IP is the service", expanding once gives the DNS names and a
// container count, expanding that gives the containers' own addresses.
func TestInspectNetworkDrillDown(t *testing.T) {
	net := inspLine{
		Kind: inspNet, Text: "gateway", Net: "gateway", Count: 2,
		Addr: "10.0.1.2/24", AddrLabel: "vip",
		Children: []string{"web", "tasks.web"},
		Tasks: []inspTaskRow{
			{Text: "web.1  10.0.1.5/24  host-a", Copy: "10.0.1.5"},
			{Text: "web.2  10.0.1.6/24  host-b", Copy: "10.0.1.6"},
		},
	}
	iv := inspectViewFor([]inspLine{net})

	// Collapsed: the VIP is visible without any expanding at all.
	iv.populate()
	out := rendered(iv)
	if !strings.Contains(out, "+ gateway") || !strings.Contains(out, "vip 10.0.1.2/24") {
		t.Errorf("collapsed row should show the vip:\n%s", out)
	}
	if strings.Contains(out, "10.0.1.5") || strings.Contains(out, "containers") {
		t.Errorf("container level must stay hidden while collapsed:\n%s", out)
	}

	// One level in: DNS names plus the container count as its own toggle row.
	iv.expanded["gateway"] = true
	iv.populate()
	out = rendered(iv)
	for _, want := range []string{"- gateway", "tasks.web", "+ 2 containers"} {
		if !strings.Contains(out, want) {
			t.Errorf("expanded network missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "10.0.1.5") {
		t.Errorf("addresses belong one level deeper:\n%s", out)
	}

	// The count row must be selectable and carry its own collapse key, or Enter
	// on it would toggle the network itself.
	countRow := -1
	for r, key := range iv.rowNet {
		if key == netTasksKey("gateway") {
			countRow = r
		}
	}
	if countRow < 0 {
		t.Fatalf("no toggle row registered for the container level: %v", iv.rowNet)
	}

	// Two levels in: the container addresses, each copying its bare address.
	iv.expanded[netTasksKey("gateway")] = true
	iv.populate()
	out = rendered(iv)
	for _, want := range []string{"- 2 containers", "web.1  10.0.1.5/24  host-a", "web.2"} {
		if !strings.Contains(out, want) {
			t.Errorf("container level missing %q:\n%s", want, out)
		}
	}
	if !containsPlain(iv, "10.0.1.5") {
		t.Errorf("a container row should copy its bare address: %v", iv.plain)
	}
	if !containsPlain(iv, "10.0.1.2") {
		t.Errorf("the network row should copy its bare vip: %v", iv.plain)
	}

	// Collapsing the network hides the deeper level again.
	iv.expanded["gateway"] = false
	iv.populate()
	if out := rendered(iv); strings.Contains(out, "containers") {
		t.Errorf("collapsing the network must hide its container level:\n%s", out)
	}
}

func containsPlain(iv *inspectView, want string) bool {
	for _, p := range iv.plain {
		if p == want {
			return true
		}
	}
	return false
}

// A network without a VIP (dnsrr) keeps the old header shape and copies its name.
func TestInspectNetworkRowWithoutVIP(t *testing.T) {
	iv := inspectViewFor([]inspLine{{Kind: inspNet, Text: "dbnet", Net: "dbnet", Count: 1, Children: []string{"db"}}})
	iv.populate()
	if out := rendered(iv); !strings.Contains(out, "+ dbnet  (1 dns names)") {
		t.Errorf("unexpected header for a vip-less network:\n%s", out)
	}
	if !containsPlain(iv, "dbnet") {
		t.Errorf("a vip-less network row should copy its name: %v", iv.plain)
	}
}

// A row whose DNS-name count would say nothing (ingress) shows its note instead.
func TestInspectNetworkRowNote(t *testing.T) {
	iv := inspectViewFor([]inspLine{{
		Kind: inspNet, Text: "ingress", Net: "ingress",
		Addr: "10.0.0.5/24", AddrLabel: "vip", Note: "routing mesh",
		Children: []string{"published 8080 -> 80/tcp"},
	}})
	iv.populate()
	out := rendered(iv)
	if !strings.Contains(out, "ingress  vip 10.0.0.5/24  (routing mesh)") {
		t.Errorf("note should replace the dns-name count:\n%s", out)
	}
	if strings.Contains(out, "dns names") {
		t.Errorf("a noted row must not also count dns names:\n%s", out)
	}
}

// The overlay's three views are named on screen, all at once. Before this the
// only place any of them was named was a footer hint for the NEXT one, so the
// resource-usage view was reachable but undiscoverable: you had to press "t"
// twice, past a hint that said "raw json", to find out it existed.
func TestInspectTabStripNamesEveryView(t *testing.T) {
	strip := inspTabStrip(inspModeTable)
	for i, m := range inspModes {
		if !strings.Contains(strip, m.label()) {
			t.Errorf("the strip does not name %q: %s", m.label(), strip)
		}
		// Each carries the digit that selects it, matching the main tab bar.
		if !strings.Contains(strip, fmt.Sprintf("%s[-::B] [#64748b]%d", m.label(), i+1)) &&
			!strings.Contains(strip, fmt.Sprintf("%s[-::B] [#2dd4bf::b]%d", m.label(), i+1)) {
			t.Errorf("%q is not followed by the digit %d: %s", m.label(), i+1, strip)
		}
	}

	// Exactly one tab is accented, and it is the active one.
	for _, active := range inspModes {
		s := inspTabStrip(active)
		if n := strings.Count(s, "[#2dd4bf::b]"); n != 2 { // label + digit
			t.Errorf("%s: %d accented spans, want the label and its digit", active.label(), n)
		}
		if !strings.Contains(s, "[#2dd4bf::b]"+active.label()) {
			t.Errorf("%s is not the accented tab: %s", active.label(), s)
		}
	}
}

// The digits the strip advertises must select the view they sit next to. An
// off-by-one here would silently open the wrong view.
func TestInspectDigitsMatchTheStripOrder(t *testing.T) {
	want := []inspMode{inspModeTable, inspModeStats, inspModeRaw}
	if len(inspModes) != len(want) {
		t.Fatalf("strip has %d tabs, want %d", len(inspModes), len(want))
	}
	for i, m := range want {
		if inspModes[i] != m {
			t.Errorf("digit %d selects %q, want %q", i+1, inspModes[i].label(), m.label())
		}
	}
}

// The actions menu is where an operator looks up what can be done to a service.
// Changing the image version was reachable only from the bare "u" key, named in
// the inspect footer and nowhere else — so the menu's fourteen entries implied
// it could not be done, and "roll back to the previous version" (the previous
// SPEC, not a version you choose) read like the closest thing on offer.
func TestActionsMenuOffersTheVersionPicker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		info    *upgradeInfo
		upgrade bool
		want    string
	}{
		{"a newer version exists", &upgradeInfo{repo: "acme/api"}, true, "update image version…"},
		{"pin or roll back to any tag", &upgradeInfo{repo: "acme/api"}, false, "set image version…"},
		// Not hidden: an absent entry is what sent the operator looking in the
		// first place. It stays listed and explains itself when chosen.
		{"nothing to pick from", nil, false, "set image version — unavailable for this image"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iv := inspectViewFor(nil)
			iv.editSvc = "acme_api"
			iv.upInfo, iv.hasUpgrade = tc.info, tc.upgrade

			var got []string
			for _, a := range iv.serviceActions() {
				got = append(got, a.label)
			}
			if len(got) == 0 || got[0] != tc.want {
				t.Fatalf("first action = %v, want %q (all: %v)", got[:1], tc.want, got)
			}
			for _, l := range got[1:] {
				if strings.Contains(l, "image version") {
					t.Errorf("the version entry is listed twice: %v", got)
				}
			}
		})
	}
}

// The three ways in have to reach one flow, not three spellings of it.
func TestVersionPickerIsANoOpWithoutData(t *testing.T) {
	iv := inspectViewFor(nil)
	iv.editSvc = "acme_api"
	iv.upInfo = nil
	iv.openVersionPicker() // must not panic or dereference a nil upInfo

	iv2 := inspectViewFor(nil)
	iv2.upInfo = &upgradeInfo{repo: "acme/api"}
	iv2.editSvc = "" // a task/container inspect has no service to update
	iv2.openVersionPicker()
}
