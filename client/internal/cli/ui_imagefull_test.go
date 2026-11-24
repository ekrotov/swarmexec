// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"swarmexec/client/internal/imageref"
	"swarmexec/client/internal/resolve"
)

// digestPinned is the case the shortened cell cannot serve: the hex is the
// whole point of the reference, and Elide cuts it to twelve characters.
const digestPinned = "registry.logle.io/internal-tools/cloud-prices-idealo/admin@sha256:" +
	"3f1a9c7e5b2d48a6f0c3e9b17d4a52c8e6f0b3d719a4c5e82f6b0d13a7c9e4f25"

// The cap shortens; no cap shows the reference exactly as the spec carries it.
// Both directions are asserted, because a cell that merely LOOKS long would
// pass a one-sided check while still dropping the last hex characters.
func TestTheUncappedImageCellKeepsTheWholeDigest(t *testing.T) {
	capped := imageCell(digestPinned, "", svcImageMax)
	if !strings.Contains(capped, imageref.Ellipsis) {
		t.Errorf("capped cell = %q, want it marked as shortened", capped)
	}
	if strings.Contains(capped, digestPinned) {
		t.Errorf("capped cell = %q, want the digest shortened", capped)
	}

	full := imageCell(digestPinned, "", svcImageUncapped)
	if full != digestPinned {
		t.Errorf("uncapped cell = %q, want the reference verbatim", full)
	}
}

// The version annotation still rides along in the uncapped cell — it is the
// one part that is appended rather than elided, and dropping it here would be
// easy to miss.
func TestTheUncappedImageCellStillCarriesTheVersionAnnotation(t *testing.T) {
	got := imageCell("nginx:1.27", " (1.4.0) ↑", svcImageUncapped)
	if want := "nginx:1.27 (1.4.0) ↑"; got != want {
		t.Errorf("imageCell = %q, want %q", got, want)
	}
}

// The column is measured at the same cap the row is drawn at. If the two ever
// disagree the full reference overflows its cell and shoves the ports and the
// resource readings of that row sideways — the bug the cap was added to fix.
func TestTheImageColumnIsMeasuredAtTheSameCapTheRowIsDrawnAt(t *testing.T) {
	svcs := []resolve.Service{{Name: "admin", Mode: "replicated", Running: 1, Desired: 1, Image: digestPinned}}
	none := func(resolve.Service) string { return "" }

	if w := serviceWidths(svcs, none, svcImageMax).image; w > svcImageMax {
		t.Errorf("capped image column = %d, want at most %d", w, svcImageMax)
	}
	w := serviceWidths(svcs, none, svcImageUncapped).image
	if want := len(digestPinned); w != want {
		t.Errorf("uncapped image column = %d, want %d", w, want)
	}
	row := serviceRow(svcs[0], serviceWidths(svcs, none, svcImageUncapped), svcImageUncapped, "", "", "")
	if !strings.Contains(row, digestPinned) {
		t.Errorf("uncapped row = %q, want the whole reference", row)
	}
}

// Turning the key back off has to NARROW the column again.
//
// This is the one that can silently break: growTo never shrinks a column, on
// purpose, so without resetting the widths the list would keep the width of
// the longest full reference for the rest of the session — the operator
// presses the key twice, sees nothing change back, and the tab is ruined
// until a manual refresh.
func TestTheImageColumnNarrowsAgainWhenTheKeyGoesOff(t *testing.T) {
	// Both carry ports, so the image column's padding is actually visible:
	// serviceRow trims trailing spaces, and a row whose last filled cell IS
	// the image would hide a column that stayed too wide.
	svcs := []resolve.Service{
		{Name: "admin", Mode: "replicated", Running: 1, Desired: 1, Image: digestPinned, Ports: "*:9090->9090/tcp"},
		{Name: "web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1.27", Ports: "*:80->80/tcp"},
	}
	u := treeUI("cs", svcs, nil)
	tt := u.treeTab()

	before := svcRowText(t, u, "web")

	tt.setImageFull(true)
	full := svcRowText(t, u, "admin")
	if !strings.Contains(full, digestPinned) {
		t.Errorf("row with the key on = %q, want the whole reference", full)
	}

	tt.setImageFull(false)
	if after := svcRowText(t, u, "web"); after != before {
		t.Errorf("the column did not narrow again:\n before: %q\n  after: %q", before, after)
	}
}

// A cluster that was not on screen while the key was pressed must narrow too:
// the widths are cached per cluster, so resetting only the visible one leaves
// every other cluster sized for a reference it no longer shows — and nothing
// but a manual refresh there would ever take that space back.
//
// The key is pressed BOTH times while "cs" is on screen; "ovb" only ever gets
// looked at in between.
func TestEveryClusterSColumnsAreResetNotJustTheVisibleOne(t *testing.T) {
	admin := func() []resolve.Service {
		return []resolve.Service{{
			Name: "admin", Mode: "replicated", Running: 1, Desired: 1,
			Image: digestPinned, Ports: "*:9090->9090/tcp",
		}}
	}
	// What that row looks like on a session where the key was never touched.
	want := svcRowText(t, treeUI("ovb", admin(), nil), "admin")

	u := treeUI("cs", []resolve.Service{
		{Name: "web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx:1.27", Ports: "*:80->80/tcp"},
	}, nil)
	tt := u.treeTab()
	cs := u.clusterState
	ovb := newClusterState("ovb", "ovb")
	u.clusters["ovb"] = ovb
	tt.per.of(ovb).svcs = admin()

	tt.setImageFull(true)
	u.clusterState = ovb
	wide := svcRowText(t, u, "admin") // ovb measures itself at the full width
	if wide == want {
		t.Fatalf("the hidden cluster never widened, so this test proves nothing: %q", wide)
	}

	u.clusterState = cs
	tt.setImageFull(false)
	u.clusterState = ovb
	if got := svcRowText(t, u, "admin"); got != want {
		t.Errorf("the hidden cluster kept its wide image column:\n got: %q\nwant: %q", got, want)
	}
}
