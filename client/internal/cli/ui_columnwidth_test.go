// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"swarmexec/client/internal/resolve"
)

// rowFor returns the rendered text of one service node, through the real
// render path — renderContainers → svcCols → serviceRow → markService.
//
// Going through the widget matters: the existing row tests hand svcColumns in
// by hand, so they check the formatting and can never see the bug, which is in
// deciding WHICH widths to use.
func svcRowText(t *testing.T, u *ui, name string) string {
	t.Helper()
	u.renderContainers()
	for _, n := range u.croot.GetChildren() {
		if ref, ok := n.GetReference().(svcRef); ok && ref.name == name {
			return n.GetText()
		}
	}
	t.Fatalf("service %q not in the tree", name)
	return ""
}

// The bug, reproduced: a swarm that runs a `replicated-job` on a cron creates
// and removes that service every minute. Its mode string is 14 characters
// against the usual 10, so every column to the right of it moved four places
// and back, twice a minute, while the operator was reading.
//
// The property under test is NOT that the row never changes — the first
// sighting legitimately widens the columns, and that single step is the price
// of option A. It is that the row never changes BACK, because a layout that
// oscillates is the thing that makes a screen unreadable.
func TestColumnWidthsSurviveATransientJobService(t *testing.T) {
	steady := []resolve.Service{
		{Name: "web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx"},
		{Name: "db", Mode: "replicated", Running: 1, Desired: 1, Image: "postgres"},
	}
	job := resolve.Service{Name: "osm_job_x", Mode: "replicated-job", Running: 0, Desired: 1, Image: "opensearch"}
	withJob := append(append([]resolve.Service{}, steady...), job)

	u := treeUI("cs", steady, nil)
	_ = svcRowText(t, u, "web") // first frame, narrow

	u.lastSvcs = withJob
	during := svcRowText(t, u, "web")

	// The job finishes and is removed — the poll must not take the space back.
	u.lastSvcs = steady
	after := svcRowText(t, u, "web")
	if after != during {
		t.Errorf("row moved back when the transient service went away:\n during: %q\n  after: %q", during, after)
	}

	// Two more cycles: still no movement, in either direction.
	for i := 0; i < 2; i++ {
		u.lastSvcs = withJob
		if got := svcRowText(t, u, "web"); got != during {
			t.Fatalf("cycle %d with the job: %q, want %q", i, got, during)
		}
		u.lastSvcs = steady
		if got := svcRowText(t, u, "web"); got != during {
			t.Fatalf("cycle %d without the job: %q, want %q", i, got, during)
		}
	}
}

// The same class without a job: any transient service with a longer name does
// it too. Here so nobody "fixes" this by hard-wiring the mode column to the
// width of "replicated-job" and sees a green suite.
func TestColumnWidthsSurviveATransientLongName(t *testing.T) {
	steady := []resolve.Service{{Name: "web", Mode: "replicated", Running: 1, Desired: 1}}
	long := resolve.Service{Name: "a-very-long-transient-service-name", Mode: "replicated", Running: 1, Desired: 1}

	u := treeUI("cs", steady, nil)
	_ = svcRowText(t, u, "web")

	u.lastSvcs = []resolve.Service{steady[0], long}
	wide := svcRowText(t, u, "web")

	u.lastSvcs = steady
	if after := svcRowText(t, u, "web"); after != wide {
		t.Errorf("width oscillated after the long name went away:\n  wide: %q\n after: %q", wide, after)
	}
}

// Monotone widths would be a leak without a way back. An explicit reload — the
// refresh key, a removed service, a deployed stack — may narrow them again.
func TestExplicitReloadResetsColumnWidths(t *testing.T) {
	steady := []resolve.Service{{Name: "web", Mode: "replicated", Running: 1, Desired: 1}}
	u := treeUI("cs", steady, nil)

	u.lastSvcs = append(append([]resolve.Service{}, steady...),
		resolve.Service{Name: "job", Mode: "replicated-job", Running: 0, Desired: 1})
	wide := svcRowText(t, u, "web")

	u.lastSvcs = steady
	if got := svcRowText(t, u, "web"); got != wide {
		t.Fatalf("the poll must not narrow the columns:\n want: %q\n  got: %q", wide, got)
	}

	// What loadContainers sets before it re-renders.
	u.resetCols = true
	narrow := svcRowText(t, u, "web")
	if narrow == wide {
		t.Errorf("an explicit reload should narrow the columns again, still %q", narrow)
	}
	if !strings.Contains(narrow, "replicated") {
		t.Errorf("row lost its content: %q", narrow)
	}
}

// The image cell renders image+suffix but used to be measured on the image
// alone, so the suffix overflowed and pushed that row's ports sideways.
func TestColumnWidthsAccountForTheVersionSuffix(t *testing.T) {
	u := treeUI("cs", nil, nil)
	// No registry cache: the suffix is empty and the width is the image length.
	u.lastSvcs = []resolve.Service{{Name: "web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1.25"}}
	u.renderContainers()
	if u.svcCols.image != len("nginx:1.25") {
		t.Errorf("image width = %d, want %d", u.svcCols.image, len("nginx:1.25"))
	}

	// The row must fit inside the width the render computed, or the cell
	// overflows and everything after it on that row moves.
	row := serviceRow(u.lastSvcs[0], u.svcCols, u.versionSuffixFor(u.lastSvcs[0]), "")
	if got := len(u.lastSvcs[0].Image) + len(u.versionSuffixFor(u.lastSvcs[0])); got > u.svcCols.image {
		t.Errorf("image cell overflows: content %d > width %d (row %q)", got, u.svcCols.image, row)
	}
}

func TestColumnFloorsApplyToTheFirstFrame(t *testing.T) {
	var c svcColumns
	c.growTo(svcColumns{})
	if c.repl < len("0/0") {
		t.Errorf("repl floor not applied: %d", c.repl)
	}
}
