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

// inspectView is one open inspect overlay: a table rendered from structured
// inspect lines (or the raw JSON), with its own key handling and sub-overlays
// (spec diff, the actions menu, the image version picker). Its state was
// formerly ~11 locals shared by the closures inside showInspect; promoting them
// onto a struct lets the render half (populate) and the input half (handleKey)
// be plain methods instead of one ~375-line closure nest.
type inspectView struct {
	u       *ui
	title   string
	op      string                             // clientlog label for the initial fetch
	editSvc string                             // "" for a task/container inspect — disables the service editor keys
	fetch   func() ([]inspLine, string, error) // pure data source (see inspect.go)

	table      *tview.Table
	lines      []inspLine
	rawJSON    string
	plain      []string        // plain text of each current row, for copy
	rowNet     map[int]string  // table row -> network name, for collapsible net rows
	rowUpgrade map[int]string  // table row -> target image ref, for the upgrade row
	hasUpgrade bool            // an upgrade row is present (for the footer hint)
	upInfo     *upgradeInfo    // the upgrade's picker data, so "u" works from any row
	expanded   map[string]bool // which networks are expanded
	loaded     bool
	showRaw    bool

	setHelp     func(string)
	restoreHelp func()
}

// showInspect opens an inspect overlay for one entity. title/op are display and
// logging labels; editSvc is the service name for a service inspect (enabling the
// editor keys) or "" for a task/container; fetch yields the structured + raw
// views. It builds an inspectView and hands off to its methods.
func (u *ui) showInspect(title string, op string, editSvc string, fetch func() ([]inspLine, string, error)) {
	table := tview.NewTable().SetSelectable(true, false)
	table.SetBorder(true)
	iv := &inspectView{
		u:          u,
		title:      title,
		op:         op,
		editSvc:    editSvc,
		fetch:      fetch,
		table:      table,
		rowNet:     map[int]string{},
		rowUpgrade: map[int]string{},
		expanded:   map[string]bool{},
		setHelp:    func(string) {},
	}
	iv.open()
}

// keysText builds the footer key hints for the current mode.
func (iv *inspectView) keysText() string {
	toggle := "raw JSON"
	if iv.showRaw {
		toggle = "table"
	}
	// Front-load the escape hatches ("? help" and "Esc/q close") so that,
	// when this dense line overflows a narrow terminal, it is the tail of
	// actions that clips — never the way out or the pointer to the full key
	// list. "?" opens the complete reference (showHelp).
	parts := []string{"[yellow]?[white] help", "[yellow]Esc/q[white] close", "[yellow]j/k[white] move", "[yellow]y/Enter[white] copy line", "[yellow]t[white] " + toggle}
	if iv.editSvc != "" {
		// The ~14 editor actions live behind the "a" menu (showActions);
		// the footer stays short. X (destructive) stays a bare key.
		parts = append(parts, "[yellow]a[white] actions", "[red]X[white] remove")
		if iv.hasUpgrade {
			parts = append(parts, "[yellow]u[white] update version")
		}
	}
	return " " + strings.Join(parts, "  ")
}

func (iv *inspectView) setFooter(status string) {
	t := iv.keysText()
	if status != "" {
		t = " " + status + "  ·" + t
	}
	iv.setHelp(t)
}

// populate renders the table from the current lines (or raw JSON) — the view
// half of the overlay. It rebuilds the row→network and row→upgrade maps used by
// handleKey and keeps the selection stable across re-renders.
func (iv *inspectView) populate() {
	table := iv.table
	mode := "table"
	if iv.showRaw {
		mode = "raw json"
	}
	table.SetTitle(fmt.Sprintf(" inspect %s — %s ", iv.title, mode))
	keepRow, _ := table.GetSelection()
	table.Clear()
	iv.plain = iv.plain[:0]
	for k := range iv.rowNet {
		delete(iv.rowNet, k)
	}
	for k := range iv.rowUpgrade {
		delete(iv.rowUpgrade, k)
	}
	iv.hasUpgrade = false
	iv.upInfo = nil
	if !iv.loaded {
		table.SetCell(0, 0, tview.NewTableCell("loading…").SetSelectable(false))
		iv.plain = append(iv.plain, "")
		iv.setFooter("")
		return
	}
	firstSel, r := -1, 0
	put := func(text string, color tcell.Color, bold, selectable bool, plainText, net string) {
		cell := tview.NewTableCell(tview.Escape(text)).SetTextColor(color).SetSelectable(selectable)
		if bold {
			cell.SetAttributes(tcell.AttrBold)
		}
		table.SetCell(r, 0, cell)
		iv.plain = append(iv.plain, plainText)
		if net != "" {
			iv.rowNet[r] = net
		}
		if selectable && firstSel < 0 {
			firstSel = r
		}
		r++
	}
	if iv.showRaw {
		for _, ln := range strings.Split(iv.rawJSON, "\n") {
			put(ln, tcell.ColorWhite, false, true, ln, "")
		}
	} else {
		for _, ln := range iv.lines {
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
				if iv.expanded[ln.Net] {
					marker = "-"
				}
				lock := ""
				if ln.Encrypted {
					lock = " 🔒"
				}
				put(fmt.Sprintf("  %s %s%s (%d dns names)", marker, ln.Text, lock, ln.Count), tcell.ColorWhite, false, true, ln.Text, ln.Net)
				if iv.expanded[ln.Net] {
					for _, c := range ln.Children {
						put("      "+c, tcell.ColorGray, false, true, c, "")
					}
				}
			case inspUpgrade:
				up := r // row index this line will occupy
				put("  "+ln.Text, tcell.ColorYellow, true, true, ln.Text, "")
				iv.rowUpgrade[up] = ln.Upgrade
				iv.hasUpgrade = true
				iv.upInfo = &upgradeInfo{
					repo:    ln.UpRepo,
					current: ln.UpCurrent,
					target:  ln.Upgrade,
					newer:   ln.UpNewer,
					all:     ln.UpAll,
				}
			case inspUpdate:
				color := tcell.ColorYellow
				switch ln.UpdateState {
				case "paused", "rollback_started":
					color = tcell.ColorOrange
				case "rollback_paused":
					color = tcell.ColorRed
				}
				put("  "+ln.Text, color, true, false, ln.Text, "")
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
	iv.setFooter("")
}

func (iv *inspectView) copyLine() {
	r, _ := iv.table.GetSelection()
	if r < 0 || r >= len(iv.plain) {
		return
	}
	txt := strings.TrimSpace(iv.plain[r])
	if txt == "" || iv.u.screen == nil {
		return
	}
	iv.u.screen.SetClipboard([]byte(txt))
	clientlog.L().Debug("inspect copy", "text", txt)
	iv.setFooter("[green]✓ copied to clipboard[white]")
}

func (iv *inspectView) close() {
	if iv.restoreHelp != nil {
		iv.restoreHelp()
	}
	iv.u.pages.RemovePage(pageInspect)
	iv.u.app.SetFocus(iv.u.ctree)
}

// reload re-fetches the inspect (after an edit) and refreshes the tree.
func (iv *inspectView) reload() {
	u := iv.u
	u.loadContainers()
	go func() {
		f, raw, err := iv.fetch()
		u.app.QueueUpdateDraw(func() {
			if err != nil || !u.pages.HasPage(pageInspect) {
				return
			}
			iv.lines, iv.rawJSON, iv.loaded = f, raw, true
			iv.populate()
		})
	}()
}

// openDiff shows a unified diff of the service's current spec against its
// PreviousSpec (what the last rolling update changed). Service-only.
func (iv *inspectView) openDiff() {
	u := iv.u
	app, pages, dcli, ctx := u.app, u.pages, u.dcli, u.ctx
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" diff %s — previous → current ", iv.editSvc))
	_, restoreDiff := u.pushOverlayHelp(footerKeys("j/k", "scroll", "g/G", "top/bottom", "Esc", "close"))
	closeDiff := func() {
		restoreDiff()
		pages.RemovePage(pageInspectDiff)
		app.SetFocus(iv.table)
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
		lines, hasPrev, derr := serviceDiffLines(ctx, dcli, iv.editSvc)
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

// showActions groups every service editor/action behind one menu (opened with
// "a"), so the footer needn't spell out ~14 case-sensitive keys (d/D, s/S, p/P
// …). The direct keys still work for power users and are listed under "?"; this
// is the discoverable, no-Shift path. Service-only.
func (iv *inspectView) showActions() {
	u := iv.u
	app, pages := u.app, u.pages
	editSvc, table := iv.editSvc, iv.table
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" actions — %s ", editSvc))
	_, restoreHelp := u.pushOverlayHelp(footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
	closeActions := func() { restoreHelp(); pages.RemovePage(pageInspectActions); app.SetFocus(table) }
	add := func(label string, fn func()) {
		list.AddItem(label, "", 0, func() { closeActions(); fn() })
	}
	add("Diff spec (previous → current)", iv.openDiff)
	add("Why — placement diagnosis", func() { u.showPlacementDiagnosis(editSvc, table) })
	add("Scale", func() { u.openScalePrompt(editSvc, table, iv.reload) })
	add("Force-update", func() { u.openForceUpdate(editSvc, table, iv.reload) })
	add("Edit ports", func() { u.openPortsEditor(editSvc, table, iv.reload) })
	add("Edit labels", func() { u.openLabelsEditor(editSvc, table, iv.reload) })
	add("Edit env", func() { u.openEnvEditor(editSvc, table, iv.reload) })
	add("Edit networks", func() { u.openNetworksEditor(editSvc, table, iv.reload) })
	add("Edit secrets", func() { u.openSecretsEditor(editSvc, table, iv.reload) })
	add("Edit mounts", func() { u.openMountsEditor(editSvc, table, iv.reload) })
	add("Edit resources", func() { u.openResourcesEditor(editSvc, table, iv.reload) })
	add("Edit placement", func() { u.openPlacementMenu(editSvc, table, iv.reload) })
	add("[red]Remove service[white]", func() { u.openRemoveService(editSvc, table, func() { iv.close(); u.loadContainers() }) })
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

// handleKey is the table's input capture — the input half of the overlay.
func (iv *inspectView) handleKey(ev *tcell.EventKey) *tcell.EventKey {
	u := iv.u
	editSvc, table := iv.editSvc, iv.table
	switch {
	case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')):
		iv.close()
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == '?':
		u.showHelp() // overlays don't route through tabKeys, so wire "?" directly
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
		iv.showActions()
		return nil
	case ev.Key() == tcell.KeyEnter:
		// Enter toggles a collapsible network row, triggers an upgrade row,
		// otherwise copies the line.
		r, _ := table.GetSelection()
		if net, ok := iv.rowNet[r]; ok {
			iv.expanded[net] = !iv.expanded[net]
			iv.populate()
		} else if _, ok := iv.rowUpgrade[r]; ok && editSvc != "" && iv.upInfo != nil {
			u.openImageVersionPicker(editSvc, *iv.upInfo, table, iv.reload)
		} else {
			iv.copyLine()
		}
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'u':
		// Available from any row, not just the (top) upgrade row — the hint
		// sits at the top precisely so the operator never has to hunt for it.
		if iv.upInfo != nil {
			u.openImageVersionPicker(editSvc, *iv.upInfo, table, iv.reload)
		}
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'y':
		iv.copyLine()
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 't':
		iv.showRaw = !iv.showRaw
		iv.populate()
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
		iv.openDiff()
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'D':
		u.showPlacementDiagnosis(editSvc, table)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 's':
		u.openScalePrompt(editSvc, table, iv.reload)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'f':
		u.openForceUpdate(editSvc, table, iv.reload)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'X':
		// Destructive: remove the service, then close inspect (it's gone)
		// and refresh the tree.
		u.openRemoveService(editSvc, table, func() { iv.close(); u.loadContainers() })
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'p':
		u.openPortsEditor(editSvc, table, iv.reload)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'l':
		u.openLabelsEditor(editSvc, table, iv.reload)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'n':
		u.openNetworksEditor(editSvc, table, iv.reload)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'S':
		u.openSecretsEditor(editSvc, table, iv.reload)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'v':
		u.openMountsEditor(editSvc, table, iv.reload)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'e':
		u.openEnvEditor(editSvc, table, iv.reload)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'r':
		u.openResourcesEditor(editSvc, table, iv.reload)
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'P':
		u.openPlacementMenu(editSvc, table, iv.reload)
		return nil
	}
	return ev
}

// open wires the widget to its input/footer and kicks off the initial fetch.
func (iv *inspectView) open() {
	u := iv.u
	app, pages := u.app, u.pages
	table := iv.table
	table.SetSelectionChangedFunc(func(int, int) { iv.setFooter("") })
	table.SetInputCapture(iv.handleKey)
	iv.setHelp, iv.restoreHelp = u.pushOverlayHelp(iv.keysText())
	iv.populate() // shows "loading…"
	pages.AddPage(pageInspect, centered(table, 110, 40), true, true)
	app.SetFocus(table)
	go func() {
		start := time.Now()
		f, raw, err := iv.fetch()
		clientlog.Timed(iv.op, start, err)
		app.QueueUpdateDraw(func() {
			if !pages.HasPage(pageInspect) {
				return
			}
			if err != nil {
				table.Clear()
				table.SetCell(0, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
				iv.setFooter("")
				return
			}
			iv.lines, iv.rawJSON, iv.loaded = f, raw, true
			iv.populate()
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
