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
	subject statsSubject                       // what the STATS view measures
	fetch   func() ([]inspLine, string, error) // pure data source (see inspect.go)

	table      *tview.Table
	tabs       *tview.TextView // the view strip, pinned above the scrolling table
	frame      *tview.Flex     // strip + table; carries the border and title
	lines      []inspLine
	rawJSON    string
	plain      []string        // plain text of each current row, for copy
	rowNet     map[int]string  // table row -> network name, for collapsible net rows
	rowUpgrade map[int]string  // table row -> target image ref, for the upgrade row
	hasUpgrade bool            // an upgrade row is present (for the footer hint)
	upInfo     *upgradeInfo    // the upgrade's picker data, so "u" works from any row
	expanded   map[string]bool // which networks are expanded
	loaded     bool
	mode       inspMode

	setHelp     func(string)
	restoreHelp func()
}

// newInspectWidgets builds the overlay's three widgets and their nesting. The
// border belongs to the FRAME, not the table: that is what keeps the tab strip
// inside the box and pinned while the table scrolls beneath it.
//
// It is a function of its own so the tests build the same arrangement the app
// does — populate writes to all three, and a test double missing one of them
// would only fail at the moment it is dereferenced.
func newInspectWidgets() (*tview.Table, *tview.TextView, *tview.Flex) {
	table := tview.NewTable().SetSelectable(true, false)
	tabs := tview.NewTextView().SetDynamicColors(true)
	frame := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tabs, 1, 0, false).
		AddItem(table, 0, 1, true)
	frame.SetBorder(true)
	return table, tabs, frame
}

// showInspect opens an inspect overlay for one entity. title/op are display and
// logging labels; editSvc is the service name for a service inspect (enabling the
// editor keys) or "" for a task/container; fetch yields the structured + raw
// views. It builds an inspectView and hands off to its methods.
func (u *ui) showInspect(title string, op string, editSvc string, subject statsSubject, fetch func() ([]inspLine, string, error)) {
	table, tabs, frame := newInspectWidgets()
	iv := &inspectView{
		u:          u,
		title:      title,
		op:         op,
		editSvc:    editSvc,
		subject:    subject,
		fetch:      fetch,
		table:      table,
		tabs:       tabs,
		frame:      frame,
		rowNet:     map[int]string{},
		rowUpgrade: map[int]string{},
		expanded:   map[string]bool{},
		setHelp:    func(string) {},
	}
	iv.open()
}

// inspMode is which of the overlay's three views is showing. "t" cycles them
// rather than each getting its own key: they are the same object read three
// ways, and the footer names whichever comes next.
type inspMode int

const (
	inspModeTable inspMode = iota // the operator-first tabular summary
	inspModeStats                 // live resource usage (see stats.go)
	inspModeRaw                   // the raw daemon JSON
)

// next cycles table -> stats -> raw -> table.
func (m inspMode) next() inspMode { return (m + 1) % 3 }

func (m inspMode) label() string {
	switch m {
	case inspModeStats:
		return "stats"
	case inspModeRaw:
		return "raw json"
	default:
		return "table"
	}
}

// inspModes is the strip's order, and the order the digits 1-3 select.
var inspModes = []inspMode{inspModeTable, inspModeStats, inspModeRaw}

// inspTabStrip renders the overlay's views as a tab bar, in the same shape as
// the main window's: the label, then the digit that selects it, the current one
// in the accent colour.
//
// It exists because naming only the NEXT view — which is all the footer's "t"
// hint could do — hid the whole feature. An operator who never pressed "t"
// twice had no way to learn there was a resource-usage view at all, and the one
// hint they did see ("t raw json", once they were in stats) described something
// else. Three named tabs say what the overlay holds without anyone having to
// step through it.
func inspTabStrip(active inspMode) string {
	var b strings.Builder
	for i, m := range inspModes {
		lbl, num := "[#94a3b8]", "[#64748b]"
		if m == active {
			lbl, num = "[#2dd4bf::b]", "[#2dd4bf::b]"
		}
		fmt.Fprintf(&b, "  %s%s[-::B] %s%d[-::B] ", lbl, m.label(), num, i+1)
	}
	return b.String()
}

// statsSubject says what the STATS view should measure: a whole service (every
// container of it) or one container.
type statsSubject struct {
	service     string
	containerID string
}

// keysText builds the footer key hints for the current mode.
func (iv *inspectView) keysText() string {
	// Front-load the escape hatches ("? help" and "Esc/q close") so that,
	// when this dense line overflows a narrow terminal, it is the tail of
	// actions that clips — never the way out or the pointer to the full key
	// list. "?" opens the complete reference (showHelp).
	//
	// The views are named by the strip at the top of the overlay, so the footer
	// only has to say how to reach them. It used to name the next one instead,
	// which was the only place they were named at all — and it could never show
	// more than one of the three.
	parts := []string{"[yellow]?[white] help", "[yellow]Esc/q[white] close", "[yellow]j/k[white] move", "[yellow]y/Enter[white] copy line", "[yellow]1-3/t[white] view"}
	if iv.editSvc != "" {
		// The ~14 editor actions live behind the "a" menu (showActions);
		// the footer stays short. X (destructive) stays a bare key.
		parts = append(parts, "[yellow]a[white] actions", "[red]X[white] remove")
		// "u" is offered whenever a version can be set at all, not just when a
		// newer one exists — pinning or rolling back to a specific tag is just
		// as valid. The wording distinguishes the two.
		switch {
		case iv.hasUpgrade:
			parts = append(parts, "[yellow]u[white] update version")
		case iv.upInfo != nil:
			parts = append(parts, "[yellow]u[white] set version")
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
	// The strip names the current view, so the title no longer repeats it.
	iv.frame.SetTitle(" inspect " + iv.title + " ")
	iv.tabs.SetText(inspTabStrip(iv.mode))
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
	switch iv.mode {
	case inspModeRaw:
		for _, ln := range strings.Split(iv.rawJSON, "\n") {
			put(ln, tcell.ColorWhite, false, true, ln, "")
		}
	case inspModeStats:
		for _, ln := range iv.statsLines() {
			put(ln.text, ln.color, ln.bold, ln.text != "", strings.TrimSpace(ln.text), "")
		}
	default:
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
				// Two drill-down levels: the network row expands to its DNS names,
				// and below them a "N containers" row expands to the addresses of
				// the containers behind the VIP.
				head := fmt.Sprintf("  %s %s%s", expandMarker(iv.expanded[ln.Net]), ln.Text, lockIcon(ln.Encrypted))
				if ln.Addr != "" {
					head += fmt.Sprintf("  %s %s", ln.AddrLabel, ln.Addr)
				}
				if ln.Note != "" {
					head += "  (" + ln.Note + ")"
				} else {
					head += fmt.Sprintf("  (%d dns names)", ln.Count)
				}
				// Copying a network row yields the address rather than the name:
				// it is the one value on the row you paste somewhere else.
				put(head, tcell.ColorWhite, false, true, netRowCopy(ln), ln.Net)
				if !iv.expanded[ln.Net] {
					break
				}
				for _, c := range ln.Children {
					put("      "+c, tcell.ColorGray, false, true, c, "")
				}
				if len(ln.Tasks) > 0 {
					key := netTasksKey(ln.Net)
					put(fmt.Sprintf("      %s %s", expandMarker(iv.expanded[key]), plural(len(ln.Tasks), "container")),
						tcell.ColorWhite, false, true, "", key)
					if iv.expanded[key] {
						for _, t := range ln.Tasks {
							put("          "+t.Text, tcell.ColorGray, false, true, t.Copy, "")
						}
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
			case inspImage:
				// Renders like any field, but it is what makes "u" available even
				// when no newer version exists: pinning or rolling back to a
				// specific tag is a normal thing to want.
				put(ln.Text, tcell.ColorWhite, false, true, strings.TrimSpace(ln.Text), "")
				if ln.UpRepo != "" && iv.upInfo == nil {
					iv.upInfo = &upgradeInfo{
						repo:    ln.UpRepo,
						current: ln.UpCurrent,
						newer:   ln.UpNewer,
						all:     ln.UpAll,
					}
				}
			case inspUpdate:
				color := tcell.ColorYellow // updating
				if ln.UpdateState == "rollback_started" {
					color = tcell.ColorOrange
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

// netTasksKey is the collapse key of a network's container drill-down, kept
// distinct from the network's own key so the two levels toggle independently.
// A NUL cannot occur in a Docker network name, so it can never collide.
func netTasksKey(net string) string { return "\x00containers\x00" + net }

// plural renders a count with its noun: "1 container", "3 containers".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func expandMarker(expanded bool) string {
	if expanded {
		return "-"
	}
	return "+"
}

func lockIcon(encrypted bool) string {
	if encrypted {
		return " 🔒"
	}
	return ""
}

// netRowCopy is what y/Enter yields on a network row: its address without the
// mask — the value an operator wants to paste — falling back to the network
// name when the attachment has none.
func netRowCopy(ln inspLine) string {
	if ln.Addr != "" {
		return stripMask(ln.Addr)
	}
	return ln.Text
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
	// Never the text itself. In the raw-JSON view every line of the service
	// inspect is selectable, including "Env": ["POSTGRES_PASSWORD=hunter2" ],
	// so logging the row verbatim persists a secret in cleartext to
	// swarmexec.log — where it outlives the session and travels with any log
	// collection. The row index and length are enough to trace a copy.
	clientlog.L().Debug("inspect copy", "row", r, "chars", len([]rune(txt)))
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

// openRollback rolls the service back to its previous spec. The diff is shown
// first, inside the confirm: "current → previous" is exactly the reverse of the
// diff view, so the operator sees what will be undone before agreeing to it —
// the reason to do this here rather than on the command line.
func (iv *inspectView) openRollback() {
	u := iv.u
	app, dcli, ctx := u.app, u.dcli, u.ctx
	svc := iv.editSvc

	go func() {
		lines, hasPrev, err := serviceDiffLines(ctx, dcli, svc)
		app.QueueUpdateDraw(func() {
			switch {
			case err != nil:
				u.info("rollback: " + err.Error())
				return
			case !hasPrev:
				// Not an error: the service simply has never been updated.
				u.info(fmt.Sprintf("%q has no previous version to roll back to —\nit has not been updated since it was created.", svc))
				return
			}

			u.confirm(rollbackConfirmText(svc, lines), "Roll back", iv.table, func() {
				go func() {
					rerr := rollbackService(ctx, dcli, svc)
					app.QueueUpdateDraw(func() {
						if rerr != nil {
							u.info("rollback failed: " + rerr.Error())
							return
						}
						u.flash(fmt.Sprintf(" [green]✓ rolling back %q[white]", svc))
						iv.reload()
					})
				}()
			})
		})
	}()
}

// rollbackDiffPreviewLines caps the diff shown inside the confirm dialog; the
// full diff stays one key away (d).
const rollbackDiffPreviewLines = 12

// rollbackConfirmText builds the confirm body for a rollback. diffLines come
// from serviceDiffLines and read previous → current; a rollback reverses that,
// so the markers are swapped: what the update ADDED will be removed, and what it
// REMOVED will come back. Getting this the wrong way round would show the
// operator the opposite of what is about to happen, so it is kept pure and
// tested rather than inlined in the overlay.
func rollbackConfirmText(svc string, diffLines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Roll %q back to its previous version?\n\n", svc)
	switch {
	case len(diffLines) == 0:
		b.WriteString("[gray]No field-level differences between the versions\n(only metadata changed).[white]\n")
	default:
		b.WriteString("This will undo:\n")
		shown := 0
		for _, l := range diffLines {
			if shown >= rollbackDiffPreviewLines {
				fmt.Fprintf(&b, "[gray]… and %d more (press d for the full diff)[white]\n", len(diffLines)-shown)
				break
			}
			switch {
			case strings.HasPrefix(l, "+ "): // added by the update → removed again
				b.WriteString("[red]- " + tview.Escape(strings.TrimPrefix(l, "+ ")) + "[white]\n")
			case strings.HasPrefix(l, "- "): // removed by the update → restored
				b.WriteString("[green]+ " + tview.Escape(strings.TrimPrefix(l, "- ")) + "[white]\n")
			default:
				b.WriteString(tview.Escape(l) + "\n")
			}
			shown++
		}
	}
	b.WriteString("\n[gray]The manager performs a rolling rollback using the\nservice's own rollback settings.[white]")
	return b.String()
}

// openVersionPicker opens the image-version picker for this service. Shared by
// the "u" key, Enter on the upgrade row, and the actions menu, so all three
// reach the same flow rather than three spellings of it.
func (iv *inspectView) openVersionPicker() {
	if iv.upInfo == nil || iv.editSvc == "" {
		return
	}
	iv.u.openImageVersionPicker(iv.editSvc, *iv.upInfo, iv.table, iv.reload)
}

// explainNoVersionPicker says why a version cannot be chosen for this image.
//
// Both causes are outside the operator's view, which is exactly why silence was
// the wrong answer: either the reference is not one that names a version we can
// resolve, or the registry would not list the repo's tags — commonly a private
// registry the client has no credentials for.
func (iv *inspectView) explainNoVersionPicker() {
	iv.u.info("No version can be set for this image.\n\n" +
		"Setting a version needs two things: an image reference that names a tag " +
		"(or a digest-pinned :latest), and a registry that will list the repo's " +
		"tags. One of them is missing here — most often the registry: a private " +
		"one the client has no credentials for returns nothing, and swarmexec " +
		"will not guess which versions exist.\n\n" +
		"The image can still be changed from the CLI:\n" +
		"  docker service update --image <repo>:<tag> " + iv.editSvc)
}

// serviceAction is one entry of the actions menu: what it is called and what it
// does. Separated from the widget so the menu's CONTENTS can be asserted — the
// bug this structure exists to prevent was an action that simply was not in the
// list, which no test of the list widget would have caught.
type serviceAction struct {
	label string
	run   func()
}

// serviceActions is everything the actions menu offers, in order.
func (iv *inspectView) serviceActions() []serviceAction {
	u, editSvc, table := iv.u, iv.editSvc, iv.table
	var out []serviceAction
	add := func(label string, fn func()) { out = append(out, serviceAction{label, fn}) }

	// Changing the image version is the most ordinary service change there is,
	// and until now it had no entry here — it was reachable only from the bare
	// "u" key, which is named in the inspect footer and nowhere else. Someone
	// looking for "how do I put this service on version X" opens this menu,
	// finds "Roll back to the previous version" (a different thing: the previous
	// SPEC, not a version you choose) and concludes it cannot be done.
	//
	// It sits first because it is the most-wanted, and its label distinguishes
	// the two cases the footer already distinguishes: a newer version exists, or
	// you are pinning/rolling back to one of your choosing.
	switch {
	case iv.upInfo != nil && iv.hasUpgrade:
		add("Update image version…", iv.openVersionPicker)
	case iv.upInfo != nil:
		add("Set image version…", iv.openVersionPicker)
	default:
		// Deliberately listed rather than hidden. An absent entry is what sent
		// the operator looking in the first place; this one answers the question
		// instead of leaving the menu silent about it.
		add("Set image version — unavailable for this image", iv.explainNoVersionPicker)
	}
	add("Diff spec (previous → current)", iv.openDiff)
	add("Roll back to the previous version", iv.openRollback)
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
	add("[red]Remove service[white]", func() {
		u.openRemoveService(editSvc, table, func() { iv.close(); u.loadContainers() })
	})
	return out
}

// showActions groups every service editor/action behind one menu (opened with
// "a"), so the footer needn't spell out ~14 case-sensitive keys (d/D, s/S, p/P
// …). The direct keys still work for power users and are listed under "?"; this
// is the discoverable, no-Shift path. Service-only.
func (iv *inspectView) showActions() {
	u := iv.u
	app, pages := u.app, u.pages
	table := iv.table
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" actions — %s ", iv.editSvc))
	_, restoreHelp := u.pushOverlayHelp(footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
	closeActions := func() { restoreHelp(); pages.RemovePage(pageInspectActions); app.SetFocus(table) }
	for _, a := range iv.serviceActions() {
		run := a.run
		list.AddItem(a.label, "", 0, func() { closeActions(); run() })
	}
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
			iv.openVersionPicker()
		} else {
			iv.copyLine()
		}
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'u':
		// Available from any row, not just the (top) upgrade row — the hint
		// sits at the top precisely so the operator never has to hunt for it.
		iv.openVersionPicker()
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'y':
		iv.copyLine()
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 't':
		iv.mode = iv.mode.next()
		iv.populate()
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() >= '1' && ev.Rune() <= '3':
		// The digits the strip shows, selecting a view directly — the same
		// bargain the main window's tabs offer. No other key in this overlay
		// takes a digit.
		iv.mode = inspModes[ev.Rune()-'1']
		iv.populate()
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
		iv.openDiff()
		return nil
	case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'R':
		iv.openRollback()
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
	pages.AddPage(pageInspect, centered(iv.frame, 110, 40), true, true)
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
			statsSubject{containerID: c.ContainerID},
			func() ([]inspLine, string, error) { return taskInspectViews(ctx, dcli, c.TaskID) })
		return
	}
	if ref, ok := n.GetReference().(svcRef); ok {
		u.showInspect(fmt.Sprintf("service %s", ref.name), "ui.inspect.service", ref.name,
			statsSubject{service: ref.name},
			func() ([]inspLine, string, error) { return serviceInspectViews(ctx, dcli, ref.name, u.regCache) })
	}
}

// statsRow is one rendered line of the STATS view. It is deliberately not an
// inspLine: that type carries the tabular view's collapse/upgrade machinery,
// none of which applies here.
type statsRow struct {
	text  string
	color tcell.Color
	bold  bool
}

// statsLines renders the live resource usage of the inspected object as a
// TABLE rather than a block per container.
//
// The readings come from the same background fetch that badges the tree
// (ui.loadUsage), so opening this view costs nothing extra and the numbers
// refresh on their own while it is open.
//
// A table because a bare percentage is not readable on its own: "94%" says
// nothing about the scale it is measured against, and stacked blocks make two
// replicas hard to compare. Every cell here carries its own units — "1.80 /
// 2.00 cores" needs no legend — and the percentage sits next to the figure it
// is a percentage of.
func (iv *inspectView) statsLines() []statsRow {
	u := iv.u
	var rows []statsRow
	head := func(s string) { rows = append(rows, statsRow{text: s, color: tcell.ColorAqua, bold: true}) }
	dim := func(s string) { rows = append(rows, statsRow{text: s, color: tcell.ColorGray}) }
	blank := func() { rows = append(rows, statsRow{}) }

	cands := iv.subject.containers(u.lastCands)
	head("RESOURCE USAGE")
	if len(cands) == 0 {
		dim("  (no running container to measure)")
		return rows
	}
	dim("  measured on each node, refreshed with the tree")
	blank()

	cells, measured := statsTableCells(cands, u.usage)
	widths := columnWidths(cells)
	for i, row := range cells {
		text := "  " + padRow(row, widths)
		switch {
		case i == 0:
			rows = append(rows, statsRow{text: text, color: tcell.ColorAqua, bold: true})
		default:
			rows = append(rows, statsRow{text: text, color: tcell.ColorWhite})
		}
	}

	blank()
	// Without this the "of what" is invisible, which is the whole reason the
	// numbers get shown as "used / allowed" in the first place.
	dim("  limits are the container's own where it has one, otherwise the node's capacity")
	if measured < len(cands) {
		dim(fmt.Sprintf("  %d of %d containers reported no reading — an unreachable node,",
			len(cands)-measured, len(cands)))
		dim("  an agent older than this release, or a container started since the last sample")
	}

	if measured > 1 && iv.subject.service != "" {
		if agg, ok := serviceUsage(u.usage, u.lastCands, iv.subject.service); ok {
			blank()
			head("ACROSS " + fmt.Sprint(measured) + " CONTAINERS")
			// The worst replica, not the average: an average hides the one that is
			// about to be OOM-killed, which is the container you opened this for.
			rows = append(rows, statsRow{
				text:  fmt.Sprintf("  %-10s %s of its limit (worst replica)", "cpu", formatRatio(agg.CPURatio)),
				color: tcell.ColorWhite,
			})
			rows = append(rows, statsRow{
				text: fmt.Sprintf("  %-10s %s of its limit (worst replica) · %s in total",
					"memory", formatRatio(agg.MemRatio), humanBytes(agg.MemBytes)),
				color: tcell.ColorWhite,
			})
		}
	}
	return rows
}

// statsTableCells builds the header row plus one row per container. Kept pure
// so the column contents can be tested without a screen.
func statsTableCells(cands []resolve.Candidate, usage map[string]containerUsage) (cells [][]string, measured int) {
	cells = [][]string{{"CONTAINER", "NODE", "HEALTH", "CPU", "MEMORY"}}
	for _, c := range cands {
		name := shortID(c.ContainerID)
		if c.Slot > 0 {
			name = fmt.Sprintf("%s.%d", orDash(c.Service), c.Slot)
		}
		u, ok := usage[c.ContainerID]
		if !ok {
			cells = append(cells, []string{name, orDash(c.NodeName), "-", "no reading", "no reading"})
			continue
		}
		measured++
		cells = append(cells, []string{
			name, orDash(c.NodeName), healthCell(u.Health),
			formatCPUUsage(u), formatMemUsage(u),
		})
	}
	return cells, measured
}

// healthCell words the verdict for the table. "no healthcheck" is not the same
// as healthy and must not render as a blank, which would read as a pass.
func healthCell(health string) string {
	switch health {
	case healthUnhealthy:
		return "unhealthy"
	case healthStarting:
		return "starting"
	case healthHealthy:
		return "healthy"
	default:
		return "none"
	}
}

// columnWidths measures each column across every row, so the table lines up.
func columnWidths(cells [][]string) []int {
	var w []int
	for _, row := range cells {
		for i, cell := range row {
			for len(w) <= i {
				w = append(w, 0)
			}
			if n := len(cell); n > w[i] {
				w[i] = n
			}
		}
	}
	return w
}

// padRow pads a row to the measured widths. The last column is not padded, so a
// selected row's highlight does not run past the text.
func padRow(row []string, widths []int) string {
	var b strings.Builder
	for i, cell := range row {
		if i > 0 {
			b.WriteString("  ")
		}
		if i == len(row)-1 {
			b.WriteString(cell)
			break
		}
		fmt.Fprintf(&b, "%-*s", widths[i], cell)
	}
	return strings.TrimRight(b.String(), " ")
}

// containers picks the candidates this subject covers: every replica of a
// service, or the single container of a task inspect.
func (s statsSubject) containers(all []resolve.Candidate) []resolve.Candidate {
	var out []resolve.Candidate
	for _, c := range all {
		switch {
		case s.containerID != "" && c.ContainerID == s.containerID:
			out = append(out, c)
		case s.containerID == "" && s.service != "" && c.Service == s.service:
			out = append(out, c)
		}
	}
	return out
}
