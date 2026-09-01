// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"swarmexec/client/internal/clientlog"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) renderNodes() {
	notable := u.notable
	selName := ""
	if row, _ := notable.GetSelection(); row >= 1 {
		if c := notable.GetCell(row, 0); c != nil {
			selName = strings.TrimPrefix(c.Text, "★ ")
		}
	}
	notable.Clear()
	for c, h := range noHeaders {
		notable.SetCell(0, c, headerCell(h))
	}
	selRow := 1
	for i, n := range u.nodeInfos {
		name, nameColor := n.Hostname, tcell.ColorWhite
		if n.Role == "manager" {
			nameColor = tcell.ColorAqua
		}
		if n.Leader {
			name = "★ " + name
		}
		volStr := "…"
		if u.nodeVolsLoaded {
			volStr = fmt.Sprintf("%d", u.nodeVolCounts[n.Hostname])
		}
		notable.SetCell(i+1, 0, tview.NewTableCell(name).SetTextColor(nameColor).SetExpansion(2))
		notable.SetCell(i+1, 1, tview.NewTableCell(orDash(n.Role)).SetExpansion(1))
		notable.SetCell(i+1, 2, tview.NewTableCell(orDash(n.Availability)).SetTextColor(nodeAvailColor(n.Availability)).SetExpansion(1))
		notable.SetCell(i+1, 3, tview.NewTableCell(orDash(n.State)).SetTextColor(nodeStateColor(n.State)).SetExpansion(1))
		notable.SetCell(i+1, 4, tview.NewTableCell(orDash(n.EngineVersion)).SetExpansion(1))
		notable.SetCell(i+1, 5, tview.NewTableCell(fmt.Sprintf("%d", n.Tasks)).SetExpansion(1))
		notable.SetCell(i+1, 6, tview.NewTableCell(volStr).SetExpansion(1))
		notable.SetCell(i+1, 7, tview.NewTableCell(fmt.Sprintf("%d", len(n.Labels))).SetExpansion(1))
		if n.Hostname == selName {
			selRow = i + 1
		}
	}
	if len(u.nodeInfos) > 0 {
		notable.Select(selRow, 0)
	}
}

func (u *ui) loadNodes() {
	app, r, cfg, dcli, f, ctx, notable := u.app, u.r, u.cfg, u.dcli, u.f, u.ctx, u.notable
	notable.Clear()
	for c, h := range noHeaders {
		notable.SetCell(0, c, headerCell(h))
	}
	notable.SetCell(1, 0, tview.NewTableCell("loading…").SetTextColor(tcell.ColorGray))
	go func() {
		start := time.Now()
		list, err := listNodeInfos(ctx, dcli)
		clientlog.Timed("ui.loadNodes", start, err)
		app.QueueUpdateDraw(func() {
			if err != nil {
				notable.Clear()
				for c, h := range noHeaders {
					notable.SetCell(0, c, headerCell(h))
				}
				notable.SetCell(1, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed))
				return
			}
			u.nodeInfos, u.nodeVolsLoaded = list, false
			u.renderNodes()
		})
	}()
	// Per-node volume counts are node-local (agent fan-out), so fill them in
	// asynchronously — the VOLS column shows "…" until they arrive.
	go func() {
		counts := map[string]int{}
		if ns, nerr := r.Nodes(ctx); nerr == nil {
			vs, _ := indexVolumes(ctx, cfg, ns, f.connectTimeout)
			counts = volumeCountsByNode(vs)
		}
		app.QueueUpdateDraw(func() {
			// Mark loaded even on failure so VOLS shows a number, not "…" forever.
			u.nodeVolCounts, u.nodeVolsLoaded = counts, true
			u.renderNodes()
		})
	}()
}

func (u *ui) selectedNode() (swarmNodeInfo, bool) {
	notable := u.notable
	return selectedRow(notable, u.nodeInfos)
}

func (u *ui) editNodeLabels(n swarmNodeInfo, after func()) {
	app, pages, dcli, ctx, notable := u.app, u.pages, u.dcli, u.ctx, u.notable
	cur := kvPairs(n.Labels)
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" labels of node %s ", n.Hostname))
	render := func() {
		i := list.GetCurrentItem()
		list.Clear()
		if len(cur) == 0 {
			list.AddItem("[gray](no labels)[-]", "", 0, nil)
		}
		for _, it := range cur {
			list.AddItem(tview.Escape(it), "", 0, nil)
		}
		if i < list.GetItemCount() {
			list.SetCurrentItem(i)
		}
	}
	render()
	_, restoreHelp := u.pushOverlayHelp(footerKeys("a", "add", "e", "edit", "d", "delete", "w", "apply", "Esc", "cancel"))
	closeEd := func() { restoreHelp(); pages.RemovePage(pageNodeLabels); app.SetFocus(notable) }
	promptLabel := func(initial string, done func(string)) {
		in := tview.NewInputField().SetLabel("key=value: ").SetText(initial).SetFieldWidth(44)
		in.SetDoneFunc(func(k tcell.Key) {
			pages.RemovePage(pageNodeLabelPrompt)
			app.SetFocus(list)
			if k != tcell.KeyEnter {
				return
			}
			txt := strings.TrimSpace(in.GetText())
			if txt == "" {
				return
			}
			if _, _, err := parseLabel(txt); err != nil {
				u.info(err.Error())
				return
			}
			done(txt)
			render()
		})
		in.SetBorder(true)
		pages.AddPage(pageNodeLabelPrompt, centeredPrompt(in, 60), true, true)
		app.SetFocus(in)
	}
	apply := func() {
		lbls, err := labelsFromStrings(cur)
		if err != nil {
			u.info(err.Error())
			return
		}
		u.confirm(fmt.Sprintf("Update labels on node %s?\n\nApplies immediately (no rolling update).", n.Hostname), "Apply", list, func() {
			go func() {
				err := setNodeLabels(ctx, dcli, n.ID, lbls)
				app.QueueUpdateDraw(func() {
					if err != nil {
						u.info("update failed: " + err.Error())
						app.SetFocus(list)
						return
					}
					closeEd()
					u.info(fmt.Sprintf("node %s labels updated", n.Hostname))
					if after != nil {
						after()
					}
				})
			}()
		})
	}
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape:
			closeEd()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
			promptLabel("", func(v string) { cur = append(cur, v) })
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'e':
			if i := list.GetCurrentItem(); i >= 0 && i < len(cur) {
				promptLabel(cur[i], func(v string) { cur[i] = v })
			}
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
			if i := list.GetCurrentItem(); i >= 0 && i < len(cur) {
				cur = append(cur[:i], cur[i+1:]...)
				render()
			}
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'w':
			apply()
			return nil
		}
		return vimListKeys(ev)
	})
	pages.AddPage(pageNodeLabels, centered(list, 70, 16), true, true)
	app.SetFocus(list)
}

func (u *ui) showNodeDetail(n swarmNodeInfo) {
	app, pages, notable := u.app, u.pages, u.notable
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" node %s ", n.Hostname))
	var b strings.Builder
	kv := func(k, v string) { fmt.Fprintf(&b, "  [gray]%-13s[-] %s\n", k, tview.Escape(v)) }
	role := n.Role
	if n.Leader {
		role += " (leader)"
	}
	kv("hostname", n.Hostname)
	kv("id", n.ID)
	kv("role", orDash(role))
	kv("availability", orDash(n.Availability))
	kv("state", orDash(n.State))
	kv("address", orDash(n.Addr))
	kv("engine", orDash(n.EngineVersion))
	kv("platform", orDash(strings.Trim(n.OS+"/"+n.Arch, "/")))
	kv("cpus", formatCPUCores(n.NanoCPUs))
	kv("memory", formatMemBytes(n.MemoryBytes))
	kv("tasks", fmt.Sprintf("%d running", n.Tasks))
	vol := "…"
	if u.nodeVolsLoaded {
		vol = fmt.Sprintf("%d", u.nodeVolCounts[n.Hostname])
	}
	kv("volumes", vol)
	b.WriteString("\n  [gray]labels[-]\n")
	if lbls := kvPairs(n.Labels); len(lbls) > 0 {
		for _, l := range lbls {
			fmt.Fprintf(&b, "    %s\n", tview.Escape(l))
		}
	} else {
		b.WriteString("    [gray](none)[-]\n")
	}
	tv.SetText(b.String())
	_, restoreHelp := u.pushOverlayHelp(footerKeys("l", "edit labels", "j/k", "scroll", "Esc", "close"))
	closeDetail := func() { restoreHelp(); pages.RemovePage(pageNodeDetail); app.SetFocus(notable) }
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')):
			closeDetail()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'l':
			closeDetail()
			u.editNodeLabels(n, u.loadNodes)
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	})
	pages.AddPage(pageNodeDetail, centered(tv, 72, 24), true, true)
	app.SetFocus(tv)
}
