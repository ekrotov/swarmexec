// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmops"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/mount"
	"github.com/rivo/tview"
)

// volumesTab is the Volumes tab: the volume list across every node, its
// selection, sort, "/" filter and the volume actions.
//
// It is the first tab with a type of its own (review A-02), and the shape the
// others follow: the widget and the session preferences live on the tab, what
// one cluster shows lives in a volumesState per cluster (see perCluster).
type volumesTab struct {
	u     *ui
	table *tview.Table

	// The sort order is the operator's preference, not a property of a
	// cluster: it survives a switch.
	sortField int
	sortDesc  bool

	per perCluster[volumesState]
	// shownFor is the cluster whose rows the table holds, nil while it shows a
	// loading or error state. A render for another cluster first saves this
	// one's cursor — that is how a switch back lands on the same volume.
	shownFor *clusterState
}

// volumesState is what the Volumes tab knows about one cluster.
type volumesState struct {
	vols  []swarmVolume
	usage map[string][]volumeConsumer
	sizes map[string]int64
	errs  map[string]error
	// sizesLoading is true while the (slow, du-style) size pass runs; the SIZE
	// header carries a spinner meanwhile.
	sizesLoading bool

	// shown is the filtered + sorted subset currently displayed; it is what
	// selected() and "select all" index.
	shown []swarmVolume
	// selected holds the volumes marked with space for a bulk delete, keyed
	// by name so the selection survives sorting and re-render.
	selected map[string]bool
	filter   string // the "/" query
	// cursor is the volume the cursor stood on when the table last showed
	// this cluster, by name.
	cursor string
}

func newVolumesTab(u *ui) *volumesTab {
	t := &volumesTab{u: u}
	t.per.init = func() *volumesState { return &volumesState{selected: map[string]bool{}} }
	t.table = tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	t.table.SetSelectedStyle(u.selStyle)
	t.table.SetSelectedFunc(func(int, int) {
		if v, ok := t.selected(); ok && len(v.Nodes) > 0 {
			t.showNodes(v)
		}
	})
	t.table.SetInputCapture(t.keys)
	return t
}

// volTab is the session's Volumes tab.
func (u *ui) volTab() *volumesTab { return u.tab("volumes").(*volumesTab) }

// st is the visible cluster's volume state.
func (t *volumesTab) st() *volumesState { return t.per.of(t.u.clusterState) }

func (t *volumesTab) widget() tview.Primitive { return t.table }
func (t *volumesTab) enter()                  { t.load() }
func (t *volumesTab) refresh()                { t.load() }

func (t *volumesTab) footer() string {
	km, kl := t.u.km, keyLabel
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]%s[white] search  [yellow]%s[white] new  [yellow]%s[white] select  [yellow]%s[white] all  [yellow]%s[white] attach  %s delete  [yellow]%s[white] prune  [yellow]Enter[white] nodes  [yellow]%s[white] used by  [yellow]%s[white] sort",
		kl(km.Search), kl(km.VolNew), kl(km.VolSelect), kl(km.VolSelectAll), kl(km.VolAttach), keyTag(dangerKey(kl(km.VolDelete))), kl(km.VolPrune), kl(km.VolUsedBy), kl(km.VolSort))
}

func (t *volumesTab) helpSection(h *helpWriter) {
	km, kl := t.u.km, keyLabel
	h.sec("Volumes")
	h.line(kl(km.VolNew), "new volume")
	h.line(kl(km.VolSelect)+"/"+kl(km.VolSelectAll), "select / select all")
	h.line(kl(km.VolAttach), "attach to a service")
	h.danger(kl(km.VolDelete), "delete")
	h.line(kl(km.VolPrune), "prune unused")
	h.line("Enter", "which nodes hold it")
	h.line(kl(km.VolUsedBy), "used-by (containers / services)")
	h.line(kl(km.VolSort)+"/"+kl(km.VolSortRev), "sort / reverse")
	h.line(kl(km.Search), "search")
}

// yank copies the displayed (filtered + sorted) volumes.
func (t *volumesTab) yank(b *strings.Builder) {
	st := t.st()
	fmt.Fprintln(b, "NAME\tDRIVER\tNODES\tUSED BY\tAGE\tSIZE")
	for _, v := range st.shown {
		used := "-"
		if n := len(st.usage[v.Name]); n > 0 {
			used = fmt.Sprintf("%d", n)
		}
		size := int64(-1)
		if s, ok := st.sizes[v.Name]; ok {
			size = s
		}
		fmt.Fprintf(b, "%s\t%s\t%d: %s\t%s\t%s\t%s\n", v.Name, orDash(v.Driver), len(v.Nodes), joinNodes(v.Nodes), used, volumeAge(v.Created), humanBytes(size))
	}
}

func (t *volumesTab) statusNote() string {
	if n := len(t.st().selected); n > 0 {
		return fmt.Sprintf("[yellow]▣ %d selected[white]", n)
	}
	return ""
}

func (t *volumesTab) searchText() string        { return t.st().filter }
func (t *volumesTab) searchPlaceholder() string { return "filter volumes / driver / node" }
func (t *volumesTab) setSearch(q string) {
	t.st().filter = q
	t.render()
}

func (st *volumesState) matches(v swarmVolume) bool {
	q := strings.ToLower(strings.TrimSpace(st.filter))
	if q == "" {
		return true
	}
	if strings.Contains(strings.ToLower(v.Name), q) || strings.Contains(strings.ToLower(v.Driver), q) {
		return true
	}
	for _, n := range v.Nodes {
		if strings.Contains(strings.ToLower(n.Name), q) {
			return true
		}
	}
	return false
}

// sortRows orders rows by the active field; for size/age an unknown value
// always sorts last (regardless of direction), with name as the tiebreaker.
func (t *volumesTab) sortRows(st *volumesState, rows []swarmVolume) {
	known := func(v swarmVolume) bool {
		switch t.sortField {
		case volSortSize:
			_, ok := st.sizes[v.Name]
			return ok
		case volSortAge:
			return !v.Created.IsZero()
		default:
			return true
		}
	}
	less := func(a, b swarmVolume) bool {
		switch t.sortField {
		case volSortNodes:
			if len(a.Nodes) != len(b.Nodes) {
				return len(a.Nodes) < len(b.Nodes)
			}
		case volSortUsed:
			if ua, ub := len(st.usage[a.Name]), len(st.usage[b.Name]); ua != ub {
				return ua < ub
			}
		case volSortAge:
			if !a.Created.Equal(b.Created) {
				return a.Created.Before(b.Created) // earlier = older = "more age"
			}
		case volSortSize:
			if sa, sb := st.sizes[a.Name], st.sizes[b.Name]; sa != sb {
				return sa < sb
			}
		}
		return a.Name < b.Name
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if ka, kb := known(a), known(b); ka != kb {
			return ka // known before unknown, both directions
		}
		if t.sortDesc {
			return less(b, a)
		}
		return less(a, b)
	})
}

// remember saves the cursor of the cluster the table currently shows, before
// the table is cleared — by a load's placeholder or by a render for another
// cluster.
func (t *volumesTab) remember() {
	if t.shownFor == nil {
		return
	}
	prev := t.per.of(t.shownFor)
	if v, ok := selectedRow(t.table, prev.shown); ok {
		prev.cursor = v.Name
	}
}

// placeholder replaces the rows with a loading or error state.
func (t *volumesTab) placeholder(cell *tview.TableCell) {
	t.remember()
	t.shownFor = nil
	tableState(t.table, vHeaders, cell)
}

func (t *volumesTab) render() {
	// Keep the cursor on the same volume across re-render (sort/size refresh)
	// and across a cluster switch: each cluster puts back its own.
	t.remember()
	st := t.st()
	t.shownFor = t.u.clusterState
	t.table.Clear()
	t.renderHeader(st)
	// Build the displayed subset from the "/" filter, then sort it. The
	// displayed order must match the slice selected() indexes, so both use
	// shown — otherwise a filter/sort makes delete act on the wrong row.
	st.shown = st.shown[:0]
	for _, v := range st.vols {
		if st.matches(v) {
			st.shown = append(st.shown, v)
		}
	}
	t.sortRows(st, st.shown)
	selRow := 1
	for i, v := range st.shown {
		t.renderRow(st, i+1, v)
		if v.Name == st.cursor {
			selRow = i + 1
		}
	}
	if len(st.shown) > 0 {
		t.table.Select(selRow, 0)
	}
	// Row 1 when the list is empty, so the note does not land on top of the
	// empty state below it.
	noteRow := len(st.shown) + 1
	if len(st.shown) == 0 {
		text := emptyText("volumes", keyHint(t.u.km.VolNew, "to create one"))
		if st.filter != "" {
			text = fmt.Sprintf("(no matches for %q)", st.filter)
		}
		t.table.SetCell(1, 0, emptyCell(text))
		noteRow = 2
	}
	if len(st.errs) > 0 {
		t.table.SetCell(noteRow, 0, stateCell(fmt.Sprintf("(%d node(s) unreachable)", len(st.errs)), tcell.ColorYellow))
	}
}

func (t *volumesTab) renderHeader(st *volumesState) {
	for c, h := range vHeaders {
		if volSortCol[t.sortField] == c {
			if t.sortDesc {
				h += " ▼"
			} else {
				h += " ▲"
			}
		}
		// While the (slow) size scan runs, show a loading marker on the SIZE
		// header; the spinner goroutine animates this cell.
		if c == volSortCol[volSortSize] && st.sizesLoading {
			h += " " + loadingText
		}
		t.table.SetCell(0, c, headerCell(h))
	}
}

func (t *volumesTab) renderRow(st *volumesState, row int, v swarmVolume) {
	usedCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
	if used := len(st.usage[v.Name]); used > 0 {
		usedCell = tview.NewTableCell(fmt.Sprintf("%d", used)).SetTextColor(tcell.ColorGreen).SetExpansion(1)
	}
	sizeCell := tview.NewTableCell("…").SetTextColor(tcell.ColorGray).SetExpansion(1)
	if st.sizes != nil {
		size, color := int64(-1), tcell.ColorGray
		if s, ok := st.sizes[v.Name]; ok {
			size, color = s, tcell.ColorWhite
		}
		sizeCell = tview.NewTableCell(humanBytes(size)).SetTextColor(color).SetExpansion(1)
	}
	mark, nameColor := "  ", tcell.ColorWhite
	if st.selected[v.Name] {
		mark, nameColor = "▣ ", tcell.ColorAqua
	}
	nodes := fmt.Sprintf("%d: %s", len(v.Nodes), joinNodes(v.Nodes))
	t.table.SetCell(row, 0, tview.NewTableCell(mark+shortVolume(v.Name)).SetTextColor(nameColor).SetExpansion(1).SetMaxWidth(volNameMaxWidth))
	t.table.SetCell(row, 1, sizeCell)
	t.table.SetCell(row, 2, tview.NewTableCell(nodes).SetExpansion(1).SetMaxWidth(volNodesMaxWidth))
	t.table.SetCell(row, 3, usedCell)
	t.table.SetCell(row, 4, tview.NewTableCell(orDash(v.Driver)).SetExpansion(1))
	t.table.SetCell(row, 5, tview.NewTableCell(volumeAge(v.Created)).SetExpansion(1))
}

// load fetches the volume list in two passes: names, nodes and consumers
// first, then the sizes, which come from a slow du-style scan.
func (t *volumesTab) load() {
	u := t.u
	r, ac, dcli, f, ctx := u.r, u.agents, u.dcli, u.f, u.ctx
	st := t.st()
	t.placeholder(loadingCell())
	gen := u.generation()
	go func() {
		start := time.Now()
		nodes, nerr := r.Nodes(ctx)
		var vs []swarmVolume
		var errs map[string]error
		var usage map[string][]volumeConsumer
		noAgent := false
		if nerr == nil {
			vs, errs = ac.Volumes(ctx, nodes, f.connectTimeout)
			usage = ac.VolumeUsage(ctx, nodes, f.connectTimeout)
			if len(nodes) > 0 && len(errs) == len(nodes) && !swarmops.AgentDeployed(ctx, dcli) {
				noAgent = true
			}
		}
		clientlog.Timed("ui.loadVolumes", start, nerr, "nodes", len(nodes), "vols", len(vs))
		u.onCluster(gen, func() {
			st.vols, st.usage, st.errs, st.sizes = vs, usage, errs, nil
			if nerr != nil {
				t.placeholder(errorCell(nerr))
				return
			}
			if noAgent {
				t.placeholder(stateCell(errNoAgent.Error(), tcell.ColorRed))
				return
			}
			t.render()
		})
		if nerr == nil && !noAgent {
			t.loadSizes(gen, st, nodes, ac, f.connectTimeout, ctx)
		}
	}()
}

// loadSizes is load's second pass: fill the SIZE column once the list is
// already shown, with a spinner on the SIZE header meanwhile. Off the UI
// goroutine, so everything cluster-specific — the agent pool, the timeout and
// the context — is handed in, captured by load on the UI goroutine. Reading
// them from u here would pick up whichever cluster is visible by now.
func (t *volumesTab) loadSizes(gen uint64, st *volumesState, nodes []resolve.Node, ac *agentcall.Client, timeout time.Duration, ctx context.Context) {
	u := t.u
	u.onCluster(gen, func() {
		st.sizesLoading = true
		t.render()
	})
	stop := make(chan struct{})
	go func() {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		tk := time.NewTicker(150 * time.Millisecond)
		defer tk.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tk.C:
				frame := frames[i%len(frames)]
				u.onCluster(gen, func() {
					if st.sizesLoading {
						t.table.SetCell(0, volSortCol[volSortSize], headerCell("SIZE "+frame))
					}
				})
			}
		}
	}()
	sz := ac.VolumeSizes(ctx, nodes, timeout)
	close(stop)
	u.onCluster(gen, func() {
		st.sizesLoading = false
		st.sizes = sz
		t.render()
	})
}

func (t *volumesTab) selected() (swarmVolume, bool) {
	return selectedRow(t.table, t.st().shown)
}

func (t *volumesTab) showNodes(v swarmVolume) {
	u := t.u
	ac, f := u.agents, u.f
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(volumeNodesTitle(v))
	sel := make([]bool, len(v.Nodes))
	render := func() { renderNodeMarks(list, v.Nodes, sel) }
	render()
	ov := u.overlayFor(pageVolNodes, t.table, "")
	closeNodes := ov.Close

	runDelete := func(targets []resolve.Node) {
		async(u, "ui.volumes.remove", func(ctx context.Context) ([]agentcall.NodeResult, error) {
			return ac.RemoveVolume(ctx, targets, v.Name, false, f.connectTimeout), nil
		}, func(results []agentcall.NodeResult, _ error) {
			closeNodes()
			t.load()
			u.info(fmt.Sprintf("volume %q:\n%s", v.Name, joinLines(nodeRemoveLines(results))))
		})
	}
	confirmDelete := func(targets []resolve.Node) {
		u.confirm(fmt.Sprintf("Remove volume %q on %d node(s)?\n%s", v.Name, len(targets), joinNodes(targets)),
			"Delete", list, func() { runDelete(targets) })
	}

	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape:
			closeNodes()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == ' ':
			if i := list.GetCurrentItem(); i < len(sel) {
				sel[i] = !sel[i]
				render()
			}
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
			confirmDelete(v.Nodes)
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == u.km.VolDelete:
			// The tab's own delete key (volume_delete), so one key deletes a
			// volume whether you are on the list or in its per-node view.
			if chosen := chosenNodes(v.Nodes, sel, list.GetCurrentItem()); len(chosen) > 0 {
				confirmDelete(chosen)
			}
			return nil
		}
		return vimListKeys(ev)
	})
	help := tview.NewTextView().SetDynamicColors(true).SetText(
		" [yellow]space[white] select  " + keyTag(dangerKey(keyLabel(u.km.VolDelete))) + " delete selected  " + keyTag(dangerKey("a")) + " delete all  [yellow]ESC[white] back")
	box := tview.NewFlex().SetDirection(tview.FlexRow)
	labelH := 0
	// The volume's labels (read-only) above the node list — Docker has no
	// volume-update API, so they can't be edited after creation. Kept out of
	// the list so the node row indices stay 1:1 with v.Nodes.
	if lbls := swarmops.KVPairs(v.Labels); len(lbls) > 0 {
		lv := tview.NewTextView().SetDynamicColors(true).
			SetText(" [gray]labels[-]\n   " + tview.Escape(strings.Join(lbls, "\n   ")))
		labelH = len(lbls) + 1
		box.AddItem(lv, labelH, 0, false)
	}
	box.AddItem(list, 0, 1, true).AddItem(help, 1, 0, false)
	ov.show(centered(box, 64, len(v.Nodes)+5+labelH), list)
}

// volumeNodesTitle is the per-node view's title: the volume, its node count,
// and when it was created if known.
func volumeNodesTitle(v swarmVolume) string {
	if !v.Created.IsZero() {
		return fmt.Sprintf(" volume %s — %d node(s) · created %s ", shortVolume(v.Name), len(v.Nodes), volumeCreated(v.Created))
	}
	return fmt.Sprintf(" volume %s — %d node(s) ", shortVolume(v.Name), len(v.Nodes))
}

// renderNodeMarks (re)fills the per-node list, a check mark per selected
// node, keeping the cursor where it was.
func renderNodeMarks(list *tview.List, nodes []resolve.Node, sel []bool) {
	cur := list.GetCurrentItem()
	list.Clear()
	for i, n := range nodes {
		mark := "[ ]"
		if sel[i] {
			mark = "[x]"
		}
		// Escaped: a List reads style tags, and "[x]" is one — the mark
		// of a selected node vanished, so space seemed to do nothing.
		list.AddItem(tview.Escape(fmt.Sprintf("%s %s", mark, n.Name)), "", 0, nil)
	}
	if cur < list.GetItemCount() {
		list.SetCurrentItem(cur)
	}
}

// nodeRemoveLines is one "node: removed" or "node: error: …" line per node.
func nodeRemoveLines(results []agentcall.NodeResult) []string {
	var b []string
	for _, res := range results {
		if res.Err != nil {
			b = append(b, fmt.Sprintf("%s: error: %v", res.Node.Name, res.Err))
		} else {
			b = append(b, fmt.Sprintf("%s: removed", res.Node.Name))
		}
	}
	return b
}

// chosenNodes is what the per-node view's delete key acts on: the marked
// nodes, or else the one under the cursor.
func chosenNodes(nodes []resolve.Node, sel []bool, cur int) []resolve.Node {
	var chosen []resolve.Node
	for i, s := range sel {
		if s {
			chosen = append(chosen, nodes[i])
		}
	}
	if len(chosen) == 0 && cur >= 0 && cur < len(nodes) {
		chosen = []resolve.Node{nodes[cur]}
	}
	return chosen
}

// showCreate opens a form to create a volume (default driver local) with
// labels, targeting one node or (blank) all nodes — volumes are node-local, so
// creation goes to each target node's agent.
func (t *volumesTab) showCreate() {
	u := t.u
	r, ac, f := u.r, u.agents, u.f
	o := newVolumeOpts{Driver: "local"}
	var labels, nodeSel string
	var allNodes []resolve.Node // fetched on open, for autocomplete + targeting
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" new volume ")
	form.AddInputField("Name", "", 32, nil, func(t string) { o.Name = t })
	form.AddInputField("Driver", "local", 20, nil, func(t string) { o.Driver = t })
	form.AddInputField("Labels (k=v,k=v)", "", 40, nil, func(t string) { labels = t })
	form.AddInputField("Node (blank = all nodes)", "", 24, nil, func(t string) { nodeSel = t })
	if nf, ok := form.GetFormItem(3).(*tview.InputField); ok {
		nf.SetAutocompleteFunc(func(text string) []string {
			text = strings.ToLower(strings.TrimSpace(text))
			var out []string
			for _, name := range nodeNames(allNodes) {
				if text == "" || strings.Contains(strings.ToLower(name), text) {
					out = append(out, name)
				}
			}
			return out
		})
	}
	async(u, "ui.volumes.create.nodes", r.Nodes, func(ns []resolve.Node, err error) {
		if err == nil {
			allNodes = ns
		}
	})
	ov := u.overlayFor(pageVolForm, t.table, footerKeys("Tab", "next field", "Enter", "confirm", "Esc", "cancel"))
	closeForm := ov.Close
	form.AddButton("Create", func() {
		lbls, err := swarmops.ParseKVList(labels)
		if err != nil {
			u.info("invalid labels: " + err.Error())
			return
		}
		o.Labels = lbls
		req, err := buildVolumeCreateReq(o)
		if err != nil {
			u.info(err.Error())
			return
		}
		targets := allNodes
		if s := strings.TrimSpace(nodeSel); s != "" {
			targets = filterNodes(allNodes, []string{s})
			if len(targets) == 0 {
				u.info(fmt.Sprintf("no node named %q", s))
				return
			}
		}
		if len(targets) == 0 {
			u.info("no nodes available")
			return
		}
		async(u, "ui.volumes.create", func(ctx context.Context) ([]agentcall.NodeResult, error) {
			return ac.CreateVolume(ctx, targets, req, f.connectTimeout), nil
		}, func(results []agentcall.NodeResult, _ error) {
			closeForm()
			t.load()
			ok := 0
			var failed []string
			for _, res := range results {
				if res.Err != nil {
					failed = append(failed, res.Node.Name+": "+res.Err.Error())
				} else {
					ok++
				}
			}
			if len(failed) == 0 {
				u.flash(fmt.Sprintf(" [green]created[white] volume %s on %d node(s)", req.Name, ok))
				return
			}
			u.info(fmt.Sprintf("volume %q: %d ok, %d failed\n%s", req.Name, ok, len(failed), joinLines(failed)))
		})
	})
	form.AddButton("Cancel", closeForm)
	form.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			closeForm()
			return nil
		}
		return ev
	})
	ov.show(centered(form, 66, 15), form)
}

// showConsumers lists the services/containers that mount a volume.
func (t *volumesTab) showConsumers(v swarmVolume) {
	u := t.u
	consumers := t.st().usage[v.Name]
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" %s — used by %d ", v.Name, len(consumers)))
	if len(consumers) == 0 {
		list.AddItem("(not in use by any container)", "", 0, nil)
	} else {
		svcW, contW := 0, 0
		for _, c := range consumers {
			if w := len(orDash(c.Service)); w > svcW {
				svcW = w
			}
			if w := len(orDash(c.Container)); w > contW {
				contW = w
			}
		}
		for _, c := range consumers {
			list.AddItem(fmt.Sprintf("%-*s  %-*s  on %s", svcW, orDash(c.Service), contW, orDash(c.Container), orDash(c.Node)), "", 0, nil)
		}
	}
	ov := u.overlayFor(pageVolUsers, t.table, footerKeys("j/k", "move", "Esc", "back"))
	closeUsers := ov.Close
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == u.km.Quit || ev.Rune() == 'i')) {
			closeUsers()
			return nil
		}
		return vimListKeys(ev)
	})
	rows := len(consumers)
	if rows == 0 {
		rows = 1
	}
	ov.show(centered(list, 72, rows+4), list)
}

// deleteVolumes removes each target volume on every node that holds it, behind
// a single confirm. Volumes are node-local, so a volume is removed across all
// its v.Nodes. Shared by the multi-select delete and prune.
func (t *volumesTab) deleteVolumes(targets []swarmVolume, prompt string) {
	u := t.u
	if len(targets) == 0 {
		return
	}
	shown := make([]string, 0, len(targets))
	for _, v := range targets {
		shown = append(shown, shortVolume(v.Name))
	}
	extra := 0
	if len(shown) > 12 {
		extra, shown = len(shown)-12, shown[:12]
	}
	body := prompt + "\n\n" + strings.Join(shown, "\n")
	if extra > 0 {
		body += fmt.Sprintf("\n(+%d more)", extra)
	}
	u.confirm(body, "Delete", t.table, func() { t.runDelete(targets) })
}

// runDelete is the confirmed half of deleteVolumes.
func (t *volumesTab) runDelete(targets []swarmVolume) {
	u := t.u
	ac, f := u.agents, u.f
	// Deleting runs per volume across every node it holds, which can take a
	// while, so show a progress overlay instead of freezing.
	prog := tview.NewTextView().SetTextAlign(tview.AlignCenter).SetDynamicColors(true)
	prog.SetBorder(true).SetTitle(" deleting volumes ")
	prog.SetText(fmt.Sprintf("\ndeleted 0/%d…", len(targets)))
	// No focus to give back: the summary notice that replaces it takes it.
	pov := u.overlayFor(pageVolProgress, nil, "")
	pov.show(centered(prog, 60, 5), prog)
	// The progress line is a second, streaming handoff: one update per
	// finished volume, through the same generation check as the result.
	gen := u.generation()
	progress := func(d int) {
		u.onCluster(gen, func() { prog.SetText(fmt.Sprintf("\ndeleted %d/%d…", d, len(targets))) })
	}
	type outcome struct {
		removed int
		fails   []string
	}
	st := t.st()
	async(u, "ui.volumes.delete", func(ctx context.Context) (outcome, error) {
		// Delete volumes with bounded parallelism: each volume already fans
		// out across its nodes, so a small volume-level pool keeps the total
		// load on the agents in check. A mutex guards the shared counters
		// and the progress overlay shows completions.
		var (
			mu      sync.Mutex
			fails   []string
			done    int
			removed int
		)
		sem := make(chan struct{}, volumeDeleteFanout)
		var wg sync.WaitGroup
		for _, v := range targets {
			wg.Add(1)
			sem <- struct{}{}
			go func(v swarmVolume) {
				defer wg.Done()
				defer func() { <-sem }()
				ok := true
				var vf []string
				for _, res := range ac.RemoveVolume(ctx, v.Nodes, v.Name, false, f.connectTimeout) {
					if res.Err != nil {
						ok = false
						vf = append(vf, fmt.Sprintf("%s on %s: %v", shortVolume(v.Name), res.Node.Name, res.Err))
					}
				}
				mu.Lock()
				done++
				if ok {
					removed++
				}
				fails = append(fails, vf...)
				d := done
				mu.Unlock()
				progress(d)
			}(v)
		}
		wg.Wait()
		return outcome{removed, fails}, nil
	}, func(o outcome, _ error) {
		pov.Close()
		st.selected = map[string]bool{}
		t.load()
		u.updateStatus()
		summary := fmt.Sprintf("removed %d of %d volume(s)", o.removed, len(targets))
		if len(o.fails) > 0 {
			summary += ":\n" + joinLines(o.fails)
		}
		u.info(summary)
	})
}

// prune deletes every volume that no running container mounts and no service
// declares (service-declared volumes are spared even with no running task).
// The service check needs a ServiceList, so it runs off the UI goroutine.
func (t *volumesTab) prune() {
	u := t.u
	dcli := u.dcli
	async(u, "ui.volumes.prune.scan", func(ctx context.Context) (map[string]bool, error) {
		return swarmops.ServiceVolumeNames(ctx, dcli), nil
	}, func(declared map[string]bool, _ error) {
		st := t.st()
		var targets []swarmVolume
		for _, v := range st.vols {
			if len(st.usage[v.Name]) == 0 && !declared[v.Name] {
				targets = append(targets, v)
			}
		}
		if len(targets) == 0 {
			u.info("no unused volumes to prune (all are in use or declared by a service)")
			return
		}
		t.deleteVolumes(targets, fmt.Sprintf("Prune %d unused volume(s)? This cannot be undone.", len(targets)))
	})
}

// attachToService mounts a volume into a service: pick a service
// (autocomplete), enter the container target path, choose read-only or not,
// then a ServiceUpdate adds the mount.
func (t *volumesTab) attachToService(volName string) {
	u := t.u
	app, dcli, vtable := u.app, u.dcli, t.table
	in := tview.NewInputField().SetLabel("service: ").SetFieldWidth(42).
		SetPlaceholder("type or ↓ to pick; Enter next, Esc cancel")
	names := u.serviceNamesFromCache()
	in.SetAutocompleteFunc(func(text string) []string {
		text = strings.ToLower(strings.TrimSpace(text))
		var out []string
		for _, s := range names {
			if text == "" || strings.Contains(strings.ToLower(s), text) {
				out = append(out, s)
			}
		}
		return out
	})
	ov := u.overlayFor(pageVolAttach, nil, "")
	in.SetDoneFunc(func(key tcell.Key) {
		svc := strings.TrimSpace(in.GetText())
		ov.Close()
		if key != tcell.KeyEnter || svc == "" {
			app.SetFocus(vtable)
			return
		}
		tin := tview.NewInputField().SetLabel("target path: ").SetFieldWidth(42).SetPlaceholder("/data")
		tov := u.overlayFor(pageVolAttachTgt, nil, "")
		tin.SetDoneFunc(func(k tcell.Key) {
			target := strings.TrimSpace(tin.GetText())
			tov.Close()
			if k != tcell.KeyEnter || target == "" {
				app.SetFocus(vtable)
				return
			}
			if !strings.HasPrefix(target, "/") {
				u.info("target must be an absolute path")
				return
			}
			cov := u.overlayFor(pageVolAttachConfirm, nil, "")
			m := tview.NewModal().
				SetText(fmt.Sprintf("Attach volume %q to service %q at %s?\n\nThis triggers a rolling update of the service.", volName, svc, target)).
				AddButtons([]string{"Attach", "Attach read-only", "Cancel"}).
				SetDoneFunc(func(_ int, lbl string) {
					cov.Close()
					if lbl == "Cancel" || lbl == "" {
						app.SetFocus(vtable)
						return
					}
					mnt := mount.Mount{Type: mount.TypeVolume, Source: volName, Target: target, ReadOnly: lbl == "Attach read-only"}
					asyncDo(u, "ui.volumes.attach", func(ctx context.Context) error {
						return swarmops.AddServiceMount(ctx, dcli, svc, mnt)
					}, func(err error) {
						app.SetFocus(vtable)
						if err != nil {
							u.info("attach failed: " + err.Error())
							return
						}
						t.load()
						u.info(fmt.Sprintf("attached volume %q to %q at %s — rolling update started", volName, svc, target))
					})
				})
			cov.show(newScrim(m), m)
		})
		tin.SetBorder(true).SetTitle(fmt.Sprintf(" attach %s → %s ", volName, svc))
		tov.show(centeredPrompt(tin, 64), tin)
	})
	in.SetBorder(true).SetTitle(fmt.Sprintf(" attach volume %q to service ", volName))
	ov.show(centeredPrompt(in, 64), in)
}

// toggleSelect marks or unmarks the volume under the cursor for a bulk delete.
func (t *volumesTab) toggleSelect() {
	v, ok := t.selected()
	if !ok {
		return
	}
	st := t.st()
	if st.selected[v.Name] {
		delete(st.selected, v.Name)
	} else {
		st.selected[v.Name] = true
	}
	t.render()
	t.u.updateStatus()
}

// toggleSelectAll selects every displayed volume, or deselects them when all
// of them already are.
func (t *volumesTab) toggleSelectAll() {
	st := t.st()
	all := len(st.shown) > 0
	for _, v := range st.shown {
		if !st.selected[v.Name] {
			all = false
			break
		}
	}
	for _, v := range st.shown {
		if all {
			delete(st.selected, v.Name)
		} else {
			st.selected[v.Name] = true
		}
	}
	t.render()
	t.u.updateStatus()
}

// deleteTargets is what the delete key acts on: the selected volumes, or the
// one under the cursor.
func (t *volumesTab) deleteTargets() []swarmVolume {
	st := t.st()
	if len(st.selected) > 0 {
		var targets []swarmVolume
		for _, v := range st.vols {
			if st.selected[v.Name] {
				targets = append(targets, v)
			}
		}
		return targets
	}
	if v, ok := t.selected(); ok {
		return []swarmVolume{v}
	}
	return nil
}

// keys is the key handler of the Volumes table.
func (t *volumesTab) keys(ev *tcell.EventKey) *tcell.EventKey {
	u, km := t.u, t.u.km
	if ev.Key() != tcell.KeyRune {
		return u.tabKeys(ev)
	}
	switch ev.Rune() {
	case km.Search:
		u.startSearch("volumes")
	case km.VolNew:
		t.showCreate()
	case km.VolAttach:
		if v, ok := t.selected(); ok {
			t.attachToService(v.Name)
		}
	case km.VolSelect:
		t.toggleSelect()
	case km.VolSelectAll:
		t.toggleSelectAll()
	case km.VolDelete:
		targets := t.deleteTargets()
		t.deleteVolumes(targets, fmt.Sprintf("Remove %d volume(s) on every node that holds them?", len(targets)))
	case km.VolPrune:
		t.prune()
	case km.VolUsedBy:
		// Which services/containers use the volume.
		if v, ok := t.selected(); ok {
			t.showConsumers(v)
		}
	case km.VolSort:
		// Cycle the sort field; pick a sensible default direction for it.
		t.sortField = (t.sortField + 1) % 5
		t.sortDesc = t.sortField != volSortName && t.sortField != volSortAge
		t.render()
	case km.VolSortRev:
		t.sortDesc = !t.sortDesc
		t.render()
	default:
		return u.tabKeys(ev)
	}
	return nil
}
