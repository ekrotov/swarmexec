// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// An action an overlay answers to may not be remapped onto one of that
// overlay's fixed keys: node_prune=M would prune from the tab and measure in
// the detail, where M is checked first. Free keys are still accepted, and an
// action no overlay answers to may take the same key.
func TestLoadKeybinds_OverlayFixedKeysAreRefused(t *testing.T) {
	km, warns := loadKeybinds(writeKeys(t, "node_prune: M\nnode_prune_schedule: l\ncontext_use: i\nforward_stop: i\n"))
	if km.NodePrune != 'P' || km.NodePruneSched != 'S' || km.CtxUse != 'u' || km.FwdStop != 'X' {
		t.Errorf("fixed overlay keys must keep the defaults, got %+v", km)
	}
	if len(warns) != 4 {
		t.Fatalf("want four warnings, got %v", warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "is a fixed key in the") {
			t.Errorf("unexpected warning %q", w)
		}
	}
	if !strings.Contains(strings.Join(warns, "\n"), "node detail") {
		t.Errorf("the warning should name the overlay, got %v", warns)
	}

	km, warns = loadKeybinds(writeKeys(t, "node_prune: Z\nvolume_new: M\n"))
	if km.NodePrune != 'Z' || km.VolNew != 'M' || len(warns) != 0 {
		t.Errorf("free keys must be accepted, got prune=%q new=%q warnings=%v", km.NodePrune, km.VolNew, warns)
	}
}

// What each overlay handles itself is derived from the table its key handler
// dispatches through (overlaykeys.go), so there is no source to compare it
// with. This pins the result instead: the sets are what keys.yaml is allowed
// to collide with, and changing one is a decision, not a side effect.
func TestOverlayKeysets(t *testing.T) {
	want := map[string]overlayKeyset{
		"node detail":       {fixed: "iMl", actions: []string{"node_prune", "node_prune_schedule"}},
		"context detail":    {fixed: "i", actions: []string{"context_use"}},
		"forward detail":    {fixed: "i", actions: []string{"forward_stop"}},
		"log view":          {fixed: "fFal/", actions: []string{"toggle_mouse"}},
		"client log viewer": {fixed: "`w", actions: []string{"toggle_mouse"}},
	}
	if len(overlayKeysets) != len(want) {
		t.Fatalf("want %d keysets, got %d", len(want), len(overlayKeysets))
	}
	for _, o := range overlayKeysets {
		w, ok := want[o.name]
		if !ok || o.fixed != w.fixed || !reflect.DeepEqual(o.actions, w.actions) {
			t.Errorf("%s: fixed %q actions %v, want %q %v", o.name, o.fixed, o.actions, w.fixed, w.actions)
		}
	}
}

// The behaviour the keysets exist for, through the real loader: every action
// an overlay answers to is refused on every fixed key of that overlay, so no
// remap can go dead inside it.
func TestLoadKeybinds_EveryOverlayFixedKeyIsRefused(t *testing.T) {
	for _, o := range overlayKeysets {
		for _, action := range o.actions {
			for _, r := range o.fixed {
				_, warns := loadKeybinds(writeKeys(t, action+": "+strconv.Quote(string(r))+"\n"))
				if len(warns) != 1 || !strings.Contains(warns[0], o.name) {
					t.Errorf("%s=%q should be refused as a fixed key of the %s, got %v", action, r, o.name, warns)
				}
			}
		}
	}
}

// A handler that goes through its table answers exactly to the table: the
// first case bound to a rune runs, anything else is passed on.
func TestRunRune(t *testing.T) {
	var ran []string
	cases := []runeCase{
		{'a', "", func() { ran = append(ran, "first") }},
		{'a', "x", func() { ran = append(ran, "shadowed") }},
		{'b', "", func() { ran = append(ran, "b") }},
	}
	if !runRune(rune_('a'), cases) || !runRune(rune_('b'), cases) {
		t.Error("a bound rune must run its case")
	}
	if runRune(rune_('c'), cases) {
		t.Error("an unbound rune must be passed on")
	}
	if !reflect.DeepEqual(ran, []string{"first", "b"}) {
		t.Errorf("ran %v, want the first case for a, then b", ran)
	}
	if runRune(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), cases) {
		t.Error("a non-rune key must not match")
	}
}
