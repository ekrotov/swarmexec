// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/resolve"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) showInspect(title string, op string, editSvc string, fetch func() ([]inspLine, string, error)) {
	app, pages, dcli, ctx, ctree := u.app, u.pages, u.dcli, u.ctx, u.ctree
	table := tview.NewTable().SetSelectable(true, false)
	table.SetBorder(true)
	var lines []inspLine
	var rawJSON string
	var plain []string             // plain text of each current row, for copy
	rowNet := map[int]string{}     // table row -> network name, for collapsible net rows
	rowUpgrade := map[int]string{} // table row -> target image ref, for the upgrade row
	hasUpgrade := false            // an upgrade row is present (for the footer hint)
	expanded := map[string]bool{}  // which networks are expanded
	loaded := false
	showRaw := false

	keysText := func() string {
		toggle := "raw JSON"
		if showRaw {
			toggle = "table"
		}
		// Front-load the escape hatches ("? help" and "Esc/q close") so that,
		// when this dense line overflows a narrow terminal, it is the tail of
		// actions that clips — never the way out or the pointer to the full key
		// list. "?" opens the complete reference (showHelp).
		parts := []string{"[yellow]?[white] help", "[yellow]Esc/q[white] close", "[yellow]j/k[white] move", "[yellow]y/Enter[white] copy line", "[yellow]t[white] " + toggle}
		if editSvc != "" {
			// The ~14 editor actions live behind the "a" menu (showActions);
			// the footer stays short. X (destructive) stays a bare key.
			parts = append(parts, "[yellow]a[white] actions", "[red]X[white] remove")
			if hasUpgrade {
				parts = append(parts, "[yellow]u[white] update-to-latest")
			}
		}
		return " " + strings.Join(parts, "  ")
	}
	// The single bottom footer shows this overlay's keys; pushed on open below.
	var setHelp func(string) = func(string) {}
	var restoreHelp func()
	setFooter := func(status string) {
		t := keysText()
		if status != "" {
			t = " " + status + "  ·" + t
		}
		setHelp(t)
	}
	populate := func() {
		mode := "table"
		if showRaw {
			mode = "raw json"
		}
		table.SetTitle(fmt.Sprintf(" inspect %s — %s ", title, mode))
		keepRow, _ := table.GetSelection()
		table.Clear()
		plain = plain[:0]
		for k := range rowNet {
			delete(rowNet, k)
		}
		for k := range rowUpgrade {
			delete(rowUpgrade, k)
		}
		hasUpgrade = false
		if !loaded {
			table.SetCell(0, 0, tview.NewTableCell("loading…").SetSelectable(false))
			plain = append(plain, "")
			setFooter("")
			return
		}
		firstSel, r := -1, 0
		put := func(text string, color tcell.Color, bold, selectable bool, plainText, net string) {
			cell := tview.NewTableCell(tview.Escape(text)).SetTextColor(color).SetSelectable(selectable)
			if bold {
				cell.SetAttributes(tcell.AttrBold)
			}
			table.SetCell(r, 0, cell)
			plain = append(plain, plainText)
			if net != "" {
				rowNet[r] = net
			}
			if selectable && firstSel < 0 {
				firstSel = r
			}
			r++
		}
		if showRaw {
			for _, ln := range strings.Split(rawJSON, "\n") {
				put(ln, tcell.ColorWhite, false, true, ln, "")
			}
		} else {
			for _, ln := range lines {
				switch ln.Kind {
				case inspTitle:
					put(ln.Text, tcell.ColorAqua, true, false, ln.Text, "")
				case inspHeader:
					put(ln.Text, tcell.ColorAqua, false, false, ln.Text, "")
				case inspDim:
					put(ln.Text, tcell.ColorGray, false, false, ln.Text, "")
				case inspBlank:
					put("", tcell.ColorWhite, false, false, "", "")
				case inspNet:
					marker := "+"
					if expanded[ln.Net] {
						marker = "-"
					}
					lock := ""
					if ln.Encrypted {
						lock = " 🔒"
					}
					put(fmt.Sprintf("  %s %s%s (%d dns names)", marker, ln.Text, lock, ln.Count), tcell.ColorWhite, false, true, ln.Text, ln.Net)
					if expanded[ln.Net] {
						for _, c := range ln.Children {
							put("      "+c, tcell.ColorGray, false, true, c, "")
						}
					}
				case inspUpgrade:
					up := r // row index this line will occupy
					put("  "+ln.Text, tcell.ColorYellow, true, true, ln.Text, "")
					rowUpgrade[up] = ln.Upgrade
					hasUpgrade = true
				default: // inspField
					put(ln.Text, tcell.ColorWhite, false, true, ln.Text, "")
				}
			}
		}
		table.ScrollToBeginning()
		if firstSel < 0 {
			firstSel = 0
		}
		if keepRow > 0 && keepRow < r {
			table.Select(keepRow, 0)
		} else {
			table.Select(firstSel, 0)
		}
		setFooter("")
	}
	copyLine := func() {
		r, _ := table.GetSelection()
		if r < 0 || r >= len(plain) {
			return
		}
		txt := strings.TrimSpace(plain[r])
		if txt == "" || u.screen == nil {
			return
		}
		u.screen.SetClipboard([]byte(txt))
		clientlog.L().Debug("inspect copy", "text", txt)
		setFooter("[green]✓ copied to clipboard[white]")
	}
	closeInspect := func() {
		if restoreHelp != nil {
			restoreHelp()
		}
		pages.RemovePage(pageInspect)
		app.SetFocus(ctree)
	}
	// reload re-fetches the inspect (after an edit) and refreshes the tree.
	reload := func() {
		u.loadContainers()
		go func() {
			f, raw, err := fetch()
			app.QueueUpdateDraw(func() {
				if err != nil || !pages.HasPage(pageInspect) {
					return
				}
				lines, rawJSON, loaded = f, raw, true
				populate()
			})
		}()
	}
	// openDiff shows a unified diff of the service's current spec against its
	// PreviousSpec (what the last rolling update changed). Service-only.
	openDiff := func() {
		tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
		tv.SetBorder(true).SetTitle(fmt.Sprintf(" diff %s — previous → current ", editSvc))
		_, restoreDiff := u.pushOverlayHelp(footerKeys("j/k", "scroll", "g/G", "top/bottom", "Esc", "close"))
		closeDiff := func() {
			restoreDiff()
			pages.RemovePage(pageInspectDiff)
			app.SetFocus(table)
		}
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'd')) {
				closeDiff()
				return nil
			}
			return ev
		})
		tv.SetText("loading…")
		pages.AddPage(pageInspectDiff, centered(tv, 100, 32), true, true)
		app.SetFocus(tv)
		go func() {
			lines, hasPrev, derr := serviceDiffLines(ctx, dcli, editSvc)
			app.QueueUpdateDraw(func() {
				if !pages.HasPage(pageInspectDiff) {
					return
				}
				switch {
				case derr != nil:
					tv.SetText("[red]error: " + tview.Escape(derr.Error()) + "[white]")
				case !hasPrev:
					tv.SetText("[gray]No previous version to diff — this service has not been updated since it was created.[white]")
				case len(lines) == 0:
					tv.SetText("[gray]No field-level differences between the current and previous spec (only metadata changed).[white]")
				default:
					var b strings.Builder
					b.WriteString("previous → current   ([red]- removed[white]  [green]+ added[white])\n\n")
					for _, l := range lines {
						switch {
						case strings.HasPrefix(l, "+ "):
							b.WriteString("[green]" + tview.Escape(l) + "[white]\n")
						case strings.HasPrefix(l, "- "):
							b.WriteString("[red]" + tview.Escape(l) + "[white]\n")
						default:
							b.WriteString(tview.Escape(l) + "\n")
						}
					}
					tv.SetText(b.String())
					tv.ScrollToBeginning()
				}
			})
		}()
	}
	// showActions groups every service editor/action behind one menu (opened
	// with "a"), so the footer needn't spell out ~14 case-sensitive keys
	// (d/D, s/S, p/P …). The direct keys still work for power users and are
	// listed under "?"; this is the discoverable, no-Shift path. Service-only.
	showActions := func() {
		list := tview.NewList().ShowSecondaryText(false)
		list.SetBorder(true).SetTitle(fmt.Sprintf(" actions — %s ", editSvc))
		_, restoreHelp := u.pushOverlayHelp(footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
		closeActions := func() { restoreHelp(); pages.RemovePage(pageInspectActions); app.SetFocus(table) }
		add := func(label string, fn func()) {
			list.AddItem(label, "", 0, func() { closeActions(); fn() })
		}
		add("Diff spec (previous → current)", openDiff)
		add("Why — placement diagnosis", func() { u.showPlacementDiagnosis(editSvc, table) })
		add("Scale", func() { u.openScalePrompt(editSvc, table, reload) })
		add("Force-update", func() { u.openForceUpdate(editSvc, table, reload) })
		add("Edit ports", func() { u.openPortsEditor(editSvc, table, reload) })
		add("Edit labels", func() { u.openLabelsEditor(editSvc, table, reload) })
		add("Edit env", func() { u.openEnvEditor(editSvc, table, reload) })
		add("Edit networks", func() { u.openNetworksEditor(editSvc, table, reload) })
		add("Edit secrets", func() { u.openSecretsEditor(editSvc, table, reload) })
		add("Edit mounts", func() { u.openMountsEditor(editSvc, table, reload) })
		add("Edit resources", func() { u.openResourcesEditor(editSvc, table, reload) })
		add("Edit placement", func() { u.openPlacementMenu(editSvc, table, reload) })
		add("[red]Remove service[white]", func() { u.openRemoveService(editSvc, table, func() { closeInspect(); u.loadContainers() }) })
		list.AddItem("Cancel", "", 0, closeActions)
		list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape {
				closeActions()
				return nil
			}
			return vimListKeys(ev)
		})
		pages.AddPage(pageInspectActions, centered(list, 54, 17), true, true)
		app.SetFocus(list)
	}
	table.SetSelectionChangedFunc(func(int, int) { setFooter("") })
	table.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')):
			closeInspect()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == '?':
			u.showHelp() // overlays don't route through tabKeys, so wire "?" directly
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
			showActions()
			return nil
		case ev.Key() == tcell.KeyEnter:
			// Enter toggles a collapsible network row, triggers an upgrade row,
			// otherwise copies the line.
			r, _ := table.GetSelection()
			if net, ok := rowNet[r]; ok {
				expanded[net] = !expanded[net]
				populate()
			} else if target, ok := rowUpgrade[r]; ok && editSvc != "" {
				u.openImageUpgrade(editSvc, target, table, reload)
			} else {
				copyLine()
			}
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'u':
			r, _ := table.GetSelection()
			if target, ok := rowUpgrade[r]; ok {
				u.openImageUpgrade(editSvc, target, table, reload)
			}
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'y':
			copyLine()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 't':
			showRaw = !showRaw
			populate()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
			openDiff()
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'D':
			u.showPlacementDiagnosis(editSvc, table)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 's':
			u.openScalePrompt(editSvc, table, reload)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'f':
			u.openForceUpdate(editSvc, table, reload)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'X':
			// Destructive: remove the service, then close inspect (it's gone)
			// and refresh the tree.
			u.openRemoveService(editSvc, table, func() { closeInspect(); u.loadContainers() })
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'p':
			u.openPortsEditor(editSvc, table, reload)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'l':
			u.openLabelsEditor(editSvc, table, reload)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'n':
			u.openNetworksEditor(editSvc, table, reload)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'S':
			u.openSecretsEditor(editSvc, table, reload)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'v':
			u.openMountsEditor(editSvc, table, reload)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'e':
			u.openEnvEditor(editSvc, table, reload)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'r':
			u.openResourcesEditor(editSvc, table, reload)
			return nil
		case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'P':
			u.openPlacementMenu(editSvc, table, reload)
			return nil
		}
		return ev
	})
	setHelp, restoreHelp = u.pushOverlayHelp(keysText())
	populate() // shows "loading…"
	pages.AddPage(pageInspect, centered(table, 110, 40), true, true)
	app.SetFocus(table)
	go func() {
		start := time.Now()
		f, raw, err := fetch()
		clientlog.Timed(op, start, err)
		app.QueueUpdateDraw(func() {
			if !pages.HasPage(pageInspect) {
				return
			}
			if err != nil {
				table.Clear()
				table.SetCell(0, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
				setFooter("")
				return
			}
			lines, rawJSON, loaded = f, raw, true
			populate()
		})
	}()
}

func (u *ui) inspectCurrent() {
	dcli, ctx, ctree := u.dcli, u.ctx, u.ctree
	n := ctree.GetCurrentNode()
	if n == nil {
		return
	}
	if c, ok := n.GetReference().(resolve.Candidate); ok {
		u.showInspect(fmt.Sprintf("task %s (%s)", shortID(c.ContainerID), orDash(c.Service)), "ui.inspect.task", "",
			func() ([]inspLine, string, error) { return taskInspectViews(ctx, dcli, c.TaskID) })
		return
	}
	if ref, ok := n.GetReference().(svcRef); ok {
		u.showInspect(fmt.Sprintf("service %s", ref.name), "ui.inspect.service", ref.name,
			func() ([]inspLine, string, error) { return serviceInspectViews(ctx, dcli, ref.name, u.regCache) })
	}
}
