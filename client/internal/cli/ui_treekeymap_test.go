// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// The tree's own keys, one by one (review Q-01 pinned them before the
// switch became a binding table): what each does on the row kinds it can
// meet — a stack, a service, a container — and that every other key falls
// through to the shared tab keys.

// treeKeysUI is the shop cluster loaded into a running UI with node agents
// and the search bar wired, so every tree key has what it reaches for.
func treeKeysUI(t *testing.T) *ui {
	t.Helper()
	u, _, _ := editUI(t, shopSwarm())
	onLoop(t, u, func() {
		u.search = tview.NewInputField()
		u.root = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.search, 0, 0, false)
		u.treeTab().load()
	})
	waitForTree(t, u, "shop_web", "web1aaaaaaaa")
	return u
}

// treeKey presses r on the tree and returns what the handler passed on.
func treeKey(t *testing.T, u *ui, ev *tcell.EventKey) *tcell.EventKey {
	t.Helper()
	var out *tcell.EventKey
	onLoop(t, u, func() { out = u.treeTab().keys(ev) })
	return out
}

func TestTreeKeysOpenTheirOverlays(t *testing.T) {
	km := defaultKeybinds()
	for _, tc := range []struct {
		name string
		row  string // cursor row ("" = leave it where load put it)
		key  rune
		page string
	}{
		{"inspect a service", "shop_web", km.ContainerInspect, pageInspect},
		{"inspect a container", "web1aaaaaaaa", km.ContainerInspect, pageInspect},
		{"logs of a container", "web1aaaaaaaa", km.Logs, pageLogs},
		{"logs of a service", "shop_web", km.Logs, pageLogs},
		{"logs of a stack", "shop  ", km.Logs, pageLogs},
		{"security risks", "shop_web", km.SecurityRisks, pageSecurity},
		{"remove a service", "shop_db", 'X', pageConfirm},
		{"remove a container's service", "db1ccccccccc", 'X', pageConfirm},
		{"remove a stack", "shop  ", 'X', pageConfirm},
		{"stack file menu", "shop_web", km.StackFile, pageMenu},
		{"forward a container", "web2bbbbbbbb", km.Forward, pageFwdPrompt},
		{"forward a service's first task", "shop_db", km.Forward, pageFwdPrompt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := treeKeysUI(t)
			cursorTo(t, u, tc.row)
			if out := treeKey(t, u, rune_(tc.key)); out != nil {
				t.Errorf("the key %q was passed on", tc.key)
			}
			waitFront(t, u, tc.page)
		})
	}
}

func TestTreeKeysThatOpenNothing(t *testing.T) {
	u := treeKeysUI(t)
	km := u.km
	// Forward on a service with no task, the stack file on a service in no
	// stack, X on the "(no stack)" bucket: handled, nothing opens.
	for _, tc := range []struct {
		row   string
		key   rune
		flash string
	}{
		{"loose", km.Forward, ""},
		{"loose", km.StackFile, "no stack under the cursor"},
		{"(no stack)", 'X', "carry no stack label"},
	} {
		cursorTo(t, u, tc.row)
		if out := treeKey(t, u, rune_(tc.key)); out != nil {
			t.Errorf("%s on %s was passed on", string(tc.key), tc.row)
		}
		if p := frontPage(t, u); p != pageMain {
			t.Errorf("%s on %s opened %s", string(tc.key), tc.row, p)
		}
		if tc.flash != "" {
			if h := textOf(t, u, u.help); !strings.Contains(h, tc.flash) {
				t.Errorf("%s on %s: footer %q", string(tc.key), tc.row, h)
			}
		}
	}
}

func TestTreeSearchKeyOpensTheBar(t *testing.T) {
	u := treeKeysUI(t)
	if out := treeKey(t, u, rune_(u.km.Search)); out != nil {
		t.Error("the search key was passed on")
	}
	onLoop(t, u, func() {
		if u.searchMode != "containers" || u.app.GetFocus() != u.search {
			t.Errorf("search mode %q, focus %T", u.searchMode, u.app.GetFocus())
		}
	})
}

// Fold walks up: an open service closes, a closed one (or a container)
// steps to its parent, a stack closes. Unfold opens, then descends.
func TestTreeFoldAndUnfoldKeys(t *testing.T) {
	u := treeKeysUI(t)
	km := u.km
	expanded := func(row string) bool {
		t.Helper()
		var open bool
		onLoop(t, u, func() {
			u.treeTab().root.Walk(func(n, _ *tview.TreeNode) bool {
				if strings.Contains(n.GetText(), row) {
					open = n.IsExpanded()
					return false
				}
				return true
			})
		})
		return open
	}

	cursorTo(t, u, "shop_web")
	if expanded("shop_web") {
		t.Fatal("services start folded")
	}
	treeKey(t, u, rune_(km.Unfold))
	if !expanded("shop_web") || !strings.HasPrefix(cursorRowRaw(t, u), "▾ ") {
		t.Fatalf("unfold did not open the service (%q)", cursorRowRaw(t, u))
	}
	treeKey(t, u, rune_(km.Unfold)) // open: descend to the first container
	if got := cursorRow(t, u); !strings.Contains(got, "web1aaaaaaaa") {
		t.Fatalf("unfold on an open service: cursor on %q", got)
	}
	treeKey(t, u, rune_(km.Fold)) // a container: up to its service
	if got := cursorRow(t, u); !strings.Contains(got, "shop_web") {
		t.Fatalf("fold on a container: cursor on %q", got)
	}
	treeKey(t, u, rune_(km.Fold)) // the open service closes
	if expanded("shop_web") {
		t.Fatal("fold did not close the service")
	}
	treeKey(t, u, rune_(km.Fold)) // closed: up to the stack
	if got := cursorRow(t, u); !strings.HasPrefix(got, "shop  ") {
		t.Fatalf("fold on a closed service: cursor on %q", got)
	}
	treeKey(t, u, rune_(km.Fold)) // the stack closes
	if expanded("shop  ") {
		t.Fatal("fold did not close the stack")
	}
	treeKey(t, u, rune_(km.Unfold)) // and opens again
	if !expanded("shop  ") {
		t.Fatal("unfold did not open the stack")
	}

	// A top-level row has nowhere to go up to; unfold on a container is a
	// no-op.
	cursorTo(t, u, "(no stack)")
	treeKey(t, u, rune_(km.Fold))
	treeKey(t, u, rune_(km.Fold))
	if got := cursorRow(t, u); !strings.Contains(got, "(no stack)") {
		t.Errorf("fold on a top-level row moved the cursor to %q", got)
	}
	cursorTo(t, u, "db1ccccccccc")
	treeKey(t, u, rune_(km.Unfold))
	if got := cursorRow(t, u); !strings.Contains(got, "db1ccccccccc") {
		t.Errorf("unfold on a container moved the cursor to %q", got)
	}
}

// Keys the tree does not bind reach the shared tab keys: j/k become
// down/up, and a key nobody binds comes back unchanged.
func TestTreeKeysFallThroughToTheTabKeys(t *testing.T) {
	u := treeKeysUI(t)
	if out := treeKey(t, u, rune_('j')); out == nil || out.Key() != tcell.KeyDown {
		t.Errorf("j became %v", out)
	}
	ev := rune_('Z')
	if out := treeKey(t, u, ev); out != ev {
		t.Errorf("an unbound key came back as %v", out)
	}
	down := tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	if out := treeKey(t, u, down); out != down {
		t.Errorf("Down came back as %v", out)
	}
}

// service_remove is a keymap action (T-15): rebound, the new key removes and
// the old X falls through like any unbound key; the footer names the bound
// key, still drawn red.
func TestTreeServiceRemoveIsRemappable(t *testing.T) {
	u := treeKeysUI(t)
	onLoop(t, u, func() { u.km.ServiceRemove = 'D' })
	cursorTo(t, u, "shop_db")
	ev := rune_('X')
	if out := treeKey(t, u, ev); out != ev {
		t.Errorf("X after the remap was handled: %v", out)
	}
	if p := frontPage(t, u); p != pageMain {
		t.Fatalf("X after the remap opened %s", p)
	}
	onLoop(t, u, func() {
		if f := u.treeTab().footer(); !strings.Contains(f, keyTag(dangerKey("D"))) || strings.Contains(f, keyTag(dangerKey("X"))) {
			t.Errorf("footer does not name the bound key: %q", f)
		}
	})
	if out := treeKey(t, u, rune_('D')); out != nil {
		t.Error("the bound key was passed on")
	}
	waitFront(t, u, pageConfirm)
}
