// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"swarmexec/client/internal/clientlog"
	"time"

	"github.com/docker/docker/api/types/swarm"
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

// openNodeAvailability offers the three availability states. One menu rather
// than three keys: the states are mutually exclusive, the menu can show which
// one is current, and it leaves the nodes tab's key space free.
//
// Draining is confirmed because it evicts the node's tasks; active and pause are
// applied directly (pause only stops NEW placements, it does not move anything).
func (u *ui) openNodeAvailability(n swarmNodeInfo, back tview.Primitive, after func()) {
	app, pages, dcli, ctx := u.app, u.pages, u.dcli, u.ctx
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" availability — %s ", n.Hostname))
	_, restoreHelp := u.pushOverlayHelp(footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
	closeMenu := func() { restoreHelp(); pages.RemovePage(pageNodeAvail); app.SetFocus(back) }

	apply := func(to swarm.NodeAvailability) {
		go func() {
			err := setNodeAvailability(ctx, dcli, n.ID, to)
			app.QueueUpdateDraw(func() {
				if err != nil {
					u.info(fmt.Sprintf("could not set %s to %s: %v", n.Hostname, to, err))
					return
				}
				u.flash(fmt.Sprintf(" [green]✓ %s is now %s[white]", n.Hostname, to))
				if after != nil {
					after()
				}
			})
		}()
	}
	add := func(label, desc string, to swarm.NodeAvailability, confirmMsg string) {
		if string(to) == n.Availability {
			label += "   [gray](current)[-]"
		}
		list.AddItem(label, desc, 0, func() {
			closeMenu()
			if confirmMsg == "" {
				apply(to)
				return
			}
			u.confirm(confirmMsg, "Drain", back, func() { apply(to) })
		})
	}
	add("Active", "schedule tasks here normally", swarm.NodeAvailabilityActive, "")
	add("Pause", "keep running tasks, place no new ones", swarm.NodeAvailabilityPause, "")
	add("[red]Drain[-]", "move every task off this node", swarm.NodeAvailabilityDrain,
		fmt.Sprintf("Drain %q?\n\nSwarm will stop this node's %d running task(s) and reschedule them on other nodes. Services whose tasks cannot be placed elsewhere will go unschedulable.",
			n.Hostname, n.Tasks))
	list.AddItem("Cancel", "", 0, closeMenu)
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			closeMenu()
			return nil
		}
		return vimListKeys(ev)
	})
	pages.AddPage(pageNodeAvail, centered(list, 62, 13), true, true)
	app.SetFocus(list)
}

// nodeResourceSection renders what the scheduler has booked on a node against
// its capacity — the arithmetic swarm itself does when placing a task, and the
// usual reason a service sits unschedulable.
//
// Deliberately labelled "reserved", not "used": these are the tasks' declared
// RESERVATIONS. A task without one books nothing here while still being free to
// consume the whole node, so that count is called out rather than quietly
// ignored — otherwise "0% reserved" would read as "idle" on a busy node.
func nodeResourceSection(n swarmNodeInfo) string {
	var b strings.Builder
	b.WriteString("\n  [gray]reserved by tasks (scheduler's view)[-]\n")
	if n.NanoCPUs == 0 && n.MemoryBytes == 0 {
		b.WriteString("    [gray](node reports no capacity)[-]\n")
		return b.String()
	}
	line := func(label string, booked, capacity int64, fmtVal func(int64) string) {
		if capacity <= 0 {
			fmt.Fprintf(&b, "    %-7s [gray]unknown capacity[-]\n", label)
			return
		}
		free := capacity - booked
		if free < 0 {
			free = 0 // over-committed: report none free rather than a negative
		}
		pct := float64(booked) / float64(capacity) * 100
		fmt.Fprintf(&b, "    %-7s %s  %s / %s  [gray](%s free)[-]\n",
			label, resourceBar(booked, capacity), fmtVal(booked), fmtVal(capacity), fmtVal(free))
		if pct > 100 {
			b.WriteString("            [red]over-committed[-]\n")
		}
	}
	line("cpu", n.ReservedNanoCPUs, n.NanoCPUs, formatCPUCores)
	line("memory", n.ReservedMemoryBytes, n.MemoryBytes, formatMemBytes)
	if n.TasksWithoutReservation > 0 {
		fmt.Fprintf(&b, "    [gray]%d task(s) here declare no reservation — they are invisible\n"+
			"            to this figure and to the scheduler's placement maths.[-]\n", n.TasksWithoutReservation)
	}
	return b.String()
}

// resourceBarWidth is the number of cells a reservation bar occupies.
const resourceBarWidth = 20

// resourceBar draws a proportional bar, coloured by how full the node is:
// green under half, yellow past 75%, red once it is full or over-committed.
func resourceBar(booked, capacity int64) string {
	if capacity <= 0 {
		return strings.Repeat("─", resourceBarWidth)
	}
	ratio := float64(booked) / float64(capacity)
	filled := int(ratio*float64(resourceBarWidth) + 0.5)
	if filled > resourceBarWidth {
		filled = resourceBarWidth
	}
	if filled < 0 {
		filled = 0
	}
	color := "green"
	switch {
	case ratio >= 1:
		color = "red"
	case ratio >= 0.75:
		color = "yellow"
	}
	return fmt.Sprintf("[%s]%s[-][gray]%s[-]",
		color, strings.Repeat("█", filled), strings.Repeat("░", resourceBarWidth-filled))
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
	b.WriteString(nodeResourceSection(n))
	use, known := u.nodeUse[n.Hostname]
	b.WriteString(nodeUsageSection(n, use, known))
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

// nodeUsageSection renders what the node's containers are ACTUALLY using, as a
// counterpart to the reservation figures above it. The two answer different
// questions and are routinely far apart: a node can be fully booked and idle,
// or barely booked and on fire. Keeping them in separate blocks, each labelled
// with where it comes from, is what stops them being read as one number.
//
// Absent when no agent answered — an unreachable or too-old agent leaves the
// reservation view intact rather than showing zeros that look like "idle".
func nodeUsageSection(n swarmNodeInfo, use nodeUsage, known bool) string {
	if !known {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n  [gray]in use by containers (measured on the node)[-]\n")

	cpus := use.NodeCPUs
	if cpus <= 0 && n.NanoCPUs > 0 {
		cpus = n.NanoCPUs / 1e9
	}
	switch {
	case !use.CPUReady:
		// The agent has one reading; a percentage needs two. Saying so beats a
		// confident 0%.
		b.WriteString("    cpu     [gray]measuring…[-]\n")
	case cpus > 0:
		// use.CPUPercent is the share of ONE cpu summed over the containers, so
		// the node's own capacity is cpus*100 of those.
		usedCores := int64(use.CPUPercent / 100 * 1e9)
		capacity := cpus * 1e9
		fmt.Fprintf(&b, "    %-7s %s  %s / %s\n", "cpu", resourceBar(usedCores, capacity),
			formatCPUCores(usedCores), formatCPUCores(capacity))
	default:
		fmt.Fprintf(&b, "    %-7s %.2f cores\n", "cpu", use.CPUPercent/100)
	}

	total := use.NodeMem
	if total <= 0 {
		total = n.MemoryBytes
	}
	if total > 0 {
		fmt.Fprintf(&b, "    %-7s %s  %s / %s\n", "memory", resourceBar(use.MemBytes, total),
			formatMemBytes(use.MemBytes), formatMemBytes(total))
	} else {
		fmt.Fprintf(&b, "    %-7s %s\n", "memory", formatMemBytes(use.MemBytes))
	}

	// Container usage is not node usage: the kernel, the daemon and anything
	// running outside docker are not in these numbers. Say so rather than let
	// the bar be read as the node's load average.
	b.WriteString("    [gray]containers only — processes outside docker are not counted.[-]\n")
	return b.String()
}
