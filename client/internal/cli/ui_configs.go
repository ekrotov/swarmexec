// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/clientlog"
)

func (u *ui) renderConfigs() {
	cfgtable := u.cfgtable
	selName := ""
	if row, _ := cfgtable.GetSelection(); row >= 1 {
		if c := cfgtable.GetCell(row, 0); c != nil {
			selName = c.Text
		}
	}
	cfgtable.Clear()
	for c, h := range cfHeaders {
		cfgtable.SetCell(0, c, headerCell(h))
	}
	selRow := 1
	for i, c := range u.cfgs {
		r := i + 1
		used := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if n := len(c.Services); n > 0 {
			used = tview.NewTableCell(fmt.Sprintf("%d", n)).SetTextColor(tcell.ColorGreen).SetExpansion(1)
		}
		labels := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if n := len(c.Labels); n > 0 {
			labels = tview.NewTableCell(fmt.Sprintf("%d", n)).SetExpansion(1)
		}
		cfgtable.SetCell(r, 0, tview.NewTableCell(c.Name).SetExpansion(1))
		cfgtable.SetCell(r, 1, used)
		cfgtable.SetCell(r, 2, tview.NewTableCell(humanBytes(int64(c.Size))).SetExpansion(1))
		cfgtable.SetCell(r, 3, tview.NewTableCell(volumeAge(c.Created)).SetExpansion(1))
		cfgtable.SetCell(r, 4, tview.NewTableCell(volumeAge(c.Updated)).SetExpansion(1))
		cfgtable.SetCell(r, 5, labels)
		if c.Name == selName {
			selRow = r
		}
	}
	if len(u.cfgs) > 0 {
		cfgtable.Select(selRow, 0)
	}
}

func (u *ui) loadConfigs() {
	app, dcli, ctx, cfgtable := u.app, u.dcli, u.ctx, u.cfgtable
	header := func() {
		cfgtable.Clear()
		for c, h := range cfHeaders {
			cfgtable.SetCell(0, c, headerCell(h))
		}
	}
	header()
	cfgtable.SetCell(1, 0, tview.NewTableCell("loading…").SetTextColor(tcell.ColorGray))
	go func() {
		start := time.Now()
		list, err := listConfigs(ctx, dcli)
		clientlog.Timed("ui.loadConfigs", start, err)
		app.QueueUpdateDraw(func() {
			if err != nil {
				header()
				cfgtable.SetCell(1, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
				return
			}
			u.cfgs = list
			u.renderConfigs()
		})
	}()
}

func (u *ui) selectedConfig() (swarmConfig, bool) {
	return selectedRow(u.cfgtable, u.cfgs)
}

// showConfigDetail shows a config's metadata, the services that mount it, and —
// unlike a secret — its actual content. That is the point of this tab: the API
// returns a config's payload, so the operator can see what a service really
// receives without dropping to `docker config inspect`. The content is fetched
// on demand, since a config can be a whole nginx.conf.
func (u *ui) showConfigDetail(c swarmConfig) {
	app, pages, dcli, ctx := u.app, u.pages, u.dcli, u.ctx
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" config %s ", c.Name))

	var b strings.Builder
	kv := func(k, v string) { fmt.Fprintf(&b, "  [gray]%-10s[-] %s\n", k, tview.Escape(v)) }
	kv("name", c.Name)
	kv("id", c.ID)
	kv("size", humanBytes(int64(c.Size)))
	kv("created", volumeAge(c.Created))
	kv("updated", volumeAge(c.Updated))

	b.WriteString("\n  [gray]used by[-]\n")
	if len(c.Services) == 0 {
		b.WriteString("    [gray](no service mounts this config)[-]\n")
	} else {
		for _, s := range c.Services {
			fmt.Fprintf(&b, "    %s\n", tview.Escape(s))
		}
	}
	if lbls := kvPairs(c.Labels); len(lbls) > 0 {
		b.WriteString("\n  [gray]labels[-]\n")
		for _, l := range lbls {
			fmt.Fprintf(&b, "    %s\n", tview.Escape(l))
		}
	}
	b.WriteString("\n  [gray]content[-]\n    [gray]loading…[-]\n")
	head := b.String()
	tv.SetText(head)

	prev := app.GetFocus()
	_, restore := u.pushOverlayHelp(footerKeys("j/k", "scroll", "g/G", "top/bottom", "Esc", "close"))
	closeIt := func() { restore(); pages.RemovePage(pageConfigDetail); app.SetFocus(prev) }
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == u.km.Quit):
			closeIt()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	})
	pages.AddPage(pageConfigDetail, centered(tv, 96, 28), true, true)
	app.SetFocus(tv)

	go func() {
		data, err := configContent(ctx, dcli, c.ID)
		app.QueueUpdateDraw(func() {
			if !pages.HasPage(pageConfigDetail) {
				return
			}
			body := head[:strings.LastIndex(head, "    [gray]loading…[-]\n")]
			switch {
			case err != nil:
				tv.SetText(body + "    [red]could not read: " + tview.Escape(err.Error()) + "[-]\n")
			case len(data) == 0:
				tv.SetText(body + "    [gray](empty)[-]\n")
			default:
				tv.SetText(body + indentConfigContent(data))
			}
			tv.ScrollToBeginning()
		})
	}()
}

// Caps on what the detail view renders. Both are needed: bounding only the
// bytes still lets a file of very short lines become tens of thousands of
// rendered rows (each gains indentation), which is what actually makes a
// TextView crawl. This is a preview — the whole config is one
// `docker config inspect` away.
const (
	configContentLimit    = 64 << 10
	configContentMaxLines = 500
)

// indentConfigContent renders a config payload for the detail view: indented,
// escaped so its own braces cannot be read as colour tags, and truncated with a
// note rather than silently cut. Binary content is reported, not dumped.
func indentConfigContent(data []byte) string {
	if !isProbablyText(data) {
		return fmt.Sprintf("    [gray](binary content, %s — not shown)[-]\n", humanBytes(int64(len(data))))
	}
	truncated := ""
	if len(data) > configContentLimit {
		data = data[:configContentLimit]
		truncated = "… truncated at " + humanBytes(configContentLimit)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > configContentMaxLines {
		lines = lines[:configContentMaxLines]
		truncated = fmt.Sprintf("… truncated at %d lines", configContentMaxLines)
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString("    " + tview.Escape(line) + "\n")
	}
	if truncated != "" {
		fmt.Fprintf(&b, "    [gray]%s[-]\n", truncated)
	}
	return b.String()
}

// isProbablyText reports whether a payload looks like text: no NUL bytes in the
// portion we would render. A config is normally a config file, but it can hold
// anything.
func isProbablyText(data []byte) bool {
	n := len(data)
	if n > 8<<10 {
		n = 8 << 10 // sampling the head is enough to spot binary
	}
	for _, b := range data[:n] {
		if b == 0 {
			return false
		}
	}
	return true
}
