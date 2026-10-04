// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// A wrapField holds one value: Enter moves on to the next form field instead
// of breaking the line, and a long value wraps onto the field's rows.
func TestWrapFieldEnterMovesOn(t *testing.T) {
	long := "/opt/some/really/long/host/path/that/does/not/fit/into/one/line/of/the/form/config.yml"
	ta := wrapField("host path", long, "", 3)
	var finished tcell.Key = -1
	ta.SetFinishedFunc(func(k tcell.Key) { finished = k })

	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(60, 5)
	ta.SetRect(0, 0, 60, 3)
	ta.Draw(scr)

	ta.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(tview.Primitive) {})
	if finished != tcell.KeyTab {
		t.Fatalf("Enter finished with %v, want Tab (next field)", finished)
	}
	if strings.Contains(ta.GetText(), "\n") {
		t.Fatal("Enter inserted a newline")
	}

	// The whole path is on screen, across rows, not scrolled away.
	var shown strings.Builder
	for y := 0; y < 3; y++ {
		for x := 0; x < 60; x++ {
			s, _, _ := scr.Get(x, y)
			shown.WriteString(s)
		}
	}
	if !strings.Contains(strings.ReplaceAll(shown.String(), " ", ""), "config.yml") {
		t.Fatalf("the end of the value is not visible:\n%s", shown.String())
	}
}

func TestFieldTextDropsNewlines(t *testing.T) {
	ta := wrapField("x", " /a/b\r\n/c \n", "", 2)
	if got := fieldText(ta); got != "/a/b/c" {
		t.Fatalf("got %q", got)
	}
}

// The label form splits key and value and hands back key=value; a key with =
// is refused before it reaches the list.
func TestLabelForm(t *testing.T) {
	var got string
	submit := func(raw string) error { got = raw; return nil }
	form := labelForm("traefik.http.routers.app.rule=Host(`a.example.com`) && PathPrefix(`/api`)", submit, func() {}).(*tview.Form)

	key := form.GetFormItem(0).(*tview.TextArea)
	val := form.GetFormItem(1).(*tview.TextArea)
	if key.GetText() != "traefik.http.routers.app.rule" || val.GetText() != "Host(`a.example.com`) && PathPrefix(`/api`)" {
		t.Fatalf("split wrong: key %q value %q", key.GetText(), val.GetText())
	}
	val.SetText("Host(`b.example.com`)", true)
	form.GetButton(0).InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(tview.Primitive) {})
	if got != "traefik.http.routers.app.rule=Host(`b.example.com`)" {
		t.Fatalf("submitted %q", got)
	}

	got = ""
	key.SetText("a=b", true)
	form.GetButton(0).InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(tview.Primitive) {})
	if got != "" || !strings.Contains(form.GetTitle(), "must not contain =") {
		t.Fatalf("a key with = was accepted (submitted %q, title %q)", got, form.GetTitle())
	}
}
