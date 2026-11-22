// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/rivo/tview"

	"swarmexec/client/internal/clusterread"
	"swarmexec/client/internal/resolve"
)

// offsetOf is where a marker starts in a rendered row, in columns the terminal
// will actually use — which is what "the same column" has to mean here.
func offsetOf(row, marker string) int {
	i := strings.Index(row, marker)
	if i < 0 {
		return -1
	}
	return tview.TaggedStringWidth(row[:i])
}

func rowCols(svcs []resolve.Service) svcColumns {
	var c svcColumns
	c.growTo(serviceWidths(svcs, func(resolve.Service) string { return "" }))
	return c
}

// The readings are read DOWN the screen. A service that is busy on memory and
// idle on CPU used to print its lone "mem" figure exactly where every other
// row carried its CPU figure.
func TestMemoryReadingKeepsItsColumnWithoutACPUReading(t *testing.T) {
	svcs := []resolve.Service{{Name: "web", Mode: "replicated", Image: "nginx:1.27"}}
	cols := rowCols(svcs)

	both := serviceRow(svcs[0], cols, "", "",
		usageBadge(containerUsage{CPUReady: true, CPURatio: 0.93, MemRatio: 0.91}))
	memOnly := serviceRow(svcs[0], cols, "", "",
		usageBadge(containerUsage{CPUReady: true, CPURatio: 0.05, MemRatio: 0.88}))

	want, got := offsetOf(both, "mem "), offsetOf(memOnly, "mem ")
	if want < 0 || got < 0 {
		t.Fatalf("no memory reading rendered: both=%q memOnly=%q", both, memOnly)
	}
	if got != want {
		t.Errorf("memory reading at column %d without a CPU reading, %d with one\n both:    %q\n memOnly: %q",
			got, want, both, memOnly)
	}
}

// Ports are an operator's choice, not a property of the row's shape: whether a
// service publishes one must not move the readings of the rows around it.
func TestReadingsAlignWhetherOrNotAServicePublishesPorts(t *testing.T) {
	svcs := []resolve.Service{
		{Name: "web", Mode: "replicated", Image: "nginx:1.27", Ports: "*:80->80/tcp"},
		{Name: "job", Mode: "replicated", Image: "nginx:1.27"},
	}
	cols := rowCols(svcs)
	badge := usageBadge(containerUsage{CPUReady: true, CPURatio: 0.93, MemRatio: 0.91})

	withPorts := serviceRow(svcs[0], cols, "", "", badge)
	without := serviceRow(svcs[1], cols, "", "", badge)
	if a, b := offsetOf(withPorts, "cpu "), offsetOf(without, "cpu "); a != b || a < 0 {
		t.Errorf("CPU reading at column %d with ports, %d without\n with:    %q\n without: %q",
			a, b, withPorts, without)
	}
}

// A service turning unhealthy must not shove the numbers behind it sideways.
func TestHealthMarkerDoesNotMoveTheReadings(t *testing.T) {
	svcs := []resolve.Service{{Name: "web", Mode: "replicated", Image: "nginx:1.27"}}
	cols := rowCols(svcs)
	sick := strings.TrimSpace(healthBadge(clusterread.ServiceHealth{Unhealthy: 2}))
	cols.growTo(svcColumns{health: tview.TaggedStringWidth(sick)})
	badge := usageBadge(containerUsage{CPUReady: true, CPURatio: 0.93, MemRatio: 0.91})

	healthy := serviceRow(svcs[0], cols, "", "", badge)
	unhealthy := serviceRow(svcs[0], cols, "", sick, badge)
	if a, b := offsetOf(healthy, "cpu "), offsetOf(unhealthy, "cpu "); a != b || a < 0 {
		t.Errorf("CPU reading at column %d when healthy, %d when not\n healthy:   %q\n unhealthy: %q",
			a, b, healthy, unhealthy)
	}
	if !strings.Contains(unhealthy, "2 unhealthy") {
		t.Errorf("row = %q, want the health marker shown", unhealthy)
	}
}

// The marker still has to appear on a reading that lands before the column has
// been measured; it is padded once the width is known, never dropped.
func TestHealthMarkerShowsBeforeItsColumnIsMeasured(t *testing.T) {
	svcs := []resolve.Service{{Name: "web", Mode: "replicated", Image: "nginx:1.27"}}
	sick := strings.TrimSpace(healthBadge(clusterread.ServiceHealth{Starting: 1}))
	row := serviceRow(svcs[0], rowCols(svcs), "", sick, "")
	if !strings.Contains(row, "1 starting") {
		t.Errorf("row = %q, want the health marker shown with a zero-width column", row)
	}
}

// One service pinned to a long registry path must not set the image column for
// every other row — that is what pushed the rest of the row out of sight.
func TestALongImageDoesNotWidenEveryRow(t *testing.T) {
	svcs := []resolve.Service{
		{Name: "web", Mode: "replicated", Image: "nginx:1.27", Ports: "*:80->80/tcp"},
		{Name: "api", Mode: "replicated", Image: "registry.example.com/platform/team/backend/api-server:v2.14.3-rc1"},
	}
	cols := rowCols(svcs)
	row := serviceRow(svcs[0], cols, "", "", "")
	if w := tview.TaggedStringWidth(row); w > 80 {
		t.Errorf("a two-service row is %d columns wide: %q", w, row)
	}
	if cols.image > svcImageMax {
		t.Errorf("image column = %d, want at most %d", cols.image, svcImageMax)
	}
}
