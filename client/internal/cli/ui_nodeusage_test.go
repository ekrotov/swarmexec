// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"swarmexec/client/internal/clusterread"
)

// T-18: the Nodes tab's CPU% and MEM% come from the usage pass the tree
// already polls, as a share of the node's capacity; a node without an agent
// reading shows "–", never a confident 0%.
func TestNodeUsagePercentCells(t *testing.T) {
	n := clusterread.NodeInfo{Hostname: "a", NanoCPUs: 4e9, MemoryBytes: 8 << 30}
	for _, c := range []struct {
		name     string
		use      nodeUsage
		known    bool
		cpu, mem string
	}{
		{"no agent", nodeUsage{}, false, "–", "–"},
		{"first sample", nodeUsage{MemBytes: 2 << 30}, true, "…", "25%"},
		{"agent's own capacity", nodeUsage{CPUReady: true, CPUPercent: 200, NodeCPUs: 4, MemBytes: 4 << 30, NodeMem: 8 << 30}, true, "50%", "50%"},
		{"manager's capacity as fallback", nodeUsage{CPUReady: true, CPUPercent: 100, MemBytes: 6 << 30}, true, "25%", "75%"},
	} {
		if got := nodeCPUPercent(n, c.use, c.known); got != c.cpu {
			t.Errorf("%s: CPU%% = %q, want %q", c.name, got, c.cpu)
		}
		if got := nodeMemPercent(n, c.use, c.known); got != c.mem {
			t.Errorf("%s: MEM%% = %q, want %q", c.name, got, c.mem)
		}
	}
	unsized := clusterread.NodeInfo{Hostname: "b"}
	if got := nodeCPUPercent(unsized, nodeUsage{CPUReady: true, CPUPercent: 50}, true); got != "–" {
		t.Errorf("CPU%% without a core count = %q", got)
	}
	if got := nodeMemPercent(unsized, nodeUsage{MemBytes: 1}, true); got != "–" {
		t.Errorf("MEM%% without a memory size = %q", got)
	}
	for cell, want := range map[string]tcell.Color{"–": tcell.ColorGray, "…": tcell.ColorGray, "12%": tcell.ColorWhite, "85%": tcell.ColorYellow, "95%": tcell.ColorRed} {
		if got := usagePercentColor(cell); got != want {
			t.Errorf("colour of %q = %v, want %v", cell, got, want)
		}
	}
}

// The columns render per node from u.nodeUse, the ? legend explains the
// leader star, and y copies the same figures.
func TestNodesTabShowsUsageColumns(t *testing.T) {
	u := tabsUI()
	nt := u.nodeTab()
	nt.st().rows = []clusterread.NodeInfo{
		{Hostname: "node-a", Role: "manager", Leader: true, NanoCPUs: 2e9, MemoryBytes: 4 << 30},
		{Hostname: "node-b", Role: "worker", NanoCPUs: 2e9, MemoryBytes: 4 << 30},
	}
	u.nodeUse = map[string]nodeUsage{"node-a": {CPUReady: true, CPUPercent: 50, NodeCPUs: 2, MemBytes: 1 << 30, NodeMem: 4 << 30}}
	nt.render()
	if h := nt.table.GetCell(0, 4).Text + " " + nt.table.GetCell(0, 5).Text; h != "CPU% MEM%" {
		t.Fatalf("headers 4/5 = %q", h)
	}
	cells := func(r int) string { return nt.table.GetCell(r, 4).Text + " " + nt.table.GetCell(r, 5).Text }
	if got := cells(1); got != "25% 25%" {
		t.Errorf("node-a = %q, want 25%% 25%%", got)
	}
	if got := cells(2); got != "– –" {
		t.Errorf("node-b without agent data = %q, want – –", got)
	}

	h := &helpWriter{}
	nt.helpSection(h)
	if legend := h.String(); !strings.Contains(legend, "★") || !strings.Contains(legend, "leader") {
		t.Errorf("the ? legend does not explain the star:\n%s", legend)
	}

	var b strings.Builder
	nt.yank(&b)
	if !strings.Contains(b.String(), "CPU%\tMEM%") || !strings.Contains(b.String(), "\t25%\t25%\t") {
		t.Errorf("y copy lacks the usage columns:\n%s", b.String())
	}
}
