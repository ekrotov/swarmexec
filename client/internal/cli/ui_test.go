// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"

	"github.com/gdamore/tcell/v2"
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

func TestServiceLabel(t *testing.T) {
	if got, want := serviceLabel("web", 1, 3), "web  1/3"; got != want {
		t.Errorf("serviceLabel = %q, want %q", got, want)
	}
	if got, want := serviceLabel("", 0, 0), "-  0/0"; got != want {
		t.Errorf("serviceLabel(empty) = %q, want %q", got, want)
	}
}
