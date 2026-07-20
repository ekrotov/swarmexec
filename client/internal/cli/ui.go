// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/session"
	cterm "swarmexec/client/internal/term"
)

type uiFlags struct {
	connectTimeout time.Duration
}

func newUICmd(g *globalFlags) *cobra.Command {
	f := &uiFlags{}
	cmd := &cobra.Command{
		Use:   "ui [service]",
		Short: "Interactive view of containers and volumes",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUI(cmd, g, f, args)
		},
	}
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to an agent")
	return cmd
}

func runUI(cmd *cobra.Command, g *globalFlags, f *uiFlags, args []string) error {
	cfg, err := g.resolveConfig(cmd)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if !cterm.IsTerminal(os.Stdout.Fd()) || !cterm.IsTerminal(os.Stdin.Fd()) {
		return &cliError{code: usageExitCode, err: fmt.Errorf("ui needs an interactive terminal (use plain `ps`/`volume ls` when piping)")}
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	var service string
	if len(args) == 1 {
		service = args[0]
	}

	dcli, err := newDockerClient(g.dockerContext)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))

	app := tview.NewApplication()
	pages := tview.NewPages()   // overlays: menus, terminal, logs, volume nodes
	content := tview.NewPages() // the two tabs

	selStyle := tcell.StyleDefault.Background(tcell.ColorTeal).Foreground(tcell.ColorWhite)

	// generic info modal.
	info := func(msg string) {
		m := tview.NewModal().SetText(msg).AddButtons([]string{"OK"}).
			SetDoneFunc(func(int, string) { pages.RemovePage("info") })
		pages.AddPage("info", m, true, true)
		app.SetFocus(m)
	}

	// ---------------------------------------------------------------- containers
	// A tree: services are parent nodes, their containers are children.
	ctree := tview.NewTreeView()
	croot := tview.NewTreeNode("")
	ctree.SetRoot(croot).SetTopLevel(1) // hide the synthetic root; services are top-level
	// filter holds the active "/" search query; empty means show everything. A
	// candidate matches when the query is a substring of its service, container
	// id or node (case-insensitive).
	filter := ""
	matchesFilter := func(c resolve.Candidate) bool {
		if filter == "" {
			return true
		}
		q := strings.ToLower(filter)
		return strings.Contains(strings.ToLower(c.Service), q) ||
			strings.Contains(strings.ToLower(c.ContainerID), q) ||
			strings.Contains(strings.ToLower(c.NodeName), q)
	}
	// lastCands caches the most recent fetch so the "/" filter can re-render
	// locally without hitting the docker API on every keystroke (a remote call
	// over the ssh tunnel — doing it per keystroke makes typing crawl).
	var lastCands []resolve.Candidate
	renderContainers := func() {
		// Remember the selected container so a refresh/filter keeps the cursor.
		prevID := ""
		if n := ctree.GetCurrentNode(); n != nil {
			if ref, ok := n.GetReference().(resolve.Candidate); ok {
				prevID = ref.ContainerID
			}
		}
		croot.ClearChildren()
		cands := lastCands
		if filter != "" {
			kept := make([]resolve.Candidate, 0, len(cands))
			for _, c := range cands {
				if matchesFilter(c) {
					kept = append(kept, c)
				}
			}
			cands = kept
		}
		// Pre-compute column widths so every container row lines up, regardless
		// of node-name length or whether a task carries a slot.
		slotStr := func(c resolve.Candidate) string {
			if c.Slot > 0 {
				return fmt.Sprintf("slot %d", c.Slot)
			}
			return ""
		}
		nodeW, slotW := 0, 0
		for _, c := range cands {
			if w := len(orDash(c.NodeName)); w > nodeW {
				nodeW = w
			}
			if w := len(slotStr(c)); w > slotW {
				slotW = w
			}
		}
		var first, target, svcNode *tview.TreeNode
		curService := ""
		for _, c := range cands {
			if svcNode == nil || c.Service != curService {
				curService = c.Service
				svcNode = tview.NewTreeNode(orDash(c.Service)).SetColor(tcell.ColorAqua).SetExpanded(true)
				croot.AddChild(svcNode)
			}
			var label string
			if slotW > 0 {
				label = fmt.Sprintf("%-12s  %-*s  %-*s  up %s", shortID(c.ContainerID), nodeW, orDash(c.NodeName), slotW, slotStr(c), uptime(c.Uptime))
			} else {
				label = fmt.Sprintf("%-12s  %-*s  up %s", shortID(c.ContainerID), nodeW, orDash(c.NodeName), uptime(c.Uptime))
			}
			node := tview.NewTreeNode(label).SetReference(c)
			svcNode.AddChild(node)
			if first == nil {
				first = node
			}
			if c.ContainerID == prevID {
				target = node
			}
		}
		if len(croot.GetChildren()) == 0 {
			empty := "(no running tasks)"
			if filter != "" {
				empty = fmt.Sprintf("(no matches for %q)", filter)
			}
			croot.AddChild(tview.NewTreeNode(empty).SetColor(tcell.ColorGray).SetSelectable(false))
		}
		switch {
		case target != nil:
			ctree.SetCurrentNode(target)
		case first != nil:
			ctree.SetCurrentNode(first)
		}
	}
	loadContainers := func() {
		cands, lerr := r.Candidates(ctx, service)
		if lerr != nil {
			croot.ClearChildren()
			croot.AddChild(tview.NewTreeNode("error: " + lerr.Error()).SetColor(tcell.ColorRed).SetSelectable(false))
			return
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].Service != cands[j].Service {
				return cands[i].Service < cands[j].Service
			}
			if cands[i].Slot != cands[j].Slot {
				return cands[i].Slot < cands[j].Slot
			}
			return cands[i].NodeName < cands[j].NodeName
		})
		lastCands = cands
		renderContainers()
	}
	openTerminal := func(c resolve.Candidate, command []string, tty bool) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		tctx, tcancel := context.WithCancel(ctx)
		tv := newTerminalView(app)
		tv.SetTitle(fmt.Sprintf(" %v · %s · %s on %s — Ctrl-] detach ", command, orDash(c.Service), shortID(c.ContainerID), orDash(c.NodeName)))
		var once sync.Once
		closeTerm := func() {
			once.Do(func() {
				tcancel()
				pages.RemovePage("term")
				app.SetFocus(ctree)
				loadContainers()
			})
		}
		tv.detach = closeTerm
		tv.run(tctx, cfg, ep, command, tty, f.connectTimeout, func(code int, rerr error) {
			app.QueueUpdateDraw(func() {
				switch {
				case tctx.Err() != nil:
					closeTerm() // user detached (Ctrl-]) — just close
				case rerr != nil:
					closeTerm()
					info(enrichAgentError(ctx, dcli, rerr).Error())
				case code != 0:
					// The command failed (e.g. `bash` not in the image). Keep the
					// pane up with its output so the error stays readable.
					tv.showEnded(fmt.Sprintf("[swarmexec] %v exited with code %d — press any key to close", command, code))
				default:
					closeTerm() // clean exit
				}
			})
		})
		pages.AddPage("term", tv, true, true)
		app.SetFocus(tv)
	}

	showLogs := func(c resolve.Candidate) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
		follow := &atomic.Bool{}
		follow.Store(true)
		setTitle := func() {
			state := "ON"
			if !follow.Load() {
				state = "OFF"
			}
			tv.SetTitle(fmt.Sprintf(" logs %s on %s — [f] follow: %s · ↑/↓ scroll · ESC/q close ", shortID(c.ContainerID), orDash(c.NodeName), state))
		}
		tv.SetBorder(true)
		setTitle()
		lctx, lcancel := context.WithCancel(ctx)
		closeLogs := func() { lcancel(); pages.RemovePage("logs"); app.SetFocus(ctree) }
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == 'q'):
				closeLogs()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'f':
				follow.Store(!follow.Load())
				if follow.Load() {
					tv.ScrollToEnd() // re-enabling: jump to the newest line
				}
				setTitle()
				return nil
			}
			return ev
		})
		go func() {
			lerr := streamLogs(lctx, cfg, ep, logsParams{follow: true, tail: 1000, connectTimeout: f.connectTimeout},
				tvLogWriter{app: app, tv: tv, follow: follow}, tvLogWriter{app: app, tv: tv, stderr: true, follow: follow})
			if lerr != nil && lctx.Err() == nil {
				app.QueueUpdateDraw(func() { fmt.Fprintf(tv, "\n[red]error: %s[-]\n", tview.Escape(lerr.Error())) })
			}
		}()
		pages.AddPage("logs", tv, true, true)
		app.SetFocus(tv)
	}

	// showServiceLogs streams the logs of every container of a service into one
	// viewer, each line prefixed with [container@node].
	showServiceLogs := func(serviceName string, members []resolve.Candidate) {
		tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
		follow := &atomic.Bool{}
		follow.Store(true)
		setTitle := func() {
			state := "ON"
			if !follow.Load() {
				state = "OFF"
			}
			tv.SetTitle(fmt.Sprintf(" service logs %s (%d containers) — [f] follow: %s · ↑/↓ scroll · ESC/q close ", serviceName, len(members), state))
		}
		tv.SetBorder(true)
		setTitle()
		lctx, lcancel := context.WithCancel(ctx)
		closeLogs := func() { lcancel(); pages.RemovePage("logs"); app.SetFocus(ctree) }
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == 'q'):
				closeLogs()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'f':
				follow.Store(!follow.Load())
				if follow.Load() {
					tv.ScrollToEnd()
				}
				setTitle()
				return nil
			}
			return ev
		})
		for _, c := range members {
			ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
			prefix := fmt.Sprintf("[%s@%s] ", shortID(c.ContainerID), orDash(c.NodeName))
			go func(ep resolve.Endpoint, prefix string) {
				lerr := streamLogs(lctx, cfg, ep, logsParams{follow: true, tail: 200, connectTimeout: f.connectTimeout},
					&linePrefixWriter{app: app, tv: tv, prefix: prefix, follow: follow},
					&linePrefixWriter{app: app, tv: tv, prefix: prefix, stderr: true, follow: follow})
				if lerr != nil && lctx.Err() == nil {
					app.QueueUpdateDraw(func() {
						fmt.Fprintf(tv, "[red]%serror: %s[-]\n", prefix, tview.Escape(lerr.Error()))
					})
				}
			}(ep, prefix)
		}
		pages.AddPage("logs", tv, true, true)
		app.SetFocus(tv)
	}

	containerMenu := func(c resolve.Candidate) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		list := tview.NewList().ShowSecondaryText(false)
		list.SetBorder(true).SetTitle(fmt.Sprintf(" %s on %s — checking shells… ", orDash(c.Service), orDash(c.NodeName)))
		closeMenu := func() { pages.RemovePage("menu"); app.SetFocus(ctree) }

		// Optimistic until the shell probe returns; then unavailable shells grey.
		bashOK, shOK, probed := true, true, false
		render := func() {
			cur := list.GetCurrentItem()
			list.Clear()
			list.AddItem("Logs", "", 0, func() { closeMenu(); showLogs(c) })
			list.AddItem(shellLabel("Bash", bashOK, probed), "", 0, func() {
				if bashOK {
					closeMenu()
					openTerminal(c, []string{"bash"}, true)
				}
			})
			list.AddItem(shellLabel("Sh", shOK, probed), "", 0, func() {
				if shOK {
					closeMenu()
					openTerminal(c, []string{"sh"}, false)
				}
			})
			list.AddItem("Cancel", "", 0, closeMenu)
			if cur >= 0 && cur < list.GetItemCount() {
				list.SetCurrentItem(cur)
			}
		}
		render()
		list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape {
				closeMenu()
				return nil
			}
			return vimListKeys(ev)
		})

		go func() {
			b, s := probeShells(ctx, cfg, ep, f.connectTimeout)
			app.QueueUpdateDraw(func() {
				bashOK, shOK, probed = b, s, true
				list.SetTitle(fmt.Sprintf(" %s on %s — pick an action (ESC cancels) ", orDash(c.Service), orDash(c.NodeName)))
				render()
			})
		}()

		pages.AddPage("menu", centered(list, 48, 6), true, true)
		app.SetFocus(list)
	}
	ctree.SetSelectedFunc(func(node *tview.TreeNode) {
		if ref, ok := node.GetReference().(resolve.Candidate); ok {
			containerMenu(ref)
			return
		}
		// Service node → aggregated logs of all its containers.
		var members []resolve.Candidate
		for _, ch := range node.GetChildren() {
			if c, ok := ch.GetReference().(resolve.Candidate); ok {
				members = append(members, c)
			}
		}
		if len(members) > 0 {
			showServiceLogs(node.GetText(), members)
		}
	})

	// ------------------------------------------------------------------- volumes
	const (
		volSortName = iota
		volSortNodes
		volSortUsed
		volSortAge
		volSortSize
	)
	// volSortCol maps a sort field to the header column it annotates with ▲/▼.
	volSortCol := map[int]int{volSortName: 0, volSortNodes: 2, volSortUsed: 3, volSortAge: 4, volSortSize: 5}
	vtable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	vtable.SetSelectedStyle(selStyle)
	vHeaders := []string{"VOLUME", "DRIVER", "NODES", "USED BY", "AGE", "SIZE"}
	var vols []swarmVolume
	var volUsage map[string][]volumeConsumer
	var volSizes map[string]int64
	var volErrs map[string]error
	sortField := volSortName
	sortDesc := false
	volSizesLoading := false

	// sortVolumes orders rows by the active field; for size/age an unknown value
	// always sorts last (regardless of direction), with name as the tiebreaker.
	sortVolumes := func(rows []swarmVolume) {
		known := func(v swarmVolume) bool {
			switch sortField {
			case volSortSize:
				_, ok := volSizes[v.Name]
				return ok
			case volSortAge:
				return !v.Created.IsZero()
			default:
				return true
			}
		}
		less := func(a, b swarmVolume) bool {
			switch sortField {
			case volSortNodes:
				if len(a.Nodes) != len(b.Nodes) {
					return len(a.Nodes) < len(b.Nodes)
				}
			case volSortUsed:
				if ua, ub := len(volUsage[a.Name]), len(volUsage[b.Name]); ua != ub {
					return ua < ub
				}
			case volSortAge:
				if !a.Created.Equal(b.Created) {
					return a.Created.Before(b.Created) // earlier = older = "more age"
				}
			case volSortSize:
				if sa, sb := volSizes[a.Name], volSizes[b.Name]; sa != sb {
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
			if sortDesc {
				return less(b, a)
			}
			return less(a, b)
		})
	}

	renderVolumeTable := func() {
		// Keep the cursor on the same volume across re-render (sort/size refresh).
		selName := ""
		if row, _ := vtable.GetSelection(); row >= 1 {
			if c := vtable.GetCell(row, 0); c != nil {
				selName = c.Text
			}
		}
		vtable.Clear()
		for c, h := range vHeaders {
			if volSortCol[sortField] == c {
				if sortDesc {
					h += " ▼"
				} else {
					h += " ▲"
				}
			}
			// While the (slow) size scan runs, show a loading marker on the SIZE
			// header; the spinner goroutine animates this cell.
			if c == volSortCol[volSortSize] && volSizesLoading {
				h += " loading…"
			}
			vtable.SetCell(0, c, headerCell(h))
		}
		// Sort vols in place so the displayed order matches the slice that
		// selectedVolume() indexes — otherwise a non-name sort makes the delete /
		// nodes modal act on the wrong volume.
		sortVolumes(vols)
		selRow := 1
		for i, v := range vols {
			used := len(volUsage[v.Name])
			usedCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if used > 0 {
				usedCell = tview.NewTableCell(fmt.Sprintf("%d", used)).SetTextColor(tcell.ColorGreen).SetExpansion(1)
			}
			sizeCell := tview.NewTableCell("…").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if volSizes != nil {
				size, color := int64(-1), tcell.ColorGray
				if s, ok := volSizes[v.Name]; ok {
					size, color = s, tcell.ColorWhite
				}
				sizeCell = tview.NewTableCell(humanBytes(size)).SetTextColor(color).SetExpansion(1)
			}
			vtable.SetCell(i+1, 0, tview.NewTableCell(v.Name).SetExpansion(1))
			vtable.SetCell(i+1, 1, tview.NewTableCell(orDash(v.Driver)).SetExpansion(1))
			vtable.SetCell(i+1, 2, tview.NewTableCell(fmt.Sprintf("%d: %s", len(v.Nodes), joinNodes(v.Nodes))).SetExpansion(1))
			vtable.SetCell(i+1, 3, usedCell)
			vtable.SetCell(i+1, 4, tview.NewTableCell(volumeAge(v.Created)).SetExpansion(1))
			vtable.SetCell(i+1, 5, sizeCell)
			if v.Name == selName {
				selRow = i + 1
			}
		}
		if len(vols) > 0 {
			vtable.Select(selRow, 0)
		}
		if len(volErrs) > 0 {
			vtable.SetCell(len(vols)+1, 0, tview.NewTableCell(fmt.Sprintf("(%d node(s) unreachable)", len(volErrs))).SetTextColor(tcell.ColorYellow).SetSelectable(false))
		}
	}

	loadVolumes := func() {
		vtable.Clear()
		for c, h := range vHeaders {
			vtable.SetCell(0, c, headerCell(h))
		}
		vtable.SetCell(1, 0, tview.NewTableCell("loading…").SetTextColor(tcell.ColorGray))
		go func() {
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
			app.QueueUpdateDraw(func() {
				vols, volUsage, volErrs, volSizes = vs, usage, errs, nil
				if nerr != nil {
					vtable.Clear()
					for c, h := range vHeaders {
						vtable.SetCell(0, c, headerCell(h))
					}
					vtable.SetCell(1, 0, tview.NewTableCell("error: "+nerr.Error()).SetTextColor(tcell.ColorRed))
					return
				}
				if noAgent {
					vtable.Clear()
					for c, h := range vHeaders {
						vtable.SetCell(0, c, headerCell(h))
					}
					vtable.SetCell(1, 0, tview.NewTableCell(errNoAgent.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
					return
				}
				renderVolumeTable()
			})

			// Sizes are computed via a du-style disk-usage scan, which is slow, so
			// fill the SIZE column in a second pass once the list is already shown.
			// A spinner on the SIZE header makes clear the data is still loading.
			if nerr == nil && !noAgent {
				app.QueueUpdateDraw(func() {
					volSizesLoading = true
					renderVolumeTable()
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
							app.QueueUpdateDraw(func() {
								if volSizesLoading {
									vtable.SetCell(0, volSortCol[volSortSize], headerCell("SIZE "+frame))
								}
							})
						}
					}
				}()
				sz := indexVolumeSizes(ctx, cfg, nodes, f.connectTimeout)
				close(stop)
				app.QueueUpdateDraw(func() {
					volSizesLoading = false
					volSizes = sz
					renderVolumeTable()
				})
			}
		}()
	}
	selectedVolume := func() (swarmVolume, bool) {
		row, _ := vtable.GetSelection()
		i := row - 1
		if i < 0 || i >= len(vols) {
			return swarmVolume{}, false
		}
		return vols[i], true
	}

	showVolumeNodes := func(v swarmVolume) {
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
		closeNodes := func() { pages.RemovePage("volnodes"); app.SetFocus(vtable) }

		runDelete := func(targets []resolve.Node) {
			go func() {
				results := removeOnNodes(ctx, cfg, targets, v.Name, false, f.connectTimeout)
				app.QueueUpdateDraw(func() {
					closeNodes()
					loadVolumes()
					var b []string
					for _, res := range results {
						if res.err != nil {
							b = append(b, fmt.Sprintf("%s: error: %v", res.node.Name, res.err))
						} else {
							b = append(b, fmt.Sprintf("%s: removed", res.node.Name))
						}
					}
					info(fmt.Sprintf("volume %q:\n%s", v.Name, joinLines(b)))
				})
			}()
		}
		confirmDelete := func(targets []resolve.Node) {
			m := tview.NewModal().
				SetText(fmt.Sprintf("Remove volume %q on %d node(s)?\n%s", v.Name, len(targets), joinNodes(targets))).
				AddButtons([]string{"Delete", "Cancel"}).
				SetDoneFunc(func(_ int, label string) {
					pages.RemovePage("confirm")
					if label == "Delete" {
						runDelete(targets)
					} else {
						app.SetFocus(list)
					}
				})
			pages.AddPage("confirm", m, true, true)
			app.SetFocus(m)
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
		box := tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(list, 0, 1, true).
			AddItem(help, 1, 0, false)
		pages.AddPage("volnodes", centered(box, 64, len(v.Nodes)+5), true, true)
		app.SetFocus(list)
	}
	vtable.SetSelectedFunc(func(int, int) {
		if v, ok := selectedVolume(); ok && len(v.Nodes) > 0 {
			showVolumeNodes(v)
		}
	})

	// showVolumeConsumers lists the services/containers that mount a volume.
	showVolumeConsumers := func(v swarmVolume) {
		consumers := volUsage[v.Name]
		list := tview.NewList().ShowSecondaryText(false)
		list.SetBorder(true).SetTitle(fmt.Sprintf(" %s — used by %d — ESC back ", v.Name, len(consumers)))
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
		closeUsers := func() { pages.RemovePage("volusers"); app.SetFocus(vtable) }
		list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')) {
				closeUsers()
				return nil
			}
			return vimListKeys(ev)
		})
		rows := len(consumers)
		if rows == 0 {
			rows = 1
		}
		pages.AddPage("volusers", centered(list, 72, rows+4), true, true)
		app.SetFocus(list)
	}

	// ---------------------------------------------------------------- tabs/chrome
	content.AddPage("containers", ctree, true, true)
	content.AddPage("volumes", vtable, true, false)

	tabBar := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignCenter)
	help := tview.NewTextView().SetDynamicColors(true)
	// Right side of the footer: a live cluster summary (ready nodes / reachable
	// agents), filled in asynchronously so probing the agents never blocks the UI.
	cluster := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight)
	cluster.SetText("[gray]cluster: …[white] ")
	footer := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(help, 0, 1, false).
		AddItem(cluster, 30, 0, false)
	// "/" search bar: hidden (height 0) until activated; filters the container
	// tree live by service / container id / node.
	search := tview.NewInputField().SetLabel("/ ").SetFieldWidth(0)
	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tabBar, 1, 0, false).
		AddItem(content, 0, 1, true).
		AddItem(search, 0, 0, false).
		AddItem(footer, 1, 0, false)
	pages.AddPage("main", root, true, true)

	startSearch := func() {
		root.ResizeItem(search, 1, 0)
		app.SetFocus(search)
	}
	search.SetChangedFunc(func(text string) {
		// Filter locally against the cached candidates — no docker call per keystroke.
		filter = strings.TrimSpace(text)
		renderContainers()
	})
	search.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			search.SetText("") // clears the filter via SetChangedFunc and reloads
		}
		if filter == "" {
			root.ResizeItem(search, 0, 0) // nothing active — collapse the bar away
		}
		app.SetFocus(ctree) // Enter keeps the filter; the bar stays as an indicator
	})

	// refreshCluster probes the swarm in the background and updates the footer
	// summary. The agent probe (a Version RPC per node) can be slow, so it runs
	// off the UI goroutine and pushes the result back via QueueUpdateDraw.
	refreshCluster := func() {
		go func() {
			nodes, err := r.Nodes(ctx)
			if err != nil {
				app.QueueUpdateDraw(func() { cluster.SetText("[red]cluster: unreachable[white] ") })
				return
			}
			agents := 0
			for _, h := range checkNodes(ctx, cfg, nodes, f.connectTimeout) {
				if h.err == nil {
					agents++
				}
			}
			app.QueueUpdateDraw(func() {
				cluster.SetText(fmt.Sprintf("[aqua]%d[white] nodes · [aqua]%d[white]/%d agents ", len(nodes), agents, len(nodes)))
			})
		}()
	}

	active := "containers"
	mouseEnabled := true
	var screen tcell.Screen // set just before Run; used for clipboard (OSC52)
	curHelp := ""
	helpFor := func(name string) string {
		if name == "containers" {
			return " [yellow]j/k[white] up/down  [yellow]h/l[white] fold  [yellow]/[white] search  [yellow]Enter[white] menu  [yellow]y[white] copy  [yellow]m[white] mouse  [yellow]Tab/1/2[white] tabs  [yellow]r[white] refresh  [yellow]q[white] quit"
		}
		return " [yellow]j/k[white] up/down  [yellow]Enter[white] nodes  [yellow]i[white] used by  [yellow]s/S[white] sort/reverse  [yellow]y[white] copy  [yellow]m[white] mouse  [yellow]Tab/1/2[white] tabs  [yellow]r[white] refresh  [yellow]q[white] quit"
	}
	setTab := func(name string) {
		active = name
		content.SwitchToPage(name)
		curHelp = helpFor(name)
		help.SetText(curHelp)
		if name == "containers" {
			tabBar.SetText(" [black:teal] Containers (1) [-:-]   Volumes (2) ")
			app.SetFocus(ctree)
		} else {
			tabBar.SetText("  Containers (1)   [black:teal] Volumes (2) [-:-] ")
			app.SetFocus(vtable)
			loadVolumes()
		}
	}

	// flash briefly replaces the footer with a status message, then restores it.
	flash := func(msg string) {
		help.SetText(msg)
		go func() {
			time.Sleep(1500 * time.Millisecond)
			app.QueueUpdateDraw(func() { help.SetText(curHelp) })
		}()
	}

	// yankCurrent copies the active tab's list to the system clipboard via the
	// terminal (OSC52), so it also works over ssh when the terminal supports it.
	yankCurrent := func() {
		if screen == nil {
			return
		}
		var b strings.Builder
		if active == "containers" {
			for _, svc := range croot.GetChildren() {
				fmt.Fprintln(&b, svc.GetText())
				for _, c := range svc.GetChildren() {
					fmt.Fprintf(&b, "  %s\n", c.GetText())
				}
			}
		} else {
			fmt.Fprintln(&b, "NAME\tDRIVER\tNODES\tUSED BY\tAGE\tSIZE")
			rows := make([]swarmVolume, len(vols))
			copy(rows, vols)
			sortVolumes(rows)
			for _, v := range rows {
				used := "-"
				if n := len(volUsage[v.Name]); n > 0 {
					used = fmt.Sprintf("%d", n)
				}
				size := int64(-1)
				if s, ok := volSizes[v.Name]; ok {
					size = s
				}
				fmt.Fprintf(&b, "%s\t%s\t%d: %s\t%s\t%s\t%s\n", v.Name, orDash(v.Driver), len(v.Nodes), joinNodes(v.Nodes), used, volumeAge(v.Created), humanBytes(size))
			}
		}
		screen.SetClipboard([]byte(b.String()))
		flash(" [green]✓ copied to clipboard[white]")
	}

	// toggleMouse flips tview's mouse capture. With it off, the terminal's own
	// text selection / copy works again (tview otherwise grabs the mouse).
	toggleMouse := func() {
		mouseEnabled = !mouseEnabled
		app.EnableMouse(mouseEnabled)
		if mouseEnabled {
			flash(" [green]mouse ON[white] — app handles the mouse")
		} else {
			flash(" [green]mouse OFF[white] — select & copy with your terminal (m to re-enable)")
		}
	}

	tabKeys := func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyTab {
			if active == "containers" {
				setTab("volumes")
			} else {
				setTab("containers")
			}
			return nil
		}
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case '1':
				setTab("containers")
				return nil
			case '2':
				setTab("volumes")
				return nil
			case 'q':
				app.Stop()
				return nil
			case 'r':
				if active == "containers" {
					loadContainers()
				} else {
					loadVolumes()
				}
				refreshCluster()
				return nil
			case 'y':
				yankCurrent()
				return nil
			case 'm':
				toggleMouse()
				return nil
			case 'j':
				return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
			case 'k':
				return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
			}
		}
		return ev
	}
	// On the tree, "/" opens search; h/l collapse/expand (mapped to ←/→ so tview
	// handles the parent/child movement); j/k stay down/up via the shared keys.
	ctree.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case '/':
				startSearch()
				return nil
			case 'h':
				return tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModNone)
			case 'l':
				return tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone)
			}
		}
		return tabKeys(ev)
	})
	// On the volumes table, "i" shows which services/containers use the volume.
	vtable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case 'i':
				if v, ok := selectedVolume(); ok {
					showVolumeConsumers(v)
				}
				return nil
			case 's':
				// Cycle the sort field; pick a sensible default direction for it.
				sortField = (sortField + 1) % 5
				sortDesc = sortField != volSortName && sortField != volSortAge
				renderVolumeTable()
				return nil
			case 'S':
				sortDesc = !sortDesc
				renderVolumeTable()
				return nil
			}
		}
		return tabKeys(ev)
	})

	loadContainers()
	setTab("containers")
	refreshCluster()

	// Own the screen so we can post to the system clipboard (OSC52) on yank.
	scr, serr := tcell.NewScreen()
	if serr != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: init screen: %w", serr)}
	}
	screen = scr
	app.SetScreen(screen)

	if err := app.SetRoot(pages, true).EnableMouse(true).Run(); err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: %w", err)}
	}
	return nil
}

// isServiceNode reports whether a tree node is a service (group) node rather
// than a container leaf (containers carry a resolve.Candidate reference).
func isServiceNode(n *tview.TreeNode) bool {
	_, ok := n.GetReference().(resolve.Candidate)
	return !ok
}

// shortVolume abbreviates long anonymous-volume hashes (64-char hex) for display
// while leaving human-named volumes intact. The full name is still used for
// operations.
func shortVolume(name string) string {
	if len(name) >= 32 && isHexString(name) {
		return name[:12] + "…"
	}
	return name
}

func isHexString(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// shellLabel renders a menu shell entry, greying it once a probe confirms the
// container can't start that shell.
func shellLabel(name string, ok, probed bool) string {
	if probed && !ok {
		return "[gray]" + name + " (not available)[-]"
	}
	return name
}

// vimListKeys maps j/k/g/G to ↓/↑/Home/End for a tview.List. TextView brings
// these natively, but List does not: it reads runes as item shortcuts, and
// since our items carry no shortcut the lookup finds nothing and swallows the
// key. Every List that the operator navigates has to run its input through
// this, or vim movement silently dies the moment an overlay takes focus.
func vimListKeys(ev *tcell.EventKey) *tcell.EventKey {
	if ev.Key() != tcell.KeyRune {
		return ev
	}
	switch ev.Rune() {
	case 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	case 'g':
		return tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone)
	case 'G':
		return tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone)
	}
	return ev
}

func headerCell(text string) *tview.TableCell {
	return tview.NewTableCell(text).SetTextColor(tcell.ColorYellow).SetAttributes(tcell.AttrBold).SetSelectable(false)
}

func joinNodes(nodes []resolve.Node) string {
	return joinComma(nodeNames(nodes))
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}

// centered wraps p in a width×height box centered on screen (for modals).
func centered(p tview.Primitive, width, height int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(p, height, 1, true).
			AddItem(nil, 0, 1, false), width, 1, true).
		AddItem(nil, 0, 1, false)
}

// tvLogWriter appends streamed log bytes to a TextView on the UI goroutine,
// auto-scrolling to the end. stderr chunks are colored red.
type tvLogWriter struct {
	app    *tview.Application
	tv     *tview.TextView
	stderr bool
	follow *atomic.Bool // when set and false, new lines do not auto-scroll
}

func (w tvLogWriter) Write(p []byte) (int, error) {
	s := tview.Escape(string(p))
	w.app.QueueUpdateDraw(func() {
		if w.stderr {
			fmt.Fprintf(w.tv, "[red]%s[-]", s)
		} else {
			fmt.Fprint(w.tv, s)
		}
		if w.follow == nil || w.follow.Load() {
			w.tv.ScrollToEnd()
		}
	})
	return len(p), nil
}

// linePrefixWriter buffers partial lines and writes each complete line to a
// TextView with a fixed prefix — used to tag aggregated service logs with which
// container/node they came from.
type linePrefixWriter struct {
	app    *tview.Application
	tv     *tview.TextView
	prefix string
	stderr bool
	follow *atomic.Bool

	mu  sync.Mutex
	buf []byte
}

func (w *linePrefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	var lines []string
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	w.mu.Unlock()

	for _, line := range lines {
		s := tview.Escape(w.prefix + line)
		stderr := w.stderr
		w.app.QueueUpdateDraw(func() {
			if stderr {
				fmt.Fprintf(w.tv, "[red]%s[-]\n", s)
			} else {
				fmt.Fprintf(w.tv, "%s\n", s)
			}
			if w.follow == nil || w.follow.Load() {
				w.tv.ScrollToEnd()
			}
		})
	}
	return len(p), nil
}
