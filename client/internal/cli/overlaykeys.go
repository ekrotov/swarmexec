// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"

	"github.com/gdamore/tcell/v2"

	"swarmexec/client/internal/clusterread"
)

// runeCase is one rune key of an overlay: what it does, and which
// configurable action it is the key of ("" for a key that is fixed).
type runeCase struct {
	key    rune
	action string
	run    func()
}

// runRune runs the first case bound to ev's rune and reports whether there was
// one. The first match wins, so the order is the precedence when keys.yaml
// binds a configurable action to a rune another case already has.
func runRune(ev *tcell.EventKey, cases []runeCase) bool {
	if ev.Key() != tcell.KeyRune {
		return false
	}
	for _, c := range cases {
		if c.key == ev.Rune() {
			c.run()
			return true
		}
	}
	return false
}

// overlayKeyset is what an overlay answers to besides the keys every overlay
// shares.
//
// Overlays that answer to configurable actions next to fixed keys of their own
// declare both in one table of runeCases, and their key handler dispatches
// through that same table (runRune). The keyset is derived from the table, so
// it cannot drift from what the handler does: an action bound onto one of the
// overlay's fixed keys would work on its tab and be dead inside the overlay
// (the overlay's own case comes first), and a keys.yaml remap onto one is
// therefore refused like a tab clash.
type overlayKeyset struct {
	name    string   // how the warning names the overlay
	fixed   string   // the runes it handles itself (j/k are reserved anyway)
	actions []string // the configurable actions it also answers to
}

// keysetOf derives the keyset from an overlay's cases. quit and copy are
// shared by every overlay (dialogActions) and are not listed.
func keysetOf(name string, cases []runeCase) overlayKeyset {
	o := overlayKeyset{name: name}
	for _, c := range cases {
		switch {
		case c.action == "":
			if !reservedRunes[c.key] && !strings.ContainsRune(o.fixed, c.key) {
				o.fixed += string(c.key)
			}
		case !dialogActions[c.action]:
			o.actions = append(o.actions, c.action)
		}
	}
	return o
}

// overlayKeysets lists the overlays with fixed keys next to configurable ones.
// The tables are built with the default keymap and never run: only the runes
// and action names are read.
var overlayKeysets = func() []overlayKeyset {
	u := &ui{km: defaultKeybinds()}
	noop := func() {}
	return []overlayKeyset{
		keysetOf("node detail", (&nodeDetail{t: &nodesTab{listTab: &listTab[clusterread.NodeInfo]{u: u}}}).cases()),
		keysetOf("context detail", u.contextDetailCases(noop, noop)),
		keysetOf("forward detail", u.forwardDetailCases(noop, noop)),
		keysetOf("log view", u.logViewCases(nil, nil, nil, noop, noop, noop)),
		keysetOf("client log viewer", u.clientLogCases(noop, noop)),
	}
}()
