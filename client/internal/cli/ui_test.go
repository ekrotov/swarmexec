// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
)

// tview.List reads runes as item shortcuts, so without this mapping j/k are
// swallowed and vim movement dies as soon as an overlay list takes focus.
func TestVimListKeys(t *testing.T) {
	for _, tc := range []struct {
		rune rune
		want tcell.Key
	}{
		{'j', tcell.KeyDown},
		{'k', tcell.KeyUp},
		{'g', tcell.KeyHome},
		{'G', tcell.KeyEnd},
	} {
		got := vimListKeys(tcell.NewEventKey(tcell.KeyRune, tc.rune, tcell.ModNone))
		if got.Key() != tc.want {
			t.Errorf("vimListKeys(%q) = %v, want %v", tc.rune, got.Key(), tc.want)
		}
	}

	// Keys the overlays bind themselves must pass through untouched.
	for _, r := range []rune{'d', 'a', ' ', 'q', 'i'} {
		ev := tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone)
		if got := vimListKeys(ev); got != ev {
			t.Errorf("vimListKeys(%q) rewrote the event, want pass-through", r)
		}
	}

	esc := tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone)
	if got := vimListKeys(esc); got != esc {
		t.Error("vimListKeys rewrote a non-rune key, want pass-through")
	}
}

func TestShortVolume(t *testing.T) {
	hash := "0319f545b2fd31e9fe03382447d1c1235c6cef40a6c527d8b3e55ad7cfc56af2"
	if got, want := shortVolume(hash), hash[:12]+"…"; got != want {
		t.Errorf("shortVolume(hash) = %q, want %q", got, want)
	}
	for _, named := range []string{"consul_consul-data", "nextcloud_harp-certs", "data", "abc123"} {
		if got := shortVolume(named); got != named {
			t.Errorf("shortVolume(%q) = %q, want unchanged", named, got)
		}
	}
}

// serviceColor encodes the containers-tab health rules: 0/0 grey, 0/n red,
// partial orange, n/n aqua.
func TestServiceColor(t *testing.T) {
	cases := []struct {
		running, desired int
		want             tcell.Color
		name             string
	}{
		{0, 0, tcell.ColorGray, "scaled to zero"},
		{0, 3, tcell.ColorRed, "down"},
		{1, 3, tcell.ColorOrange, "partial"},
		{3, 3, tcell.ColorAqua, "healthy"},
		{1, 1, tcell.ColorAqua, "single healthy"},
		{2, 0, tcell.ColorGray, "desired zero wins"},
	}
	for _, c := range cases {
		if got := serviceColor(c.running, c.desired); got != c.want {
			t.Errorf("%s: serviceColor(%d,%d) = %v, want %v", c.name, c.running, c.desired, got, c.want)
		}
	}
}

func TestServiceRow(t *testing.T) {
	cols := svcColumns{name: 6, mode: 10, repl: 3, image: 11}
	web := resolve.Service{Name: "web", Mode: "replicated", Running: 1, Desired: 3, Image: "nginx:1.27", Ports: "*:80->80/tcp"}
	if got, want := serviceRow(web, cols), "web     replicated  1/3  nginx:1.27   *:80->80/tcp"; got != want {
		t.Errorf("serviceRow(web) = %q, want %q", got, want)
	}
	// No ports → the padded image column must not leave a trailing space.
	db := resolve.Service{Name: "db", Mode: "global", Running: 2, Desired: 2, Image: "postgres:15"}
	got := serviceRow(db, cols)
	if strings.HasSuffix(got, " ") {
		t.Errorf("serviceRow(db) has trailing space: %q", got)
	}
	for _, want := range []string{"db", "global", "2/2", "postgres:15"} {
		if !strings.Contains(got, want) {
			t.Errorf("serviceRow(db) = %q, missing %q", got, want)
		}
	}
	// Zero image width drops the image column entirely.
	bare := serviceRow(resolve.Service{Name: "x", Mode: "replicated"}, svcColumns{name: 1, mode: 10, repl: 3})
	if strings.Contains(bare, "-") {
		t.Errorf("serviceRow with no image column should not render a dash: %q", bare)
	}
}

func TestTrimFoldMarker(t *testing.T) {
	cases := map[string]string{
		"▸ web  1/3": "web  1/3", // collapsed
		"▾ web  1/3": "web  1/3", // expanded
		"  -  0/0":   "-  0/0",   // childless padding
		"web  1/3":   "web  1/3", // already plain
	}
	for in, want := range cases {
		if got := trimFoldMarker(in); got != want {
			t.Errorf("trimFoldMarker(%q) = %q, want %q", in, got, want)
		}
	}
}

// The h/l fold keys depend on telling a service group from a container leaf and
// on finding a leaf's owning service (tview.TreeNode has no parent pointer).
func TestServiceParentAndNode(t *testing.T) {
	root := tview.NewTreeNode("root")
	web := tview.NewTreeNode("web").SetReference(svcRef{name: "web"})
	db := tview.NewTreeNode("db").SetReference(svcRef{name: "db"})
	root.AddChild(web)
	root.AddChild(db)
	leaf := tview.NewTreeNode("c1").SetReference(resolve.Candidate{ContainerID: "abc"})
	web.AddChild(leaf)

	if got := serviceParent(root, leaf); got != web {
		t.Errorf("serviceParent(leaf) = %v, want the web node", got)
	}
	if got := serviceParent(root, web); got != nil {
		t.Errorf("serviceParent(service) = %v, want nil", got)
	}
	if !isServiceNode(web) {
		t.Error("service group should be a service node")
	}
	if isServiceNode(leaf) {
		t.Error("container leaf should not be a service node")
	}
}
