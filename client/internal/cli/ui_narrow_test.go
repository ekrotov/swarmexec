// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/resolve"
)

// What the tabs show on a small terminal (review T-13, T-17): the columns and
// values that matter must stay on screen at 80 columns.

// drawPrimitive draws p onto a w×h simulation screen and returns its rows,
// trailing blanks trimmed.
func drawPrimitive(t *testing.T, p tview.Primitive, w, h int) []string {
	t.Helper()
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(w, h)
	p.SetRect(0, 0, w, h)
	p.Draw(scr)
	scr.Show()
	cells, cw, _ := scr.GetContents()
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var line []rune
		for x := 0; x < cw; x++ {
			if c := cells[y*cw+x]; len(c.Runes) > 0 {
				line = append(line, c.Runes[0])
			} else {
				line = append(line, ' ')
			}
		}
		rows[y] = strings.TrimRight(string(line), " ")
	}
	return rows
}

// T-13: at 80×24 a long volume name and a long node list are capped, and the
// SIZE column — the reason to open the tab — stays visible, right after the
// name.
func TestVolumesTabKeepsSizeVisibleAt80Columns(t *testing.T) {
	u := tabsUI()
	vt := u.volTab()
	long := vol("a_really_long_stack_name_with_a_longer_volume_name_data", "node-alpha-01", "node-beta-02", "node-gamma-03")
	long.Created = time.Now().Add(-49 * time.Hour)
	hex := vol(strings.Repeat("0123456789abcdef", 4), "node-alpha-01")
	vt.st().vols = []swarmVolume{long, hex}
	vt.st().sizes = map[string]int64{long.Name: 5_243_000, hex.Name: 1_049_000}
	vt.render()

	rows := drawPrimitive(t, vt.table, 80, 24)
	header := rows[0]
	for _, col := range []string{"VOLUME", "SIZE", "NODES", "USED BY", "DRIVER", "AGE"} {
		if !strings.Contains(header, col) {
			t.Errorf("header at 80 columns lacks %s: %q", col, header)
		}
	}
	if strings.Index(header, "SIZE") > strings.Index(header, "DRIVER") {
		t.Errorf("SIZE must come before DRIVER: %q", header)
	}
	body := strings.Join(rows[1:], "\n")
	for _, want := range []string{"5.243MB", "1.049MB", "0123456789ab…", "3: node-alpha-01", "local", "2d"} {
		if !strings.Contains(body, want) {
			t.Errorf("80-column rows lack %q:\n%s", want, body)
		}
	}
	for _, r := range rows {
		if len([]rune(r)) > 80 {
			t.Errorf("row wider than the screen: %q", r)
		}
	}
	if strings.Contains(body, long.Name) {
		t.Errorf("the long name was not capped:\n%s", body)
	}
}

// statusUI is a UI with just the status row wired: context "default" over
// $DOCKER_HOST, one node and no agent, as review T-17 observed it.
func statusUI(t *testing.T) *ui {
	t.Helper()
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	u := treeUI("default", nil, nil)
	u.km = defaultKeybinds()
	u.g = &globalFlags{version: Version{Binary: "review-e87d42c"}}
	u.ctxs = []dockerctx.Context{{Name: "default"}, {Name: "staging"}}
	u.sidebar = tview.NewFlex()
	u.body = tview.NewFlex().AddItem(tview.NewBox(), 0, 1, true).AddItem(nil, 0, 0, false).AddItem(u.sidebar, 0, 0, false)
	u.status = tview.NewTextView().SetDynamicColors(true)
	u.statusRight = tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight)
	u.statusRow = tview.NewFlex().AddItem(u.status, 0, 1, false).AddItem(u.statusRight, 0, 0, false)
	u.clusterText, u.clusterShort = clusterSummary(1, 0), clusterSummaryShort(1, 0)
	return u
}

// T-17: at 80 columns the version goes and the cluster part shortens, every
// value whole and a gap before the right end; at 120 the long form and the
// version are back.
func TestStatusLineFitsAt80And120Columns(t *testing.T) {
	u := statusUI(t)
	line := func(w int) string {
		u.resizeSidebar(w)
		return drawPrimitive(t, u.statusRow, w, 1)[0]
	}

	narrow := line(80)
	for _, want := range []string{"ctx default ($DOCKER_HOST)", "1n · 0/1 ag  ", "c contexts"} {
		if !strings.Contains(narrow, want) {
			t.Errorf("80 columns lack %q: %q", want, narrow)
		}
	}
	if strings.Contains(narrow, "review-e87d42c") || strings.Contains(narrow, "agents") {
		t.Errorf("80 columns still carry the version or the long cluster part: %q", narrow)
	}

	wide := line(120)
	for _, want := range []string{"ctx default ($DOCKER_HOST)", "1 node · 0/1 agents — swarmexec init", "review-e87d42c"} {
		if !strings.Contains(wide, want) {
			t.Errorf("120 columns lack %q: %q", want, wide)
		}
	}
}

// Whatever does not fit is dropped as a whole part, never cut mid-value:
// with forwards and a long context name, the left text plus the right end
// still fit the screen.
func TestStatusLineDropsWholePartsWhenNarrow(t *testing.T) {
	u := statusUI(t)
	u.activeCtx = "a-rather-long-context-name-for-production"
	for i := 0; i < 3; i++ {
		u.forwards.Add("default", resolve.Candidate{}, uint32(8080+i), 80, func() {})
	}
	u.resizeSidebar(80)
	got := strings.TrimSpace(u.status.GetText(true))
	if left := tview.TaggedStringWidth(u.status.GetText(false)); left+u.statusRightWidth > 80 {
		t.Errorf("status %q (%d) + right %d overflow 80 columns", got, left, u.statusRightWidth)
	}
	if !strings.HasSuffix(got, " ag") && !strings.HasSuffix(got, " fwd") && !strings.HasSuffix(got, "production") {
		t.Errorf("the line does not end on a whole part: %q", got)
	}
	u.resizeSidebar(160)
	if got := u.status.GetText(true); !strings.Contains(got, "fwd") || !strings.Contains(got, "agents") {
		t.Errorf("160 columns must show every part in full: %q", got)
	}
}
