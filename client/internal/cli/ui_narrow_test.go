// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
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
