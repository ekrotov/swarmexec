// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/rivo/tview"

	"swarmexec/client/internal/dockerctx"
)

func TestStatusRightText(t *testing.T) {
	plain := func(s string) string {
		return strings.TrimSpace(tview.NewTextView().SetDynamicColors(true).SetText(s).GetText(true))
	}
	cases := []struct {
		name          string
		hidden        bool
		contexts      int
		key           rune
		want, wantNot string
	}{
		{"wide: version only", false, 3, 'c', "v1.22.3", "contexts"},
		{"narrow: hint and version", true, 3, 'c', "c contexts  ·  v1.22.3", ""},
		{"remapped key", true, 3, 'C', "C contexts", ""},
		{"no contexts, no hint", true, 0, 'c', "v1.22.3", "contexts"},
	}
	for _, c := range cases {
		got := plain(statusRightText("v1.22.3", c.key, c.hidden, c.contexts))
		if !strings.Contains(got, c.want) || (c.wantNot != "" && strings.Contains(got, c.wantNot)) {
			t.Errorf("%s: got %q", c.name, got)
		}
		if !strings.HasSuffix(got, "v1.22.3") {
			t.Errorf("%s: the version is not at the far right: %q", c.name, got)
		}
	}
}

// The hint follows the terminal width: it appears in the frame the context
// list is hidden in, and goes when the list comes back.
func TestFooterHintFollowsSidebar(t *testing.T) {
	u := treeUI("a", nil, nil)
	u.km = defaultKeybinds()
	u.g = &globalFlags{version: Version{Binary: "v9.8.7"}}
	u.ctxs = []dockerctx.Context{{Name: "prod"}, {Name: "staging"}}
	u.sidebar = tview.NewFlex()
	u.body = tview.NewFlex().AddItem(tview.NewBox(), 0, 1, true).AddItem(nil, 0, 0, false).AddItem(u.sidebar, 0, 0, false)
	u.status, u.statusRight = tview.NewTextView(), tview.NewTextView().SetDynamicColors(true)
	u.statusRow = tview.NewFlex().AddItem(u.status, 0, 1, false).AddItem(u.statusRight, 0, 0, false)
	u.updateStatusRight()

	right := func() string { return strings.TrimSpace(u.statusRight.GetText(true)) }
	if got := right(); got != "v9.8.7" {
		t.Fatalf("at start: %q, want just the version", got)
	}
	u.resizeSidebar(80) // too narrow for the list beside the tree
	if got := right(); !strings.Contains(got, "c contexts") || !strings.HasSuffix(got, "v9.8.7") {
		t.Fatalf("narrow: %q, want the hint then the version", got)
	}
	u.resizeSidebar(160)
	if got := right(); got != "v9.8.7" {
		t.Fatalf("wide again: %q, want the hint gone", got)
	}
}

// The destructive key in a tab footer is red, not yellow, and it is the LIVE
// key: remap volume_delete to D and the footer shows a red D.
func TestFooterDeleteKeyIsRedAndFollowsTheKeymap(t *testing.T) {
	u := &ui{km: defaultKeybinds()}
	for tab, want := range map[string]string{
		"volumes":    "[red]X[white] delete",
		"secrets":    "[red]X[white] delete",
		"contexts":   "[red]X[white] delete",
		"forwards":   "[red]X[white] stop",
		"containers": "[red]X[white] remove",
	} {
		got := u.helpFor(tab)
		if !strings.Contains(got, want) {
			t.Errorf("%s footer %q lacks %q", tab, got, want)
		}
		if strings.Contains(got, "[yellow]X[white]") {
			t.Errorf("%s footer draws X yellow: %q", tab, got)
		}
	}

	u.km.VolDelete = 'D'
	got := u.helpFor("volumes")
	if !strings.Contains(got, "[red]D[white] delete") {
		t.Errorf("remapped footer %q lacks a red D", got)
	}
	if strings.Contains(got, "X[white] delete") {
		t.Errorf("remapped footer still names X: %q", got)
	}

	// footerKeys: only the marked pair turns red.
	fk := footerKeys("a", "add", dangerKey("d"), "delete")
	if !strings.Contains(fk, "[yellow]a[white] add") || !strings.Contains(fk, "[red]d[white] delete") {
		t.Errorf("footerKeys = %q", fk)
	}
	if strings.Contains(fk, dangerMark) {
		t.Errorf("footerKeys leaked the danger marker: %q", fk)
	}
}
