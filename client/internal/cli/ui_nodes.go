// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/client/internal/fanout"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/pb"
)

// nodesTab is the Nodes tab: the swarm's nodes, with what each one holds
// (tasks, volumes, image footprint) and the node actions — labels,
// availability, prune and the cluster-wide prune schedule.
type nodesTab struct {
	*listTab[swarmops.NodeInfo]
	// ext is what the node list itself does not carry: the per-node figures
	// that come from the agents, one fan-out each, per cluster.
	ext perCluster[nodesExtra]
}

// nodesExtra is the agent-sourced part of one cluster's Nodes tab.
type nodesExtra struct {
	volCounts  map[string]int
	volsLoaded bool // VOLS shows "…" until the counts arrive
	imgs       map[string]nodeImages
}

// volumes is a node's volume count as the Nodes tab shows it: "…" while the
// counts load, "–" when the node's agent did not answer. That used to be "0",
// which read as "this node has no volumes" when the truth was "no agent could
// tell".
func (ex *nodesExtra) volumes(host string) string {
	if !ex.volsLoaded {
		return "…"
	}
	n, ok := ex.volCounts[host]
	if !ok {
		return "–"
	}
	return strconv.Itoa(n)
}

func newNodesTab(u *ui) *nodesTab {
	// The cursor's key is the hostname; the leader's cell carries a ★ the
	// name func does not see.
	t := &nodesTab{listTab: newListTab(u, noHeaders, func(n swarmops.NodeInfo) string { return n.Hostname })}
	t.ext.init = func() *nodesExtra { return &nodesExtra{} }
	t.row = t.renderRow
	// No advice to give: a swarm you are talking to has nodes, so an empty
	// list is a statement about the cluster, not something to act on here.
	t.empty = func() string { return emptyText("nodes", "the manager reported none") }
	t.table.SetSelectedFunc(func(int, int) {
		if n, ok := t.selected(); ok {
			t.showDetail(n)
		}
	})
	t.table.SetInputCapture(t.keys)
	return t
}

// nodeTab is the session's Nodes tab.
func (u *ui) nodeTab() *nodesTab { return u.tab("nodes").(*nodesTab) }

// extra is the visible cluster's agent-sourced node figures.
func (t *nodesTab) extra() *nodesExtra { return t.ext.of(t.u.clusterState) }

func (t *nodesTab) enter() { t.reload() }

func (t *nodesTab) refresh() {
	t.reload()
	t.u.loadUsage() // the node detail shows measured usage next to reservations
}

func (t *nodesTab) footer() string {
	km, kl := t.u.km, keyLabel
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] edit labels  [yellow]%s[white] availability  %s prune  [yellow]%s[white] prune schedule (all nodes)",
		kl(km.NodeLabels), kl(km.NodeAvail), keyTag(dangerKey(kl(km.NodePrune))), kl(km.NodePruneSched))
}

func (t *nodesTab) helpSection(h *helpWriter) {
	km, kl := t.u.km, keyLabel
	h.sec("Nodes")
	h.line("★", "the swarm leader (managers in aqua)")
	h.line("CPU% MEM%", "containers' use of the node, from its agent (– no agent)")
	h.line("Enter / i", "details")
	h.line(kl(km.NodeLabels), "edit labels")
	h.line(kl(km.NodeAvail), "availability: active / pause / drain")
	h.danger(kl(km.NodePrune), "prune the node: containers, images, build cache, networks, volumes (choose + confirm)")
	h.line(kl(km.NodePruneSched), "prune schedule for ALL nodes (restarts the agents on save)")
	h.line("M", "in the details: measure reclaimable space now")
}

// yank copies the node list. Before the registry the Nodes tab fell into
// yankCurrent's default branch and copied the VOLUME list.
func (t *nodesTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "NODE\tROLE\tAVAIL\tSTATE\tCPU%\tMEM%\tENGINE\tTASKS\tLABELS")
	for _, n := range t.rows() {
		name := n.Hostname
		if n.Leader {
			name += " (leader)"
		}
		use, known := t.u.nodeUse[n.Hostname]
		fmt.Fprintf(b, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n", name, orDash(n.Role), orDash(n.Availability),
			orDash(n.State), nodeCPUPercent(n, use, known), nodeMemPercent(n, use, known), orDash(n.EngineVersion), n.Tasks, len(n.Labels))
	}
}

func (t *nodesTab) renderRow(r int, n swarmops.NodeInfo) {
	ex := t.extra()
	name, nameColor := n.Hostname, tcell.ColorWhite
	if n.Role == "manager" {
		nameColor = tcell.ColorAqua
	}
	if n.Leader {
		name = "★ " + name
	}
	volStr := ex.volumes(n.Hostname)
	use, known := t.u.nodeUse[n.Hostname]
	cpu, mem := nodeCPUPercent(n, use, known), nodeMemPercent(n, use, known)
	t.table.SetCell(r, 0, tview.NewTableCell(name).SetTextColor(nameColor).SetExpansion(2))
	t.table.SetCell(r, 1, tview.NewTableCell(orDash(n.Role)).SetExpansion(1))
	t.table.SetCell(r, 2, tview.NewTableCell(orDash(n.Availability)).SetTextColor(nodeAvailColor(n.Availability)).SetExpansion(1))
	t.table.SetCell(r, 3, tview.NewTableCell(orDash(n.State)).SetTextColor(nodeStateColor(n.State)).SetExpansion(1))
	t.table.SetCell(r, 4, tview.NewTableCell(cpu).SetTextColor(usagePercentColor(cpu)).SetExpansion(1))
	t.table.SetCell(r, 5, tview.NewTableCell(mem).SetTextColor(usagePercentColor(mem)).SetExpansion(1))
	t.table.SetCell(r, 6, tview.NewTableCell(fmt.Sprintf("%d", n.Tasks)).SetExpansion(1))
	t.table.SetCell(r, 7, tview.NewTableCell(volStr).SetExpansion(1))
	t.table.SetCell(r, 8, tview.NewTableCell(orDash(n.EngineVersion)).SetExpansion(1))
	t.table.SetCell(r, 9, tview.NewTableCell(fmt.Sprintf("%d", len(n.Labels))).SetExpansion(1))
}

// nodeCPUPercent is the Nodes tab's CPU% cell: what the node's containers
// use, as a share of the node's cores. "–" when no agent answered (not "0%",
// which would read as idle), "…" while the agent has only one sample.
func nodeCPUPercent(n swarmops.NodeInfo, use nodeUsage, known bool) string {
	if !known {
		return "–"
	}
	if !use.CPUReady {
		return "…"
	}
	cpus := use.NodeCPUs
	if cpus <= 0 && n.NanoCPUs > 0 {
		cpus = n.NanoCPUs / 1e9
	}
	if cpus <= 0 {
		return "–"
	}
	// CPUPercent is the share of ONE cpu summed over the containers.
	return fmt.Sprintf("%.0f%%", use.CPUPercent/float64(cpus))
}

// nodeMemPercent is the MEM% cell: the containers' memory as a share of the
// node's, "–" when no agent answered or the node's size is unknown.
func nodeMemPercent(n swarmops.NodeInfo, use nodeUsage, known bool) string {
	if !known {
		return "–"
	}
	total := use.NodeMem
	if total <= 0 {
		total = n.MemoryBytes
	}
	if total <= 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", float64(use.MemBytes)*100/float64(total))
}

// usagePercentColor greys a missing reading and warns from 80 % up.
func usagePercentColor(cell string) tcell.Color {
	p, err := strconv.Atoi(strings.TrimSuffix(cell, "%"))
	switch {
	case err != nil:
		return tcell.ColorGray
	case p >= 90:
		return tcell.ColorRed
	case p >= 80:
		return tcell.ColorYellow
	}
	return tcell.ColorWhite
}

// reload fetches the node list from the manager, and — on their own, since
// each is an agent fan-out — the image footprints and volume counts.
func (t *nodesTab) reload() {
	u := t.u
	r, ac, dcli, f := u.r, u.agents, u.dcli, u.f
	ex := t.extra()
	// VOLS goes back to "…" for the new load. Reset here, at the start, not
	// when the list arrives: the counts may well arrive first.
	ex.volsLoaded = false
	t.load("ui.loadNodes", func(ctx context.Context) ([]swarmops.NodeInfo, error) { return swarmops.ListNodes(ctx, dcli) })
	// Image footprints are node-local too and only shown in the detail, so they
	// load on their own and simply stay absent for an agent that cannot answer.
	async(u, "ui.loadNodes.images", func(ctx context.Context) (map[string]nodeImages, error) {
		imgs := map[string]nodeImages{}
		if ns, nerr := r.Nodes(ctx); nerr == nil {
			per := make([]nodeImages, len(ns))
			ok := make([]bool, len(ns))
			_ = fanout.Nodes(ctx, ns, func(ctx context.Context, i int, n resolve.Node) {
				ni, ierr := ac.Images(ctx, n, f.connectTimeout)
				per[i], ok[i] = ni, ierr == nil
			})
			for i, n := range ns {
				if ok[i] {
					imgs[n.Name] = per[i]
				}
			}
		}
		return imgs, nil
	}, func(imgs map[string]nodeImages, _ error) { ex.imgs = imgs })
	// Per-node volume counts are node-local (agent fan-out), so fill them in
	// asynchronously — the VOLS column shows "…" until they arrive.
	async(u, "ui.loadNodes.volumes", func(ctx context.Context) (map[string]int, error) {
		counts := map[string]int{}
		if ns, nerr := r.Nodes(ctx); nerr == nil {
			vs, errs := ac.Volumes(ctx, ns, f.connectTimeout)
			counts = swarmops.VolumeCountsByNode(vs)
			// A node whose agent answered with no volumes has 0; one whose
			// agent did not answer has no entry at all, and shows "–".
			for _, n := range ns {
				_, failed := errs[n.Name]
				if _, counted := counts[n.Name]; !failed && !counted {
					counts[n.Name] = 0
				}
			}
		}
		return counts, nil
	}, func(counts map[string]int, _ error) {
		// Mark loaded even on failure so VOLS shows a result, not "…" forever.
		ex.volCounts, ex.volsLoaded = counts, true
		if t.shownFor == u.clusterState {
			t.render() // only over rows; a loading or error state stays
		}
	})
}

// editNodeLabels is a compact staged editor (add/edit/delete/apply) over a
// node's labels, applied via NodeUpdate. Dedicated (not the service editList)
// because a node update is immediate — there is no rolling update.
func (u *ui) editNodeLabels(n swarmops.NodeInfo, back tview.Primitive, after func()) {
	e := &nodeLabelEditor{u: u, n: n, cur: swarmops.KVPairs(n.Labels), after: after,
		list: tview.NewList().ShowSecondaryText(false)}
	e.list.SetBorder(true).SetTitle(fmt.Sprintf(" labels of node %s ", n.Hostname))
	e.render()
	e.ov = u.overlayFor(pageNodeLabels, back, footerKeys("a", "add", "e", "edit", "d", "delete", "Ctrl-S", "apply", "Esc", "cancel"))
	e.list.SetInputCapture(e.keys)
	e.ov.show(centered(e.list, 70, 16), e.list)
}

// nodeLabelEditor is one open node label editor (review Q-01 split
// editNodeLabels): the staged labels, the list showing them, and its keys.
type nodeLabelEditor struct {
	u     *ui
	n     swarmops.NodeInfo
	cur   []string // the staged key=value labels, applied together by w
	list  *tview.List
	ov    *overlay
	after func()
}

// render rebuilds the list from the staged labels, keeping the cursor.
func (e *nodeLabelEditor) render() {
	list := e.list
	i := list.GetCurrentItem()
	list.Clear()
	if len(e.cur) == 0 {
		list.AddItem("[gray](no labels)[-]", "", 0, nil)
	}
	for _, it := range e.cur {
		list.AddItem(tview.Escape(it), "", 0, nil)
	}
	if i < list.GetItemCount() {
		list.SetCurrentItem(i)
	}
}

// keys is the list's input capture.
func (e *nodeLabelEditor) keys(ev *tcell.EventKey) *tcell.EventKey {
	if ev.Key() == tcell.KeyEscape {
		e.ov.Close()
		return nil
	}
	// Ctrl-S applies, as in every editor; w is its one-key alias.
	if ev.Key() == tcell.KeyCtrlS {
		e.apply()
		return nil
	}
	if ev.Key() != tcell.KeyRune {
		return vimListKeys(ev)
	}
	switch ev.Rune() {
	case 'a':
		e.prompt("", func(v string) { e.cur = append(e.cur, v) })
	case 'e':
		if i, ok := e.current(); ok {
			e.prompt(e.cur[i], func(v string) { e.cur[i] = v })
		}
	case 'd':
		if i, ok := e.current(); ok {
			e.cur = append(e.cur[:i], e.cur[i+1:]...)
			e.render()
		}
	case 'w':
		e.apply()
	default:
		return vimListKeys(ev)
	}
	return nil
}

// current is the row under the cursor; false on the "(no labels)" row.
func (e *nodeLabelEditor) current() (int, bool) {
	i := e.list.GetCurrentItem()
	return i, i >= 0 && i < len(e.cur)
}

// prompt asks for one key=value label, prefilled with initial, and hands a
// well-formed answer to done.
func (e *nodeLabelEditor) prompt(initial string, done func(string)) {
	u := e.u
	in := tview.NewInputField().SetLabel("key=value: ").SetText(initial).SetFieldWidth(44)
	// Nested inside the label editor: it goes back to the list, not to the
	// table the editor itself returns to.
	pov := u.overlayFor(pageNodeLabelPrompt, e.list, "")
	in.SetDoneFunc(func(k tcell.Key) {
		pov.Close()
		if k != tcell.KeyEnter {
			return
		}
		txt := strings.TrimSpace(in.GetText())
		if txt == "" {
			return
		}
		if _, _, err := swarmops.ParseLabel(txt); err != nil {
			u.info(err.Error())
			return
		}
		done(txt)
		e.render()
	})
	in.SetBorder(true)
	pov.show(centeredPrompt(in, 60), in)
}

// apply writes the staged labels to the node, behind a confirm.
func (e *nodeLabelEditor) apply() {
	u, n, list := e.u, e.n, e.list
	app, dcli := u.app, u.dcli
	lbls, err := swarmops.LabelsFromStrings(e.cur)
	if err != nil {
		u.info(err.Error())
		return
	}
	u.confirm(fmt.Sprintf("Update labels on node %s?\n\nApplies immediately (no rolling update).", n.Hostname), "Apply", list, func() {
		mutateDo(u, "ui.nodes.labels", "node label update", func(ctx context.Context) error {
			return swarmops.SetNodeLabels(ctx, dcli, n.ID, n.Version, lbls)
		}, func(err error) {
			if err != nil {
				u.info("update failed: " + err.Error())
				app.SetFocus(list)
				return
			}
			e.ov.Close()
			u.info(fmt.Sprintf("node %s labels updated", n.Hostname))
			if e.after != nil {
				e.after()
			}
		})
	})
}

// openNodeAvailability offers the three availability states. One menu rather
// than three keys: the states are mutually exclusive, the menu can show which
// one is current, and it leaves the nodes tab's key space free.
//
// Draining is confirmed because it evicts the node's tasks; active and pause are
// applied directly (pause only stops NEW placements, it does not move anything).
func (u *ui) openNodeAvailability(n swarmops.NodeInfo, back tview.Primitive, after func()) {
	dcli := u.dcli
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" availability — %s ", n.Hostname))
	ov := u.overlayFor(pageNodeAvail, back, footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
	closeMenu := ov.Close

	apply := func(to swarm.NodeAvailability) {
		mutateDo(u, "ui.nodes.availability", "node availability change", func(ctx context.Context) error {
			return swarmops.SetNodeAvailability(ctx, dcli, n.ID, n.Version, to)
		}, func(err error) {
			if err != nil {
				u.info(fmt.Sprintf("could not set %s to %s: %v", n.Hostname, to, err))
				return
			}
			u.flash(fmt.Sprintf(" [green]✓ %s is now %s[white]", n.Hostname, to))
			if after != nil {
				after()
			}
		})
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
	add("active", "schedule tasks here normally", swarm.NodeAvailabilityActive, "")
	add("pause", "keep running tasks, place no new ones", swarm.NodeAvailabilityPause, "")
	add("[red]drain[-]", "move every task off this node", swarm.NodeAvailabilityDrain,
		fmt.Sprintf("Drain %q?\n\nSwarm will stop this node's %d running task(s) and reschedule them on other nodes. Services whose tasks cannot be placed elsewhere will go unschedulable.",
			n.Hostname, n.Tasks))
	list.AddItem("cancel", "", 0, closeMenu)
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			closeMenu()
			return nil
		}
		return vimListKeys(ev)
	})
	ov.show(centered(list, 62, 13), list)
}

// nodeResourceSection renders what the scheduler has booked on a node against
// its capacity — the arithmetic swarm itself does when placing a task, and the
// usual reason a service sits unschedulable.
//
// Deliberately labelled "reserved", not "used": these are the tasks' declared
// RESERVATIONS. A task without one books nothing here while still being free to
// consume the whole node, so that count is called out rather than quietly
// ignored — otherwise "0% reserved" would read as "idle" on a busy node.
func nodeResourceSection(n swarmops.NodeInfo) string {
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
	line("cpu", n.ReservedNanoCPUs, n.NanoCPUs, swarmops.FormatCPUCores)
	line("memory", n.ReservedMemoryBytes, n.MemoryBytes, swarmops.FormatMemBytes)
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

func (t *nodesTab) showDetail(n swarmops.NodeInfo) {
	u := t.u
	d := &nodeDetail{t: t, n: n, back: t.table, rec: &nodeReclaimView{loading: true},
		tv: tview.NewTextView().SetDynamicColors(true).SetScrollable(true)}
	d.tv.SetBorder(true).SetTitle(fmt.Sprintf(" node %s ", n.Hostname))
	d.head = t.detailHead(n)
	d.render()
	u.loadNodeReclaim(n.Hostname, false, d.apply)

	d.ov = u.overlayFor(pageNodeDetail, d.back, footerKeys("M", "measure now", dangerKey(keyLabel(u.km.NodePrune)), "prune…",
		keyLabel(u.km.NodePruneSched), "schedule (all nodes)", "l", "edit labels", "j/k", "scroll", "Esc", "back"))
	d.tv.SetInputCapture(d.keys)
	d.ov.show(centered(d.tv, 80, 40), d.tv)
}

// nodeDetail is one open node detail overlay (review Q-01 split showDetail):
// the head, the reclaim block under it, and the keys that measure or leave
// for the node's dialogs.
type nodeDetail struct {
	t    *nodesTab
	n    swarmops.NodeInfo
	back tview.Primitive // the nodes table, which every dialog opened from here returns to
	tv   *tview.TextView
	head string // everything above the reclaim block
	ov   *overlay

	// The reclaim block arrives from the agent after the detail is open. It is
	// re-rendered in place; closed (UI goroutine only) stops a late answer
	// from writing into a detail the operator already left.
	rec    *nodeReclaimView
	closed bool
}

// render repaints the detail, keeping the scroll position.
func (d *nodeDetail) render() {
	row, col := d.tv.GetScrollOffset()
	d.tv.SetText(d.head + d.rec.text(time.Now()))
	d.tv.ScrollTo(row, col)
}

// apply takes a reclaim answer (on the UI goroutine).
func (d *nodeDetail) apply(resp *pb.ReclaimStatsResponse, err error) {
	if d.closed {
		return
	}
	d.rec.loading, d.rec.err = false, err
	if resp != nil {
		d.rec.resp = resp
	}
	d.render()
}

func (d *nodeDetail) close() {
	d.closed = true
	d.ov.Close()
}

// measure asks the agent to measure now, unless an answer is still pending.
// Rule: never QueueUpdateDraw from here (the UI goroutine) — the "measuring"
// state is written straight into the widget.
func (d *nodeDetail) measure() {
	if d.rec.loading {
		return
	}
	d.rec.loading = true
	d.render()
	d.t.u.loadNodeReclaim(d.n.Hostname, true, d.apply)
}

// keys is the detail's input capture. The dialog keys close the detail first:
// the dialog takes its place over the nodes table.
func (d *nodeDetail) keys(ev *tcell.EventKey) *tcell.EventKey {
	u, t := d.t.u, d.t
	if ev.Key() == tcell.KeyEscape {
		d.close()
		return nil
	}
	if ev.Key() != tcell.KeyRune {
		return ev
	}
	switch ev.Rune() {
	case u.km.Quit, 'i':
		d.close()
	case 'M':
		// Capital M: m toggles the mouse on every tab, and an overlay does not
		// give a global key a second meaning (review T-10).
		d.measure()
	case u.km.NodePrune:
		d.close()
		u.openNodePrune(d.n, d.back, t.reload)
	case u.km.NodePruneSched:
		d.close()
		u.openPruneSchedule(d.back)
	case 'l':
		d.close()
		u.editNodeLabels(d.n, d.back, t.reload)
	case 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	default:
		return ev
	}
	return nil
}

// detailHead is everything in the node detail above the reclaim block.
func (t *nodesTab) detailHead(n swarmops.NodeInfo) string {
	u, ex := t.u, t.extra()
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
	kv("cpus", swarmops.FormatCPUCores(n.NanoCPUs))
	kv("memory", swarmops.FormatMemBytes(n.MemoryBytes))
	kv("tasks", fmt.Sprintf("%d running", n.Tasks))
	b.WriteString(nodeResourceSection(n))
	use, known := u.nodeUse[n.Hostname]
	b.WriteString(nodeUsageSection(n, use, known))
	imgs, imgsKnown := ex.imgs[n.Hostname]
	b.WriteString(imageSection(imgs, imgsKnown))
	kv("volumes", ex.volumes(n.Hostname))
	b.WriteString("\n  [gray]labels[-]\n")
	if lbls := swarmops.KVPairs(n.Labels); len(lbls) > 0 {
		for _, l := range lbls {
			fmt.Fprintf(&b, "    %s\n", tview.Escape(l))
		}
	} else {
		b.WriteString("    [gray](none)[-]\n")
	}
	return b.String()
}

// nodeUsageSection renders what the node's containers are ACTUALLY using, as a
// counterpart to the reservation figures above it. The two answer different
// questions and are routinely far apart: a node can be fully booked and idle,
// or barely booked and on fire. Keeping them in separate blocks, each labelled
// with where it comes from, is what stops them being read as one number.
//
// Absent when no agent answered — an unreachable or too-old agent leaves the
// reservation view intact rather than showing zeros that look like "idle".
func nodeUsageSection(n swarmops.NodeInfo, use nodeUsage, known bool) string {
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
			swarmops.FormatCPUCores(usedCores), swarmops.FormatCPUCores(capacity))
	default:
		fmt.Fprintf(&b, "    %-7s %.2f cores\n", "cpu", use.CPUPercent/100)
	}

	total := use.NodeMem
	if total <= 0 {
		total = n.MemoryBytes
	}
	if total > 0 {
		fmt.Fprintf(&b, "    %-7s %s  %s / %s\n", "memory", resourceBar(use.MemBytes, total),
			swarmops.FormatMemBytes(use.MemBytes), swarmops.FormatMemBytes(total))
	} else {
		fmt.Fprintf(&b, "    %-7s %s\n", "memory", swarmops.FormatMemBytes(use.MemBytes))
	}

	// Container usage is not node usage: the kernel, the daemon and anything
	// running outside docker are not in these numbers. Say so rather than let
	// the bar be read as the node's load average.
	b.WriteString("    [gray]containers only — processes outside docker are not counted.[-]\n")
	return b.String()
}

// openNodeImagePrune offers the two reclaim modes as two separate entries
// rather than one action with a checkbox.
//
// They are different acts: one removes untagged leftovers that nothing can
// start from, the other removes images a service scaled to zero still needs.
// A checkbox invites getting the destructive one by accident; a menu makes the
// operator name which one they mean, and each still goes through its own
// confirm spelling out the consequence.
func (u *ui) openNodeImagePrune(n swarmops.NodeInfo, back tview.Primitive, after func()) {
	r, ac, f := u.r, u.agents, u.f
	ni, known := u.nodeTab().extra().imgs[n.Hostname]
	if !known {
		u.info(fmt.Sprintf("No image information for %q.\n\n"+
			"The node's agent did not answer — it may be unreachable, or older\n"+
			"than this release and unaware of the image RPC.", n.Hostname))
		return
	}

	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" reclaim image space — %s ", n.Hostname))
	ov := u.overlayFor(pageNodeImages, back, footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
	closeIt := ov.Close

	run := func(all bool) {
		closeIt()
		u.confirm(pruneConfirmText(n.Hostname, ni, all), "Remove", back, func() {
			type pruned struct {
				reclaimed int64
				deleted   int
			}
			mutate(u, "ui.nodes.imageprune", "image prune", func(ctx context.Context) (pruned, error) {
				// Resolve the dial address rather than deriving one from the node
				// row: which address reaches an agent depends on the configured
				// address mode, and only the resolver knows it.
				target, terr := nodeByHostname(ctx, r, n.Hostname)
				if terr != nil {
					return pruned{}, terr
				}
				reclaimed, deleted, err := ac.PruneImages(ctx, target, all, f.connectTimeout)
				return pruned{reclaimed, deleted}, err
			}, func(p pruned, err error) {
				if err != nil {
					u.info("prune images: " + err.Error())
					return
				}
				u.flash(fmt.Sprintf(" [green]✓ %s freed on %s (%d image(s))[white]",
					swarmops.FormatMemBytes(p.reclaimed), n.Hostname, p.deleted))
				if after != nil {
					after()
				}
			})
		})
	}

	list.AddItem(fmt.Sprintf("untagged leftovers — frees %s", swarmops.FormatMemBytes(ni.Dangling)),
		"safe: nothing can start from an untagged image", 0, func() { run(false) })
	list.AddItem(fmt.Sprintf("[red]every unused image[white] — frees %s", swarmops.FormatMemBytes(ni.Dangling+ni.Unused)),
		"also removes images a stopped service still needs", 0, func() { run(true) })
	list.AddItem("cancel", "", 0, closeIt)

	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			closeIt()
			return nil
		}
		return vimListKeys(ev)
	})
	ov.show(centered(list, 66, 9), list)
}

// keys is the key handler of the Nodes table.
func (t *nodesTab) keys(ev *tcell.EventKey) *tcell.EventKey {
	u, km := t.u, t.u.km
	if ev.Key() != tcell.KeyRune {
		return u.tabKeys(ev)
	}
	switch ev.Rune() {
	case km.NodeAvail:
		if n, ok := t.selected(); ok {
			u.openNodeAvailability(n, t.table, t.reload)
		}
	case km.NodePrune:
		if n, ok := t.selected(); ok {
			u.openNodePrune(n, t.table, t.reload)
		}
	case km.NodePruneSched:
		u.openPruneSchedule(t.table)
	case km.NodeLabels:
		if n, ok := t.selected(); ok {
			u.editNodeLabels(n, t.table, t.reload)
		}
	case 'i':
		if n, ok := t.selected(); ok {
			t.showDetail(n)
		}
	default:
		return u.tabKeys(ev)
	}
	return nil
}
