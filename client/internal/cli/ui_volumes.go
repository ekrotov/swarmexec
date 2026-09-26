// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"sort"
	"strings"
	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/resolve"
	"sync"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) volMatches(v swarmVolume) bool {
	q := strings.ToLower(strings.TrimSpace(u.volFilter))
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

// sortVolumes orders rows by the active field; for size/age an unknown value
// always sorts last (regardless of direction), with name as the tiebreaker.
func (u *ui) sortVolumes(rows []swarmVolume) {
	known := func(v swarmVolume) bool {
		switch u.sortField {
		case volSortSize:
			_, ok := u.volSizes[v.Name]
			return ok
		case volSortAge:
			return !v.Created.IsZero()
		default:
			return true
		}
	}
	less := func(a, b swarmVolume) bool {
		switch u.sortField {
		case volSortNodes:
			if len(a.Nodes) != len(b.Nodes) {
				return len(a.Nodes) < len(b.Nodes)
			}
		case volSortUsed:
			if ua, ub := len(u.volUsage[a.Name]), len(u.volUsage[b.Name]); ua != ub {
				return ua < ub
			}
		case volSortAge:
			if !a.Created.Equal(b.Created) {
				return a.Created.Before(b.Created) // earlier = older = "more age"
			}
		case volSortSize:
			if sa, sb := u.volSizes[a.Name], u.volSizes[b.Name]; sa != sb {
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
		if u.sortDesc {
			return less(b, a)
		}
		return less(a, b)
	})
}

func (u *ui) renderVolumeTable() {
	vtable := u.vtable
	// Keep the cursor on the same volume across re-render (sort/size refresh).
	selName := ""
	if row, _ := vtable.GetSelection(); row >= 1 {
		if c := vtable.GetCell(row, 0); c != nil {
			selName = strings.TrimLeft(c.Text, "▣ ") // drop the selection marker
		}
	}
	vtable.Clear()
	for c, h := range vHeaders {
		if volSortCol[u.sortField] == c {
			if u.sortDesc {
				h += " ▼"
			} else {
				h += " ▲"
			}
		}
		// While the (slow) size scan runs, show a loading marker on the SIZE
		// header; the spinner goroutine animates this cell.
		if c == volSortCol[volSortSize] && u.volSizesLoading {
			h += " " + loadingText
		}
		vtable.SetCell(0, c, headerCell(h))
	}
	// Build the displayed subset from the "/" filter, then sort it. The
	// displayed order must match the slice selectedVolume() indexes, so both
	// use shownVols — otherwise a filter/sort makes delete act on the wrong row.
	u.shownVols = u.shownVols[:0]
	for _, v := range u.vols {
		if u.volMatches(v) {
			u.shownVols = append(u.shownVols, v)
		}
	}
	u.sortVolumes(u.shownVols)
	selRow := 1
	for i, v := range u.shownVols {
		used := len(u.volUsage[v.Name])
		usedCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if used > 0 {
			usedCell = tview.NewTableCell(fmt.Sprintf("%d", used)).SetTextColor(tcell.ColorGreen).SetExpansion(1)
		}
		sizeCell := tview.NewTableCell("…").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if u.volSizes != nil {
			size, color := int64(-1), tcell.ColorGray
			if s, ok := u.volSizes[v.Name]; ok {
				size, color = s, tcell.ColorWhite
			}
			sizeCell = tview.NewTableCell(humanBytes(size)).SetTextColor(color).SetExpansion(1)
		}
		mark, nameColor := "  ", tcell.ColorWhite
		if u.selectedVols[v.Name] {
			mark, nameColor = "▣ ", tcell.ColorAqua
		}
		vtable.SetCell(i+1, 0, tview.NewTableCell(mark+v.Name).SetTextColor(nameColor).SetExpansion(1))
		vtable.SetCell(i+1, 1, tview.NewTableCell(orDash(v.Driver)).SetExpansion(1))
		vtable.SetCell(i+1, 2, tview.NewTableCell(fmt.Sprintf("%d: %s", len(v.Nodes), joinNodes(v.Nodes))).SetExpansion(1))
		vtable.SetCell(i+1, 3, usedCell)
		vtable.SetCell(i+1, 4, tview.NewTableCell(volumeAge(v.Created)).SetExpansion(1))
		vtable.SetCell(i+1, 5, sizeCell)
		if v.Name == selName {
			selRow = i + 1
		}
	}
	if len(u.shownVols) > 0 {
		vtable.Select(selRow, 0)
	}
	// Row 1 when the list is empty, so the note does not land on top of the
	// empty state below it.
	noteRow := len(u.shownVols) + 1
	if len(u.shownVols) == 0 {
		text := emptyText("volumes", keyHint(u.km.VolNew, "to create one"))
		if u.volFilter != "" {
			text = fmt.Sprintf("(no matches for %q)", u.volFilter)
		}
		vtable.SetCell(1, 0, emptyCell(text))
		noteRow = 2
	}
	if len(u.volErrs) > 0 {
		vtable.SetCell(noteRow, 0, stateCell(fmt.Sprintf("(%d node(s) unreachable)", len(u.volErrs)), tcell.ColorYellow))
	}
}

func (u *ui) loadVolumes() {
	r, cfg, dcli, f, ctx, vtable := u.r, u.cfg, u.dcli, u.f, u.ctx, u.vtable
	tableState(vtable, vHeaders, loadingCell())
	gen := u.generation()
	go func() {
		start := time.Now()
		nodes, nerr := r.Nodes(ctx)
		var vs []swarmVolume
		var errs map[string]error
		var usage map[string][]volumeConsumer
		noAgent := false
		if nerr == nil {
			vs, errs = indexVolumes(ctx, cfg, nodes, f.connectTimeout)
			usage = indexVolumeUsage(ctx, cfg, nodes, f.connectTimeout)
			if len(nodes) > 0 && len(errs) == len(nodes) && !agentDeployed(ctx, dcli) {
				noAgent = true
			}
		}
		clientlog.Timed("ui.loadVolumes", start, nerr, "nodes", len(nodes), "vols", len(vs))
		u.onCluster(gen, func() {
			u.vols, u.volUsage, u.volErrs, u.volSizes = vs, usage, errs, nil
			if nerr != nil {
				tableState(vtable, vHeaders, errorCell(nerr))
				return
			}
			if noAgent {
				tableState(vtable, vHeaders, stateCell(errNoAgent.Error(), tcell.ColorRed))
				return
			}
			u.renderVolumeTable()
		})

		// Sizes are computed via a du-style disk-usage scan, which is slow, so
		// fill the SIZE column in a second pass once the list is already shown.
		// A spinner on the SIZE header makes clear the data is still loading.
		if nerr == nil && !noAgent {
			u.onCluster(gen, func() {
				u.volSizesLoading = true
				u.renderVolumeTable()
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
							if u.volSizesLoading {
								vtable.SetCell(0, volSortCol[volSortSize], headerCell("SIZE "+frame))
							}
						})
					}
				}
			}()
			sz := indexVolumeSizes(ctx, cfg, nodes, f.connectTimeout)
			close(stop)
			u.onCluster(gen, func() {
				u.volSizesLoading = false
				u.volSizes = sz
				u.renderVolumeTable()
			})
		}
	}()
}

func (u *ui) selectedVolume() (swarmVolume, bool) {
	vtable := u.vtable
	return selectedRow(vtable, u.shownVols)
}

func (u *ui) showVolumeNodes(v swarmVolume) {
	app, cfg, f, ctx, vtable := u.app, u.cfg, u.f, u.ctx, u.vtable
	list := tview.NewList().ShowSecondaryText(false)
	title := fmt.Sprintf(" volume %s — %d node(s) ", shortVolume(v.Name), len(v.Nodes))
	if !v.Created.IsZero() {
		title = fmt.Sprintf(" volume %s — %d node(s) · created %s ", shortVolume(v.Name), len(v.Nodes), volumeCreated(v.Created))
	}
	list.SetBorder(true).SetTitle(title)
	sel := make([]bool, len(v.Nodes))
	render := func() {
		cur := list.GetCurrentItem()
		list.Clear()
		for i, n := range v.Nodes {
			mark := "[ ]"
			if sel[i] {
				mark = "[x]"
			}
			list.AddItem(fmt.Sprintf("%s %s", mark, n.Name), "", 0, nil)
		}
		if cur < list.GetItemCount() {
			list.SetCurrentItem(cur)
		}
	}
	render()
	ov := u.overlayFor(pageVolNodes, vtable, "")
	closeNodes := ov.Close

	runDelete := func(targets []resolve.Node) {
		go func() {
			results := removeOnNodes(ctx, cfg, targets, v.Name, false, f.connectTimeout)
			app.QueueUpdateDraw(func() {
				closeNodes()
				u.loadVolumes()
				var b []string
				for _, res := range results {
					if res.err != nil {
						b = append(b, fmt.Sprintf("%s: error: %v", res.node.Name, res.err))
					} else {
						b = append(b, fmt.Sprintf("%s: removed", res.node.Name))
					}
				}
				u.info(fmt.Sprintf("volume %q:\n%s", v.Name, joinLines(b)))
			})
		}()
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
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
			var chosen []resolve.Node
			for i, s := range sel {
				if s {
					chosen = append(chosen, v.Nodes[i])
				}
			}
			if len(chosen) == 0 {
				if i := list.GetCurrentItem(); i < len(v.Nodes) {
					chosen = []resolve.Node{v.Nodes[i]}
				}
			}
			if len(chosen) > 0 {
				confirmDelete(chosen)
			}
			return nil
		}
		return vimListKeys(ev)
	})
	help := tview.NewTextView().SetDynamicColors(true).SetText(
		" [yellow]space[white] select  [yellow]d[white] delete selected  [yellow]a[white] delete all  [yellow]ESC[white] back")
	box := tview.NewFlex().SetDirection(tview.FlexRow)
	labelH := 0
	// The volume's labels (read-only) above the node list — Docker has no
	// volume-update API, so they can't be edited after creation. Kept out of
	// the list so the node row indices stay 1:1 with v.Nodes.
	if lbls := kvPairs(v.Labels); len(lbls) > 0 {
		lv := tview.NewTextView().SetDynamicColors(true).
			SetText(" [gray]labels[-]\n   " + tview.Escape(strings.Join(lbls, "\n   ")))
		labelH = len(lbls) + 1
		box.AddItem(lv, labelH, 0, false)
	}
	box.AddItem(list, 0, 1, true).AddItem(help, 1, 0, false)
	ov.show(centered(box, 64, len(v.Nodes)+5+labelH), list)
}

// showCreateVolume opens a form to create a volume (default driver local) with
// labels, targeting one node or (blank) all nodes — volumes are node-local, so
// creation goes to each target node's agent.
func (u *ui) showCreateVolume() {
	app, r, cfg, f, ctx, vtable := u.app, u.r, u.cfg, u.f, u.ctx, u.vtable
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
	go func() {
		ns, err := r.Nodes(ctx)
		app.QueueUpdateDraw(func() {
			if err == nil {
				allNodes = ns
			}
		})
	}()
	ov := u.overlayFor(pageVolForm, vtable, footerKeys("Tab", "next field", "Enter", "confirm", "Esc", "cancel"))
	closeForm := ov.Close
	form.AddButton("Create", func() {
		lbls, err := parseKVList(labels)
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
		go func() {
			results := createVolumeOnNodes(ctx, cfg, targets, req, f.connectTimeout)
			app.QueueUpdateDraw(func() {
				closeForm()
				u.loadVolumes()
				ok := 0
				var failed []string
				for _, res := range results {
					if res.err != nil {
						failed = append(failed, res.node.Name+": "+res.err.Error())
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
		}()
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

// showVolumeConsumers lists the services/containers that mount a volume.
func (u *ui) showVolumeConsumers(v swarmVolume) {
	vtable := u.vtable
	consumers := u.volUsage[v.Name]
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
	ov := u.overlayFor(pageVolUsers, vtable, footerKeys("j/k", "move", "Esc", "back"))
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
func (u *ui) deleteVolumes(targets []swarmVolume, prompt string) {
	app, cfg, f, ctx, vtable := u.app, u.cfg, u.f, u.ctx, u.vtable
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
	u.confirm(body, "Delete", vtable, func() {
		// Deleting runs per volume across every node it holds, which can
		// take a while, so show a progress overlay instead of freezing.
		prog := tview.NewTextView().SetTextAlign(tview.AlignCenter).SetDynamicColors(true)
		prog.SetBorder(true).SetTitle(" deleting volumes ")
		prog.SetText(fmt.Sprintf("\ndeleted 0/%d…", len(targets)))
		// No focus to give back: the summary notice that replaces it takes it.
		pov := u.overlayFor(pageVolProgress, nil, "")
		pov.show(centered(prog, 60, 5), prog)
		go func() {
			// Delete volumes with bounded parallelism: each volume already
			// fans out across its nodes, so a small volume-level pool keeps
			// the total load on the agents in check. A mutex guards the
			// shared counters and the progress overlay shows completions.
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
					for _, res := range removeOnNodes(ctx, cfg, v.Nodes, v.Name, false, f.connectTimeout) {
						if res.err != nil {
							ok = false
							vf = append(vf, fmt.Sprintf("%s on %s: %v", shortVolume(v.Name), res.node.Name, res.err))
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
					app.QueueUpdateDraw(func() {
						prog.SetText(fmt.Sprintf("\ndeleted %d/%d…", d, len(targets)))
					})
				}(v)
			}
			wg.Wait()
			app.QueueUpdateDraw(func() {
				pov.Close()
				u.selectedVols = map[string]bool{}
				u.loadVolumes()
				u.updateStatus()
				summary := fmt.Sprintf("removed %d of %d volume(s)", removed, len(targets))
				if len(fails) > 0 {
					summary += ":\n" + joinLines(fails)
				}
				u.info(summary)
			})
		}()
	})
}

// pruneVolumes deletes every volume that no running container mounts and no
// service declares (service-declared volumes are spared even with no running
// task). The service check needs a ServiceList, so it runs off the UI goroutine.
func (u *ui) pruneVolumes() {
	app, dcli, ctx := u.app, u.dcli, u.ctx
	go func() {
		declared := serviceVolumeNames(ctx, dcli)
		app.QueueUpdateDraw(func() {
			var targets []swarmVolume
			for _, v := range u.vols {
				if len(u.volUsage[v.Name]) == 0 && !declared[v.Name] {
					targets = append(targets, v)
				}
			}
			if len(targets) == 0 {
				u.info("no unused volumes to prune (all are in use or declared by a service)")
				return
			}
			u.deleteVolumes(targets, fmt.Sprintf("Prune %d unused volume(s)? This cannot be undone.", len(targets)))
		})
	}()
}

func (u *ui) attachVolumeToService(volName string) {
	app, dcli, ctx, vtable := u.app, u.dcli, u.ctx, u.vtable
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
					go func() {
						err := addServiceMount(ctx, dcli, svc, mnt)
						app.QueueUpdateDraw(func() {
							app.SetFocus(vtable)
							if err != nil {
								u.info("attach failed: " + err.Error())
								return
							}
							u.loadVolumes()
							u.info(fmt.Sprintf("attached volume %q to %q at %s — rolling update started", volName, svc, target))
						})
					}()
				})
			cov.show(newScrim(m), m)
		})
		tin.SetBorder(true).SetTitle(fmt.Sprintf(" attach %s → %s ", volName, svc))
		tov.show(centeredPrompt(tin, 64), tin)
	})
	in.SetBorder(true).SetTitle(fmt.Sprintf(" attach volume %q to service ", volName))
	ov.show(centeredPrompt(in, 64), in)
}

// volumeKeys is the key handler of the Volumes table.
//
// On the volumes table, "i" shows which services/containers use the volume.
// attachVolumeToService mounts a volume into a service from the Volumes tab:
// pick a service (autocomplete), enter the container target path, choose
// read-only or not, then a ServiceUpdate adds the mount.
func (u *ui) volumeKeys(ev *tcell.EventKey) *tcell.EventKey {
	km := u.km
	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case km.Search:
			u.startSearch("volumes")
			return nil
		case km.VolNew:
			u.showCreateVolume()
			return nil
		case km.VolAttach:
			if v, ok := u.selectedVolume(); ok {
				u.attachVolumeToService(v.Name)
			}
			return nil
		case km.VolSelect:
			// Toggle the current volume's selection for a bulk delete.
			if v, ok := u.selectedVolume(); ok {
				if u.selectedVols[v.Name] {
					delete(u.selectedVols, v.Name)
				} else {
					u.selectedVols[v.Name] = true
				}
				u.renderVolumeTable()
				u.updateStatus()
			}
			return nil
		case km.VolSelectAll:
			// Select or deselect all currently displayed volumes.
			all := len(u.shownVols) > 0
			for _, v := range u.shownVols {
				if !u.selectedVols[v.Name] {
					all = false
					break
				}
			}
			for _, v := range u.shownVols {
				if all {
					delete(u.selectedVols, v.Name)
				} else {
					u.selectedVols[v.Name] = true
				}
			}
			u.renderVolumeTable()
			u.updateStatus()
			return nil
		case km.VolDelete:
			// Delete the selected volumes, or the one under the cursor.
			var targets []swarmVolume
			if len(u.selectedVols) > 0 {
				for _, v := range u.vols {
					if u.selectedVols[v.Name] {
						targets = append(targets, v)
					}
				}
			} else if v, ok := u.selectedVolume(); ok {
				targets = []swarmVolume{v}
			}
			u.deleteVolumes(targets, fmt.Sprintf("Remove %d volume(s) on every node that holds them?", len(targets)))
			return nil
		case km.VolPrune:
			u.pruneVolumes()
			return nil
		case km.VolUsedBy:
			if v, ok := u.selectedVolume(); ok {
				u.showVolumeConsumers(v)
			}
			return nil
		case km.VolSort:
			// Cycle the sort field; pick a sensible default direction for it.
			u.sortField = (u.sortField + 1) % 5
			u.sortDesc = u.sortField != volSortName && u.sortField != volSortAge
			u.renderVolumeTable()
			return nil
		case km.VolSortRev:
			u.sortDesc = !u.sortDesc
			u.renderVolumeTable()
			return nil
		}
	}
	return u.tabKeys(ev)
}
