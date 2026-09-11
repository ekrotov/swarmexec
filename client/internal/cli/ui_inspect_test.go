// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/rivo/tview"
)

// inspectViewFor builds an inspectView detached from the app, so the render half
// (populate) can be exercised without a screen.
func inspectViewFor(lines []inspLine) *inspectView {
	return &inspectView{
		table:      tview.NewTable(),
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
