// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/swarmops"
)

// The inspect overlay's keys on a running UI (review Q-01 pinned them before
// handleKey was split): close, help, the actions menu, Enter on each row kind,
// copy, the view keys, j/k, and the service editor keys a task inspect lacks.

// inspectLinesFixture: a plain field, a network with DNS names, and an
// upgrade row.
func inspectLinesFixture() []swarmops.InspectLine {
	return []swarmops.InspectLine{
		{Kind: swarmops.InspectField, Text: "Image: nginx:1.27"},
		{Kind: swarmops.InspectNet, Text: "front", Net: "front", Count: 1, Children: []string{"web"}},
		{Kind: swarmops.InspectUpgrade, Text: "↑ 1.28 available", Upgrade: "nginx:1.28",
			UpRepo: "nginx", UpCurrent: "1.27", UpNewer: []string{"1.28"}, UpAll: []string{"1.27", "1.28"}},
	}
}

// openInspectView opens an inspect overlay over lines the way showInspect
// does, for the service editSvc ("" = a task inspect), and waits for the
// lines to be in.
func openInspectView(t *testing.T, u *ui, editSvc string) *inspectView {
	t.Helper()
	table, tabs, frame := newInspectWidgets()
	iv := &inspectView{
		u: u, title: "service web", op: "ui.inspect.test", editSvc: editSvc,
		subject: statsSubject{service: "web"},
		fetch: func() ([]swarmops.InspectLine, string, error) {
			return inspectLinesFixture(), "{\n  \"ID\": \"s1\"\n}", nil
		},
		table: table, tabs: tabs, frame: frame,
		rowNet: map[int]string{}, rowUpgrade: map[int]string{}, expanded: map[string]bool{},
		setHelp: func(string) {},
	}
	onLoop(t, u, func() { iv.open() })
	waitFront(t, u, pageInspect)
	waitFor(t, "the inspect lines", func() bool {
		var loaded bool
		onLoop(t, u, func() { loaded = iv.loaded })
		return loaded
	})
	return iv
}

func inspectKey(t *testing.T, u *ui, iv *inspectView, ev *tcell.EventKey) *tcell.EventKey {
	t.Helper()
	var out *tcell.EventKey
	onLoop(t, u, func() { out = iv.handleKey(ev) })
	return out
}

func inspectUI(t *testing.T) *ui {
	t.Helper()
	u, _, _ := editUI(t, editSwarm())
	onLoop(t, u, func() {
		scr := tcell.NewSimulationScreen("UTF-8")
		u.screen = scr
	})
	return u
}

func TestInspectKeysClose(t *testing.T) {
	for _, ev := range []*tcell.EventKey{
		tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone), rune_(defaultKeybinds().Quit), rune_('i'),
	} {
		u := inspectUI(t)
		iv := openInspectView(t, u, "web")
		if out := inspectKey(t, u, iv, ev); out != nil {
			t.Errorf("%v was passed on", ev.Name())
		}
		waitFront(t, u, pageMain)
		onLoop(t, u, func() {
			if u.onUsage != nil {
				t.Error("closing left the usage hook set")
			}
		})
	}
}

func TestInspectKeysViewsAndMovement(t *testing.T) {
	u := inspectUI(t)
	iv := openInspectView(t, u, "web")
	mode := func() inspMode {
		var m inspMode
		onLoop(t, u, func() { m = iv.mode })
		return m
	}
	for _, want := range []inspMode{inspModeStats, inspModeRaw, inspModeTable} {
		inspectKey(t, u, iv, rune_('t'))
		if m := mode(); m != want {
			t.Fatalf("t: mode %v, want %v", m, want)
		}
	}
	for i, want := range inspModes {
		inspectKey(t, u, iv, rune_(rune('1'+i)))
		if m := mode(); m != want {
			t.Fatalf("%d: mode %v, want %v", i+1, m, want)
		}
	}
	inspectKey(t, u, iv, rune_('1'))
	if out := inspectKey(t, u, iv, rune_('j')); out == nil || out.Key() != tcell.KeyDown {
		t.Errorf("j became %v", out)
	}
	if out := inspectKey(t, u, iv, rune_('k')); out == nil || out.Key() != tcell.KeyUp {
		t.Errorf("k became %v", out)
	}
	down := tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	if out := inspectKey(t, u, iv, down); out != down {
		t.Errorf("Down came back as %v", out)
	}
	ev := rune_('Z')
	if out := inspectKey(t, u, iv, ev); out != ev {
		t.Errorf("an unbound key came back as %v", out)
	}
}

func TestInspectKeysEnterOnEachRow(t *testing.T) {
	u := inspectUI(t)
	iv := openInspectView(t, u, "web")
	enter := tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)
	rowOf := func(want string) int {
		t.Helper()
		var row = -1
		onLoop(t, u, func() {
			for r := 0; r < iv.table.GetRowCount(); r++ {
				if strings.Contains(iv.table.GetCell(r, 0).Text, want) {
					row = r
					break
				}
			}
			iv.table.Select(row, 0)
		})
		if row < 0 {
			t.Fatalf("no row %q", want)
		}
		return row
	}

	// The network row expands and collapses.
	rowOf("front")
	inspectKey(t, u, iv, enter)
	onLoop(t, u, func() {
		if !iv.expanded["front"] {
			t.Error("Enter did not expand the network")
		}
	})
	inspectKey(t, u, iv, enter)
	onLoop(t, u, func() {
		if iv.expanded["front"] {
			t.Error("Enter did not collapse the network")
		}
	})

	// A plain row is copied, and the footer says so; so does the copy key.
	rowOf("Image: nginx")
	inspectKey(t, u, iv, enter)
	if h := textOf(t, u, u.help); !strings.Contains(h, "copied") {
		t.Errorf("Enter on a field: footer %q", h)
	}
	onLoop(t, u, func() { u.help.SetText("") })
	inspectKey(t, u, iv, rune_(u.km.Copy))
	if h := textOf(t, u, u.help); !strings.Contains(h, "copied") {
		t.Errorf("copy key: footer %q", h)
	}

	// The upgrade row opens the version picker.
	rowOf("1.28 available")
	if out := inspectKey(t, u, iv, enter); out != nil {
		t.Error("Enter was passed on")
	}
	waitFront(t, u, pageImageVersion)
}

func TestInspectKeysHelpActionsAndEditors(t *testing.T) {
	u := inspectUI(t)
	iv := openInspectView(t, u, "web")
	inspectKey(t, u, iv, rune_('?'))
	waitFront(t, u, pageHelp)
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageInspect)

	inspectKey(t, u, iv, rune_('a'))
	waitFront(t, u, pageInspectActions)
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageInspect)

	// The editor keys are the actions menu's own: d opens the diff, s the
	// scale prompt.
	inspectKey(t, u, iv, rune_('d'))
	waitFront(t, u, pageInspectDiff)
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageInspect)
	if out := inspectKey(t, u, iv, rune_('s')); out != nil {
		t.Error("s was passed on")
	}
	waitFront(t, u, pageScalePrompt)
}

// A task inspect has no service to edit: a and the editor keys pass
// through, and Enter on the upgrade row copies instead of opening a picker.
func TestInspectKeysOnATaskInspect(t *testing.T) {
	u := inspectUI(t)
	iv := openInspectView(t, u, "")
	for _, r := range []rune{'a', 's', 'd', 'X'} {
		ev := rune_(r)
		if out := inspectKey(t, u, iv, ev); out != ev {
			t.Errorf("%c on a task inspect: %v", r, out)
		}
	}
	onLoop(t, u, func() {
		for r := 0; r < iv.table.GetRowCount(); r++ {
			if strings.Contains(iv.table.GetCell(r, 0).Text, "1.28 available") {
				iv.table.Select(r, 0)
			}
		}
	})
	inspectKey(t, u, iv, tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if p := frontPage(t, u); p != pageInspect {
		t.Errorf("Enter on a task's upgrade row opened %s", p)
	}
	if h := textOf(t, u, u.help); !strings.Contains(h, "copied") {
		t.Errorf("Enter on a task's upgrade row: footer %q", h)
	}
}

// Only the collision-free lowercase letters and X run straight from a
// service inspect (review T-16); every other action letter is passed on and
// opens nothing — it is reached through the a menu.
func TestInspectKeysOnlyDirectKeysRunDirectly(t *testing.T) {
	u := inspectUI(t)
	iv := openInspectView(t, u, "web")
	for _, a := range iv.serviceActions() {
		if inspectDirectKeys[a.key] {
			continue
		}
		ev := rune_(a.key)
		if out := inspectKey(t, u, iv, ev); out != ev {
			t.Errorf("%c (%s) ran directly", a.key, a.label)
		}
		if p := frontPage(t, u); p != pageInspect {
			t.Fatalf("%c (%s) opened %s", a.key, a.label, p)
		}
	}
	if out := inspectKey(t, u, iv, rune_('X')); out != nil {
		t.Error("X was passed on")
	}
	waitFront(t, u, pageConfirm)
}

// The menu shows every action with its letter, and the letter selects it
// there — Shift variants included.
func TestInspectActionsMenuLetters(t *testing.T) {
	u := inspectUI(t)
	iv := openInspectView(t, u, "web")
	inspectKey(t, u, iv, rune_('a'))
	waitFront(t, u, pageInspectActions)
	var items []string
	onLoop(t, u, func() {
		list, _ := u.app.GetFocus().(*tview.List)
		for i := 0; list != nil && i < list.GetItemCount(); i++ {
			main, _ := list.GetItemText(i)
			items = append(items, main)
		}
	})
	actions := iv.serviceActions()
	if len(items) != len(actions)+1 {
		t.Fatalf("menu has %d items, want %d actions + cancel: %q", len(items), len(actions), items)
	}
	for i, a := range actions {
		if !strings.HasPrefix(items[i], "[yellow]"+keyLabel(a.key)+"[white]") {
			t.Errorf("item %d %q does not lead with %c", i, items[i], a.key)
		}
	}

	// j/k still move rather than select.
	keyRune(t, u, 'j')
	waitFront(t, u, pageInspectActions)

	keyRune(t, u, 'P')
	waitFront(t, u, pagePlacementMenu)
	if pageOpen(t, u, pageInspectActions) {
		t.Error("the menu stayed open under the placement menu")
	}
}

// A cluster switch closes the overlays of the cluster it leaves without going
// through their owner's close method. The inspect view's usage hook must go
// with its overlay either way, or every usage pass keeps refilling a dead view.
func TestClusterSwitchClosingInspectReleasesTheUsageHook(t *testing.T) {
	u := inspectUI(t)
	openInspectView(t, u, "web")
	onLoop(t, u, func() {
		if u.onUsage == nil {
			t.Fatal("the open inspect view should have set the usage hook")
		}
		if n := u.closeClusterOverlays(); n == 0 {
			t.Fatal("the switch closed no overlay")
		}
		if u.onUsage != nil {
			t.Error("the usage hook outlived its overlay")
		}
	})
}
