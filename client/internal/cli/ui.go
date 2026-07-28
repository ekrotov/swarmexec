// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/dockerctx"
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
		Short: "Interactive view of containers, volumes, networks, secrets and contexts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Loop so activating a context in the Contexts tab restarts the UI
			// cleanly against the chosen cluster; "" means a normal quit.
			ctxOverride := g.dockerContext
			for {
				next, err := runUI(cmd, g, f, args, ctxOverride)
				if err != nil {
					return err
				}
				if next == "" {
					return nil
				}
				ctxOverride = next
			}
		},
	}
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to an agent")
	return cmd
}

// runUI runs one session of the UI against ctxOverride's docker context. It
// returns the name of a context to switch to (the operator activated one in the
// Contexts tab) so the caller can restart cleanly against it, or "" on a normal
// quit.
//
// A context switch restarts the UI rather than re-pointing it live, for two
// reasons:
//   - Data races: the docker client and resolver are read by many background
//     goroutines (per-node volume/agent probes, the cluster-summary probe,
//     container loads). Reassigning them under those in-flight reads would be a
//     data race, so a live swap would need locking around every access.
//   - Cluster-bound state: active port-forwards, open exec/logs streams, the
//     async cluster summary and cached candidates all belong to the old cluster.
//     A fresh run tears them down (the deferred cleanups fire, old goroutines
//     drain) and rebuilds everything for the new cluster, so there is no
//     half-switched state — e.g. a forward left pointing at an old-cluster node.
func runUI(cmd *cobra.Command, g *globalFlags, f *uiFlags, args []string, ctxOverride string) (string, error) {
	cfg, err := g.resolveConfig(cmd)
	if err != nil {
		return "", &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return "", &cliError{code: usageExitCode, err: err}
	}
	if !cterm.IsTerminal(os.Stdout.Fd()) || !cterm.IsTerminal(os.Stdin.Fd()) {
		return "", &cliError{code: usageExitCode, err: fmt.Errorf("ui needs an interactive terminal (use plain `ps`/`volume ls` when piping)")}
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	var service string
	if len(args) == 1 {
		service = args[0]
	}

	// switchTo is set when the operator activates a context in the Contexts tab;
	// the UI then stops and the caller restarts against it.
	var switchTo string

	dcli, err := newDockerClient(ctxOverride)
	if err != nil {
		return "", &cliError{code: session.TransportFailure, err: err}
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

	// forwards is the UI's only persistent background resource: a port forward
	// outlives the overlay that started it, unlike every stream here.
	forwards := newForwardRegistry()
	defer forwards.stopAll()

	// Set below, once the widgets they touch exist. They are declared up here
	// because starting a forward has to refresh the tree, the forwards table and
	// the footer, and those are all built further down.
	var (
		refreshForwardViews func()
		flash               func(string)
	)

	// ---------------------------------------------------------------- containers
	// A tree: services are parent nodes, their containers are children.
	ctree := tview.NewTreeView()
	croot := tview.NewTreeNode("")
	ctree.SetRoot(croot).SetTopLevel(1) // hide the synthetic root; services are top-level
	// filter holds the active "/" search query; empty means show everything. A
	// candidate matches when the query is a substring of its service, container
	// id or node (case-insensitive). It starts from the optional `ui [service]`
	// argument so `swarmexec ui web` opens pre-narrowed to matching services.
	filter := service
	matchesFilter := func(c resolve.Candidate) bool {
		if filter == "" {
			return true
		}
		q := strings.ToLower(filter)
		return strings.Contains(strings.ToLower(c.Service), q) ||
			strings.Contains(strings.ToLower(c.ContainerID), q) ||
			strings.Contains(strings.ToLower(c.NodeName), q)
	}
	// lastCands / lastSvcs cache the most recent fetch so the "/" filter can
	// re-render locally without hitting the docker API on every keystroke (a
	// remote call over the ssh tunnel — doing it per keystroke makes typing
	// crawl). lastSvcs drives the tree so every service shows, even one with no
	// running task; lastCands supplies the container leaves.
	var (
		lastCands []resolve.Candidate
		lastSvcs  []resolve.Service
	)
	// svcByName / svcCols cache the current services and the column widths so the
	// fold marker (▸/▾) can be rebuilt on a fold — and the docker service ls-style
	// row (mode, replicas, image, ports) stays aligned — without re-rendering the
	// whole tree. A service with no containers gets no marker (nothing to expand),
	// just padding so the rows still line up.
	svcByName := map[string]resolve.Service{}
	svcCols := svcColumns{}
	markService := func(n *tview.TreeNode) {
		ref, ok := n.GetReference().(svcRef)
		if !ok {
			return
		}
		row := serviceRow(svcByName[ref.name], svcCols)
		switch {
		case len(n.GetChildren()) == 0:
			n.SetText("  " + row)
		case n.IsExpanded():
			n.SetText("▾ " + row)
		default:
			n.SetText("▸ " + row)
		}
	}

	renderContainers := func() {
		// Remember the cursor (a leaf by container id, else a service by name)
		// and which services were expanded, so a refresh keeps both.
		prevID, prevSvc := "", ""
		if n := ctree.GetCurrentNode(); n != nil {
			if ref, ok := n.GetReference().(resolve.Candidate); ok {
				prevID = ref.ContainerID
			} else if ref, ok := n.GetReference().(svcRef); ok {
				prevSvc = ref.name
			}
		}
		wasExpanded := map[string]bool{}
		for _, sn := range croot.GetChildren() {
			if ref, ok := sn.GetReference().(svcRef); ok {
				wasExpanded[ref.name] = sn.IsExpanded()
			}
		}
		croot.ClearChildren()

		// Group running containers by service for the leaves.
		byService := map[string][]resolve.Candidate{}
		for _, c := range lastCands {
			byService[c.Service] = append(byService[c.Service], c)
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
		for _, c := range lastCands {
			if w := len(orDash(c.NodeName)); w > nodeW {
				nodeW = w
			}
			if w := len(slotStr(c)); w > slotW {
				slotW = w
			}
		}

		// Column widths for the docker service ls-style service rows, computed
		// over every service so the alignment stays stable while filtering.
		svcCols = svcColumns{}
		svcByName = make(map[string]resolve.Service, len(lastSvcs))
		for _, s := range lastSvcs {
			svcByName[s.Name] = s
			if w := len(orDash(s.Name)); w > svcCols.name {
				svcCols.name = w
			}
			if w := len(orDash(s.Mode)); w > svcCols.mode {
				svcCols.mode = w
			}
			if w := len(fmt.Sprintf("%d/%d", s.Running, s.Desired)); w > svcCols.repl {
				svcCols.repl = w
			}
			if w := len(s.Image); w > svcCols.image {
				svcCols.image = w
			}
		}

		q := strings.ToLower(strings.TrimSpace(filter))
		var firstSvc, targetSvc, targetLeaf *tview.TreeNode
		for _, s := range lastSvcs {
			nameMatch := q == "" || strings.Contains(strings.ToLower(s.Name), q)
			// Which running containers to list: all when the service name matches,
			// otherwise only the containers that match the filter themselves.
			var shown []resolve.Candidate
			for _, c := range byService[s.Name] {
				if nameMatch || matchesFilter(c) {
					shown = append(shown, c)
				}
			}
			// Hide a service only if it neither matches by name nor has any
			// matching container.
			if !nameMatch && len(shown) == 0 {
				continue
			}
			// Collapsed by default (spec); keep a service the operator expanded.
			svcNode := tview.NewTreeNode(serviceRow(s, svcCols)).
				SetColor(serviceColor(s.Running, s.Desired)).
				SetReference(svcRef{name: s.Name}).
				SetExpanded(wasExpanded[s.Name])
			croot.AddChild(svcNode)
			if firstSvc == nil {
				firstSvc = svcNode
			}
			if s.Name == prevSvc {
				targetSvc = svcNode
			}
			for _, c := range shown {
				var label string
				if slotW > 0 {
					label = fmt.Sprintf("%-12s  %-*s  %-*s  up %s", shortID(c.ContainerID), nodeW, orDash(c.NodeName), slotW, slotStr(c), uptime(c.Uptime))
				} else {
					label = fmt.Sprintf("%-12s  %-*s  up %s", shortID(c.ContainerID), nodeW, orDash(c.NodeName), uptime(c.Uptime))
				}
				leaf := tview.NewTreeNode(annotateForwards(label, forwards.forContainer(c.ContainerID))).SetReference(c)
				svcNode.AddChild(leaf)
				if c.ContainerID == prevID {
					targetLeaf = leaf
					svcNode.SetExpanded(true) // reveal the previously-selected leaf
				}
			}
			// Marker depends on the final child count / expanded state, so set it
			// once the leaves are attached.
			markService(svcNode)
		}
		if len(croot.GetChildren()) == 0 {
			empty := "(no services)"
			if q != "" {
				empty = fmt.Sprintf("(no matches for %q)", filter)
			}
			croot.AddChild(tview.NewTreeNode(empty).SetColor(tcell.ColorGray).SetSelectable(false))
		}
		switch {
		case targetLeaf != nil:
			ctree.SetCurrentNode(targetLeaf)
		case targetSvc != nil:
			ctree.SetCurrentNode(targetSvc)
		case firstSvc != nil:
			ctree.SetCurrentNode(firstSvc)
		}
	}
	loadContainers := func() {
		svcs, serr := r.Services(ctx)
		if serr != nil {
			croot.ClearChildren()
			croot.AddChild(tview.NewTreeNode("error: " + serr.Error()).SetColor(tcell.ColorRed).SetSelectable(false))
			return
		}
		// Fetch every running container (not just the CLI-arg service); the tree
		// filters client-side so services with 0 containers still appear.
		cands, lerr := r.Candidates(ctx, "")
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
		lastSvcs = svcs
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

	// startForward brings a forward up off the UI goroutine: dialling the agent
	// can take up to the connect timeout, and blocking the UI for that would
	// freeze the whole app. The entry is registered immediately in the starting
	// state so the operator sees that something is happening.
	startForward := func(c resolve.Candidate, local, remote uint32) {
		fctx, fcancel := context.WithCancel(ctx)
		var once sync.Once
		entry := forwards.add(c, local, remote, func() { once.Do(fcancel) })
		refreshForwardViews()

		go func() {
			ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
			fw, ferr := startForwarder(fctx, cfg, ep, forwardParams{
				address:        "127.0.0.1", // loopback: do not re-expose an internal port to the local network
				localPort:      local,
				remotePort:     remote,
				connectTimeout: f.connectTimeout,
			})
			if ferr != nil {
				forwards.markFailed(entry.id, ferr)
				app.QueueUpdateDraw(func() { refreshForwardViews() })
				return
			}
			addr := fw.LocalAddr().String()
			// Kept as a local: reading entry.localAddr later would race with
			// the registry's own writers.
			boundPort := local
			if _, ps, perr := net.SplitHostPort(addr); perr == nil {
				if n, cerr := strconv.ParseUint(ps, 10, 32); cerr == nil {
					boundPort = uint32(n)
				}
			}
			forwards.markActive(entry.id, addr)
			app.QueueUpdateDraw(func() { refreshForwardViews() })

			serr := fw.Serve(fctx, func(cerr error) {
				// Announce the first failure only: against an outdated agent
				// every connection fails, and flashing each one would hide the
				// footer behind a stutter of identical messages.
				// Only the first failure changes anything visible (the ⚠
				// marker). Redrawing on every one would rebuild the whole
				// container tree per rejected connection — a browser hammering
				// a broken forward would turn that into a redraw storm.
				if !forwards.noteConnError(entry.id, cerr) {
					return
				}
				app.QueueUpdateDraw(func() {
					refreshForwardViews()
					flash(fmt.Sprintf(" [red]forward %d[white]: %v", boundPort, cerr))
				})
			})
			fw.Close()
			// A cancelled forward was stopped on purpose; anything else is a
			// real failure the operator needs to see in the table.
			if serr != nil && fctx.Err() == nil {
				forwards.markFailed(entry.id, serr)
				app.QueueUpdateDraw(func() { refreshForwardViews() })
			}
		}()
	}

	// portPrompt asks which port to forward. There is deliberately no list of
	// exposed ports to pick from: the manager API cannot inspect a container on
	// another node, and the services worth forwarding are exactly the ones that
	// publish nothing — so a suggestion list would be empty where it matters.
	portPrompt := func(c resolve.Candidate) {
		input := tview.NewInputField().SetLabel(" port: ").SetFieldWidth(20)
		input.SetBorder(true).SetTitle(fmt.Sprintf(" forward %s on %s ", orDash(c.Service), orDash(c.NodeName)))
		hint := "  8080  or  9090:8080 (local:remote)"
		input.SetPlaceholder(hint)

		closePrompt := func() { pages.RemovePage("fwdprompt"); app.SetFocus(ctree) }
		input.SetDoneFunc(func(key tcell.Key) {
			if key != tcell.KeyEnter {
				closePrompt()
				return
			}
			local, remote, perr := parsePortSpec(strings.TrimSpace(input.GetText()))
			if perr != nil {
				// Keep the prompt open so the operator can correct the typo
				// instead of retyping the whole thing.
				input.SetTitle(fmt.Sprintf(" %v ", perr))
				return
			}
			closePrompt()
			startForward(c, local, remote)
			flash(fmt.Sprintf(" [green]forwarding[white] localhost:%d → %s:%d", local, shortID(c.ContainerID), remote))
		})
		pages.AddPage("fwdprompt", centered(input, 54, 3), true, true)
		app.SetFocus(input)
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
			list.AddItem("Port forward", "", 0, func() { closeMenu(); portPrompt(c) })
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

		// Height tracks the item count: 5 items plus the border.
		pages.AddPage("menu", centered(list, 48, 7), true, true)
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
			title := trimFoldMarker(node.GetText())
			if ref, ok := node.GetReference().(svcRef); ok {
				title = ref.name // the row now carries mode/image/ports — log by name
			}
			showServiceLogs(title, members)
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

	// ------------------------------------------------------------------ forwards
	ftable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	ftable.SetSelectedStyle(selStyle)
	fHeaders := []string{"LOCAL", "REMOTE", "CONTAINER", "SERVICE", "NODE", "AGE", "STATE"}
	// fRows mirrors the rendered table so a row index maps back to a forward.
	var fRows []forwardEntry
	renderForwards := func() {
		prev, _ := ftable.GetSelection()
		ftable.Clear()
		for i, h := range fHeaders {
			ftable.SetCell(0, i, headerCell(h))
		}
		fRows = forwards.list()
		for i, e := range fRows {
			row := i + 1
			local := "-"
			if p := e.boundPort(); p > 0 {
				local = fmt.Sprintf("127.0.0.1:%d", p)
			}
			state := e.state.String()
			color := tcell.ColorWhite
			switch e.state {
			case forwardActive:
				color = tcell.ColorGreen
				if e.connErr != nil {
					// Listening, but connections are failing — the operator
					// needs to see that, not a reassuring green "active". Kept
					// to a marker because the column truncates; Enter shows the
					// reason in full, and it is flashed once when it happens.
					color = tcell.ColorYellow
					state = "active ⚠"
				}
			case forwardStarting:
				color = tcell.ColorYellow
			case forwardFailed:
				color = tcell.ColorRed
				if e.err != nil {
					// The reason matters more than the word "failed": it is the
					// only place the operator can learn what went wrong.
					state = "failed: " + e.err.Error()
				}
			}
			ftable.SetCell(row, 0, tview.NewTableCell(local))
			ftable.SetCell(row, 1, tview.NewTableCell(fmt.Sprintf("%d", e.remote)))
			ftable.SetCell(row, 2, tview.NewTableCell(shortID(e.cand.ContainerID)))
			ftable.SetCell(row, 3, tview.NewTableCell(orDash(e.cand.Service)))
			ftable.SetCell(row, 4, tview.NewTableCell(orDash(e.cand.NodeName)))
			ftable.SetCell(row, 5, tview.NewTableCell(uptime(time.Since(e.started))))
			ftable.SetCell(row, 6, tview.NewTableCell(state).SetTextColor(color))
		}
		if len(fRows) == 0 {
			ftable.SetCell(1, 0, tview.NewTableCell("(no forwards — press p on a container)").
				SetTextColor(tcell.ColorGray).SetSelectable(false))
			return
		}
		if prev > 0 && prev <= len(fRows) {
			ftable.Select(prev, 0)
		} else {
			ftable.Select(1, 0)
		}
	}
	// selectedForward maps the cursor row back to a forward.
	selectedForward := func() (forwardEntry, bool) {
		row, _ := ftable.GetSelection()
		if row < 1 || row > len(fRows) {
			return forwardEntry{}, false
		}
		return fRows[row-1], true
	}

	// ------------------------------------------------------------------ networks
	nettable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	nettable.SetSelectedStyle(selStyle)
	nHeaders := []string{"NETWORK", "DRIVER", "SCOPE", "TYPE", "SERVICES", "AGE"}
	var nets []swarmNetwork
	renderNetworks := func() {
		selName := ""
		if row, _ := nettable.GetSelection(); row >= 1 {
			if c := nettable.GetCell(row, 0); c != nil {
				selName = c.Text
			}
		}
		nettable.Clear()
		for c, h := range nHeaders {
			nettable.SetCell(0, c, headerCell(h))
		}
		selRow := 1
		for i, n := range nets {
			svcCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if len(n.Services) > 0 {
				svcCell = tview.NewTableCell(fmt.Sprintf("%d", len(n.Services))).SetTextColor(tcell.ColorGreen).SetExpansion(1)
			}
			nettable.SetCell(i+1, 0, tview.NewTableCell(n.Name).SetExpansion(1))
			nettable.SetCell(i+1, 1, tview.NewTableCell(orDash(n.Driver)).SetExpansion(1))
			nettable.SetCell(i+1, 2, tview.NewTableCell(orDash(n.Scope)).SetExpansion(1))
			nettable.SetCell(i+1, 3, tview.NewTableCell(networkType(n)).SetExpansion(1))
			nettable.SetCell(i+1, 4, svcCell)
			nettable.SetCell(i+1, 5, tview.NewTableCell(volumeAge(n.Created)).SetExpansion(1))
			if n.Name == selName {
				selRow = i + 1
			}
		}
		if len(nets) > 0 {
			nettable.Select(selRow, 0)
		}
	}
	loadNetworks := func() {
		nettable.Clear()
		for c, h := range nHeaders {
			nettable.SetCell(0, c, headerCell(h))
		}
		nettable.SetCell(1, 0, tview.NewTableCell("loading…").SetTextColor(tcell.ColorGray))
		go func() {
			list, err := listNetworks(ctx, dcli)
			app.QueueUpdateDraw(func() {
				if err != nil {
					nettable.Clear()
					for c, h := range nHeaders {
						nettable.SetCell(0, c, headerCell(h))
					}
					nettable.SetCell(1, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
					return
				}
				nets = list
				renderNetworks()
			})
		}()
	}
	selectedNetwork := func() (swarmNetwork, bool) {
		row, _ := nettable.GetSelection()
		i := row - 1
		if i < 0 || i >= len(nets) {
			return swarmNetwork{}, false
		}
		return nets[i], true
	}
	// showNetworkMembers lists the services attached to a network with the
	// containers of each service nested under it. Service membership is known
	// synchronously (from the list); the per-service containers need a task
	// lookup, so they fill in lazily after the overlay is up.
	showNetworkMembers := func(n swarmNetwork) {
		list := tview.NewList().ShowSecondaryText(false)
		list.SetBorder(true).SetTitle(fmt.Sprintf(" %s — attached services — ESC back ", n.Name))
		render := func(svcs []netService, loading bool) {
			cur := list.GetCurrentItem()
			list.Clear()
			if len(svcs) == 0 {
				if loading {
					list.AddItem("loading…", "", 0, nil)
				} else {
					list.AddItem("(no services attached)", "", 0, nil)
				}
			}
			for _, s := range svcs {
				head := s.Name + " …"
				if !loading {
					head = fmt.Sprintf("%s (%d)", s.Name, len(s.Containers))
				}
				list.AddItem(head, "", 0, nil)
				for _, c := range s.Containers {
					list.AddItem(fmt.Sprintf("    %s  %s  %s", c.ID, orDash(c.Node), orDash(c.IPv4)), "", 0, nil)
				}
			}
			if cur < list.GetItemCount() {
				list.SetCurrentItem(cur)
			}
		}
		// Seed with the services already known from the list; containers pending.
		init := make([]netService, 0, len(n.Services))
		for _, s := range n.Services {
			init = append(init, netService{Name: s})
		}
		render(init, true)
		closeMembers := func() { pages.RemovePage("netmembers"); app.SetFocus(nettable) }
		list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')) {
				closeMembers()
				return nil
			}
			return vimListKeys(ev)
		})
		height := len(n.Services) + 6
		if height > 22 {
			height = 22
		}
		pages.AddPage("netmembers", centered(list, 78, height), true, true)
		app.SetFocus(list)
		go func() {
			members := networkMembers(ctx, dcli, n)
			app.QueueUpdateDraw(func() {
				// Only repaint if this overlay is still the one on screen.
				if pages.HasPage("netmembers") {
					render(members, false)
				}
			})
		}()
	}
	nettable.SetSelectedFunc(func(int, int) {
		if n, ok := selectedNetwork(); ok {
			showNetworkMembers(n)
		}
	})

	// ------------------------------------------------------------------- secrets
	sectable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	sectable.SetSelectedStyle(selStyle)
	sHeaders := []string{"SECRET", "USED BY", "AGE", "UPDATED", "LABELS"}
	var secs []swarmSecret
	renderSecrets := func() {
		selName := ""
		if row, _ := sectable.GetSelection(); row >= 1 {
			if c := sectable.GetCell(row, 0); c != nil {
				selName = c.Text
			}
		}
		sectable.Clear()
		for c, h := range sHeaders {
			sectable.SetCell(0, c, headerCell(h))
		}
		selRow := 1
		for i, s := range secs {
			usedCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if len(s.Services) > 0 {
				usedCell = tview.NewTableCell(fmt.Sprintf("%d", len(s.Services))).SetTextColor(tcell.ColorGreen).SetExpansion(1)
			}
			labels := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if len(s.Labels) > 0 {
				labels = tview.NewTableCell(fmt.Sprintf("%d", len(s.Labels))).SetExpansion(1)
			}
			sectable.SetCell(i+1, 0, tview.NewTableCell(s.Name).SetExpansion(1))
			sectable.SetCell(i+1, 1, usedCell)
			sectable.SetCell(i+1, 2, tview.NewTableCell(volumeAge(s.Created)).SetExpansion(1))
			sectable.SetCell(i+1, 3, tview.NewTableCell(volumeAge(s.Updated)).SetExpansion(1))
			sectable.SetCell(i+1, 4, labels)
			if s.Name == selName {
				selRow = i + 1
			}
		}
		if len(secs) > 0 {
			sectable.Select(selRow, 0)
		}
	}
	loadSecrets := func() {
		sectable.Clear()
		for c, h := range sHeaders {
			sectable.SetCell(0, c, headerCell(h))
		}
		sectable.SetCell(1, 0, tview.NewTableCell("loading…").SetTextColor(tcell.ColorGray))
		go func() {
			list, err := listSecrets(ctx, dcli)
			app.QueueUpdateDraw(func() {
				if err != nil {
					sectable.Clear()
					for c, h := range sHeaders {
						sectable.SetCell(0, c, headerCell(h))
					}
					sectable.SetCell(1, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
					return
				}
				secs = list
				renderSecrets()
			})
		}()
	}
	selectedSecret := func() (swarmSecret, bool) {
		row, _ := sectable.GetSelection()
		i := row - 1
		if i < 0 || i >= len(secs) {
			return swarmSecret{}, false
		}
		return secs[i], true
	}
	// showSecretDetail shows a secret's metadata and the services/containers that
	// use it. The value is deliberately absent: the Docker API never returns it,
	// and the overlay says so. Usage (services with their running containers)
	// needs a task lookup, so it fills in lazily after the overlay is up.
	showSecretDetail := func(s swarmSecret) {
		ts := func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		}
		tv := tview.NewTextView().SetDynamicColors(true)
		tv.SetBorder(true).SetTitle(fmt.Sprintf(" secret %s — ESC back ", s.Name))
		render := func(members []netService, loading bool) {
			var b strings.Builder
			fmt.Fprintf(&b, "Name:     %s\n", s.Name)
			fmt.Fprintf(&b, "ID:       %s\n", s.ID)
			fmt.Fprintf(&b, "Created:  %s\n", ts(s.Created))
			fmt.Fprintf(&b, "Updated:  %s\n", ts(s.Updated))
			if len(s.Labels) == 0 {
				fmt.Fprintf(&b, "Labels:   -\n")
			} else {
				fmt.Fprintf(&b, "Labels:\n")
				keys := make([]string, 0, len(s.Labels))
				for k := range s.Labels {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					fmt.Fprintf(&b, "  %s=%s\n", k, s.Labels[k])
				}
			}
			fmt.Fprintf(&b, "\nused by:\n")
			if len(members) == 0 {
				if loading {
					fmt.Fprintf(&b, "  …\n")
				} else {
					fmt.Fprintf(&b, "  (no services)\n")
				}
			}
			for _, m := range members {
				head := m.Name + " …"
				if !loading {
					head = fmt.Sprintf("%s (%d)", m.Name, len(m.Containers))
				}
				fmt.Fprintf(&b, "  %s\n", head)
				for _, c := range m.Containers {
					fmt.Fprintf(&b, "      %s  %s\n", c.ID, orDash(c.Node))
				}
			}
			fmt.Fprintf(&b, "\n[gray]the secret value is not retrievable via the Docker API[white]")
			tv.SetText(b.String())
		}
		// Seed with the service names already known from the list; containers pending.
		init := make([]netService, 0, len(s.Services))
		for _, name := range s.Services {
			init = append(init, netService{Name: name})
		}
		render(init, true)
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')):
				pages.RemovePage("secdetail")
				app.SetFocus(sectable)
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
				return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
				return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
			}
			return ev
		})
		height := 11 + len(s.Labels) + len(s.Services)
		if height > 24 {
			height = 24
		}
		pages.AddPage("secdetail", centered(tv, 72, height), true, true)
		app.SetFocus(tv)
		go func() {
			members := secretMembers(ctx, dcli, s)
			app.QueueUpdateDraw(func() {
				if pages.HasPage("secdetail") {
					render(members, false)
				}
			})
		}()
	}
	sectable.SetSelectedFunc(func(int, int) {
		if s, ok := selectedSecret(); ok {
			showSecretDetail(s)
		}
	})

	// ------------------------------------------------------------------ contexts
	// activeCtx is this session's effective docker context (what the UI is
	// connected to): the context the session was started with, else
	// $DOCKER_CONTEXT, else the stored current, else "default". Shown in the
	// footer and marked here.
	activeCtx := firstNonEmpty(ctxOverride, os.Getenv("DOCKER_CONTEXT"))
	if activeCtx == "" {
		activeCtx = dockerctx.Current()
	}
	if activeCtx == "" {
		activeCtx = "default"
	}
	cxtable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	cxtable.SetSelectedStyle(selStyle)
	cxHeaders := []string{"CONTEXT", "DOCKER HOST"}
	var ctxs []dockerctx.Context
	renderContexts := func() {
		selName := ""
		if row, _ := cxtable.GetSelection(); row >= 1 {
			if c := cxtable.GetCell(row, 0); c != nil {
				selName = strings.TrimLeft(c.Text, "▶ ")
			}
		}
		cxtable.Clear()
		for c, h := range cxHeaders {
			cxtable.SetCell(0, c, headerCell(h))
		}
		selRow := 1
		for i, c := range ctxs {
			label, color := "  "+c.Name, tcell.ColorWhite
			if c.Name == activeCtx {
				label, color = "▶ "+c.Name, tcell.ColorAqua // the active session context
			}
			cxtable.SetCell(i+1, 0, tview.NewTableCell(label).SetTextColor(color).SetExpansion(1))
			cxtable.SetCell(i+1, 1, tview.NewTableCell(orDash(c.Host)).SetTextColor(tcell.ColorGray).SetExpansion(2))
			if c.Name == selName {
				selRow = i + 1
			}
		}
		if len(ctxs) > 0 {
			cxtable.Select(selRow, 0)
		}
	}
	// Contexts come from docker's local store (no network), so load synchronously.
	loadContexts := func() {
		list, err := dockerctx.List()
		if err != nil {
			cxtable.Clear()
			for c, h := range cxHeaders {
				cxtable.SetCell(0, c, headerCell(h))
			}
			cxtable.SetCell(1, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
			return
		}
		ctxs = list
		renderContexts()
	}
	selectedContext := func() (dockerctx.Context, bool) {
		row, _ := cxtable.GetSelection()
		i := row - 1
		if i < 0 || i >= len(ctxs) {
			return dockerctx.Context{}, false
		}
		return ctxs[i], true
	}
	// showCreateContext opens a form to add a docker context (name + endpoint).
	showCreateContext := func() {
		var name, host, desc string
		form := tview.NewForm()
		form.SetBorder(true).SetTitle(" new context — Esc cancels ")
		form.AddInputField("Name", "", 32, nil, func(t string) { name = t })
		form.AddInputField("Docker host", "", 44, nil, func(t string) { host = t })
		form.AddInputField("Description", "", 44, nil, func(t string) { desc = t })
		if hf, ok := form.GetFormItem(1).(*tview.InputField); ok {
			hf.SetPlaceholder("ssh://ops@manager  |  tcp://host:2376")
		}
		closeForm := func() { pages.RemovePage("ctxform"); app.SetFocus(cxtable) }
		form.AddButton("Create", func() {
			if err := dockerctx.Create(strings.TrimSpace(name), strings.TrimSpace(host), strings.TrimSpace(desc)); err != nil {
				info("create failed: " + err.Error())
				return
			}
			closeForm()
			loadContexts()
			flash(" [green]created[white] context " + strings.TrimSpace(name))
		})
		form.AddButton("Cancel", closeForm)
		form.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape {
				closeForm()
				return nil
			}
			return ev
		})
		pages.AddPage("ctxform", centered(form, 66, 13), true, true)
		app.SetFocus(form)
	}
	// deleteContext removes the selected context behind a confirm. "default" is
	// protected; removing the current one resets the selection to default.
	deleteContext := func(c dockerctx.Context) {
		if c.Name == "default" {
			info("the built-in \"default\" context cannot be removed")
			return
		}
		force := c.Name == dockerctx.Current()
		msg := fmt.Sprintf("Remove context %q?\n%s", c.Name, orDash(c.Host))
		if force {
			msg += "\n\nIt is the current context — its selection resets to \"default\"."
		}
		m := tview.NewModal().SetText(msg).AddButtons([]string{"Delete", "Cancel"}).
			SetDoneFunc(func(_ int, label string) {
				pages.RemovePage("confirm")
				if label != "Delete" {
					app.SetFocus(cxtable)
					return
				}
				if err := dockerctx.Remove(c.Name, force); err != nil {
					info("remove failed: " + err.Error())
					return
				}
				loadContexts()
				flash(" [green]removed[white] context " + c.Name)
			})
		pages.AddPage("confirm", m, true, true)
		app.SetFocus(m)
	}
	// activateContext makes c the current docker context and restarts the UI so
	// it reconnects to that cluster. Restarting (rather than swapping the client
	// live) avoids racing the in-flight background loads.
	activateContext := func(c dockerctx.Context) {
		if c.Name == activeCtx {
			flash(" [gray]already on[white] context " + c.Name)
			return
		}
		if err := dockerctx.Use(c.Name); err != nil {
			info("switch failed: " + err.Error())
			return
		}
		switchTo = c.Name
		app.Stop() // the caller restarts against switchTo
	}

	// ---------------------------------------------------------------- tabs/chrome
	content.AddPage("containers", ctree, true, true)
	content.AddPage("volumes", vtable, true, false)
	content.AddPage("forwards", ftable, true, false)
	content.AddPage("networks", nettable, true, false)
	content.AddPage("secrets", sectable, true, false)
	content.AddPage("contexts", cxtable, true, false)

	tabBar := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignCenter)
	help := tview.NewTextView().SetDynamicColors(true)
	// Right side of the footer: a live cluster summary (ready nodes / reachable
	// agents), filled in asynchronously so probing the agents never blocks the UI.
	cluster := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight)
	cluster.SetText("[gray]cluster: …[white] ")
	// Forward count sits next to the cluster summary, the one footer slot built
	// for asynchronously updated state. flash() cannot carry it: it self-clears
	// after 1.5s, and a forward is persistent.
	fwdCount := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight)
	// Active docker context (computed with the Contexts tab) — always shown so
	// it's clear which cluster the UI is talking to.
	ctxView := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight)
	ctxView.SetText(fmt.Sprintf("[aqua]ctx[white] %s ", activeCtx))
	ctxW := len(activeCtx) + 6
	if ctxW < 12 {
		ctxW = 12
	} else if ctxW > 30 {
		ctxW = 30
	}
	footer := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(help, 0, 1, false).
		AddItem(fwdCount, 12, 0, false).
		AddItem(ctxView, ctxW, 0, false).
		AddItem(cluster, 30, 0, false)
	// "/" search bar: hidden (height 0) until activated; filters the container
	// tree live by service / container id / node.
	search := tview.NewInputField().SetLabel("/ ").SetFieldWidth(0).
		SetPlaceholder("filter services / containers / nodes")
	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tabBar, 1, 0, false).
		AddItem(content, 0, 1, true).
		AddItem(search, 0, 0, false).
		AddItem(footer, 1, 0, false)
	pages.AddPage("main", root, true, true)

	// savedHelp holds the footer help to restore when search closes. While the
	// search field has focus the footer shows how to leave it — there is no
	// other on-screen hint, which is what made "how do I exit search?" a real
	// snag. (Not curHelp: that is declared further down, out of scope here.)
	var savedHelp string
	startSearch := func() {
		root.ResizeItem(search, 1, 0)
		savedHelp = help.GetText(false)
		help.SetText(" [yellow]type[white] to filter   [yellow]Enter[white] keep filter & exit   [yellow]Esc[white] clear & exit")
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
		help.SetText(savedHelp) // restore the tab help
		app.SetFocus(ctree)     // Enter keeps the filter; the bar stays as an indicator
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

	// refreshForwardViews repaints everything a forward's state feeds: the
	// footer counter, the forwards table, and the tree annotations. Must run on
	// the UI goroutine.
	refreshForwardViews = func() {
		total, act := forwards.counts()
		switch {
		case total == 0:
			fwdCount.SetText("")
		case act == total:
			fwdCount.SetText(fmt.Sprintf("[aqua]%d[white] fwd ", total))
		default:
			fwdCount.SetText(fmt.Sprintf("[aqua]%d[white]/%d fwd ", act, total))
		}
		renderForwards()
		renderContainers()
	}

	active := "containers"
	mouseEnabled := true
	var screen tcell.Screen // set just before Run; used for clipboard (OSC52)
	curHelp := ""
	helpFor := func(name string) string {
		switch name {
		case "containers":
			return " [yellow]j/k[white] up/down  [yellow]h/l[white] fold  [yellow]/[white] search  [yellow]Enter[white] menu  [yellow]p[white] forward  [yellow]y[white] copy  [yellow]m[white] mouse  [yellow]Tab/1-6[white] tabs  [yellow]r[white] refresh  [yellow]q[white] quit"
		case "volumes":
			return " [yellow]j/k[white] up/down  [yellow]Enter[white] nodes  [yellow]i[white] used by  [yellow]s/S[white] sort/reverse  [yellow]y[white] copy  [yellow]m[white] mouse  [yellow]Tab/1-6[white] tabs  [yellow]r[white] refresh  [yellow]q[white] quit"
		case "networks":
			return " [yellow]j/k[white] up/down  [yellow]Enter/i[white] attached  [yellow]y[white] copy  [yellow]m[white] mouse  [yellow]Tab/1-6[white] tabs  [yellow]r[white] refresh  [yellow]q[white] quit"
		case "secrets":
			return " [yellow]j/k[white] up/down  [yellow]Enter[white] details  [yellow]y[white] copy  [yellow]m[white] mouse  [yellow]Tab/1-6[white] tabs  [yellow]r[white] refresh  [yellow]q[white] quit"
		case "contexts":
			return " [yellow]j/k[white] up/down  [yellow]u[white] use  [yellow]n[white] new  [yellow]d[white] delete  [yellow]y[white] copy  [yellow]m[white] mouse  [yellow]Tab/1-6[white] tabs  [yellow]r[white] refresh  [yellow]q[white] quit"
		default:
			return " [yellow]j/k[white] up/down  [yellow]Enter[white] details  [yellow]d[white] stop  [yellow]o[white] copy url  [yellow]m[white] mouse  [yellow]Tab/1-6[white] tabs  [yellow]r[white] refresh  [yellow]q[white] quit"
		}
	}
	// tabChrome renders the tab bar with one tab highlighted, so adding a tab is
	// a single list entry instead of five hand-aligned strings.
	tabList := []struct{ key, label string }{
		{"containers", "Containers (1)"},
		{"volumes", "Volumes (2)"},
		{"forwards", "Forwards (3)"},
		{"networks", "Networks (4)"},
		{"secrets", "Secrets (5)"},
		{"contexts", "Contexts (6)"},
	}
	renderTabBar := func(active string) {
		var b strings.Builder
		for _, t := range tabList {
			if t.key == active {
				fmt.Fprintf(&b, " [black:teal] %s [-:-]  ", t.label)
			} else {
				fmt.Fprintf(&b, " %s  ", t.label)
			}
		}
		tabBar.SetText(b.String())
	}
	setTab := func(name string) {
		active = name
		content.SwitchToPage(name)
		curHelp = helpFor(name)
		help.SetText(curHelp)
		renderTabBar(name)
		switch name {
		case "containers":
			app.SetFocus(ctree)
		case "volumes":
			app.SetFocus(vtable)
			loadVolumes()
		case "forwards":
			app.SetFocus(ftable)
			renderForwards()
		case "networks":
			app.SetFocus(nettable)
			loadNetworks()
		case "secrets":
			app.SetFocus(sectable)
			loadSecrets()
		case "contexts":
			app.SetFocus(cxtable)
			loadContexts()
		}
	}

	// flash briefly replaces the footer with a status message, then restores it.
	flash = func(msg string) {
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
		switch active {
		case "containers":
			for _, svc := range croot.GetChildren() {
				fmt.Fprintln(&b, trimFoldMarker(svc.GetText()))
				for _, c := range svc.GetChildren() {
					fmt.Fprintf(&b, "  %s\n", c.GetText())
				}
			}
		case "forwards":
			fmt.Fprintln(&b, "LOCAL\tREMOTE\tCONTAINER\tSERVICE\tNODE\tSTATE")
			for _, e := range forwards.list() {
				local := "-"
				if p := e.boundPort(); p > 0 {
					local = fmt.Sprintf("127.0.0.1:%d", p)
				}
				fmt.Fprintf(&b, "%s\t%d\t%s\t%s\t%s\t%s\n",
					local, e.remote, shortID(e.cand.ContainerID),
					orDash(e.cand.Service), orDash(e.cand.NodeName), e.state)
			}
		case "networks":
			fmt.Fprintln(&b, "NETWORK\tDRIVER\tSCOPE\tTYPE\tSERVICES\tAGE")
			for _, n := range nets {
				fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%d\t%s\n",
					n.Name, orDash(n.Driver), orDash(n.Scope), networkType(n), len(n.Services), volumeAge(n.Created))
			}
		case "secrets":
			fmt.Fprintln(&b, "SECRET\tUSED BY\tAGE\tUPDATED\tLABELS")
			for _, s := range secs {
				fmt.Fprintf(&b, "%s\t%d\t%s\t%s\t%d\n", s.Name, len(s.Services), volumeAge(s.Created), volumeAge(s.Updated), len(s.Labels))
			}
		case "contexts":
			fmt.Fprintln(&b, "CONTEXT\tDOCKER HOST\tACTIVE")
			for _, c := range ctxs {
				mark := ""
				if c.Name == activeCtx {
					mark = "*"
				}
				fmt.Fprintf(&b, "%s\t%s\t%s\n", c.Name, orDash(c.Host), mark)
			}
		default:
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

	// tabOrder drives Tab cycling; every tab joins it.
	tabOrder := []string{"containers", "volumes", "forwards", "networks", "secrets", "contexts"}
	tabKeys := func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyTab {
			for i, name := range tabOrder {
				if name == active {
					setTab(tabOrder[(i+1)%len(tabOrder)])
					break
				}
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
			case '3':
				setTab("forwards")
				return nil
			case '4':
				setTab("networks")
				return nil
			case '5':
				setTab("secrets")
				return nil
			case '6':
				setTab("contexts")
				return nil
			case 'q':
				app.Stop()
				return nil
			case 'r':
				switch active {
				case "containers":
					loadContainers()
				case "volumes":
					loadVolumes()
				case "networks":
					loadNetworks()
				case "secrets":
					loadSecrets()
				case "contexts":
					loadContexts()
				default:
					renderForwards()
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
	// On the tree, "/" opens search; h/l collapse/expand the service under the
	// cursor; j/k stay down/up via the shared keys.
	ctree.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case '/':
				startSearch()
				return nil
			case 'h':
				// Collapse. tview's TreeView has no fold key — Left/Right only
				// move the cursor — so fold explicitly. On a container leaf,
				// step out to its service (press h again to fold it).
				if n := ctree.GetCurrentNode(); n != nil {
					if isServiceNode(n) {
						n.SetExpanded(false)
						markService(n)
					} else if p := serviceParent(croot, n); p != nil {
						ctree.SetCurrentNode(p)
					}
				}
				return nil
			case 'l':
				// Expand the service under the cursor; if it is already open,
				// descend to its first container.
				if n := ctree.GetCurrentNode(); n != nil && isServiceNode(n) {
					if n.IsExpanded() && len(n.GetChildren()) > 0 {
						ctree.SetCurrentNode(n.GetChildren()[0])
					} else {
						n.SetExpanded(true)
						markService(n)
					}
				}
				return nil
			case 'p':
				// On a service node, forward to the task under the cursor —
				// exactly one, like kubectl does with a pod. Forwarding "the
				// service" would have to load-balance, which makes debugging
				// misleading.
				if n := ctree.GetCurrentNode(); n != nil {
					if c, ok := n.GetReference().(resolve.Candidate); ok {
						portPrompt(c)
					} else if kids := n.GetChildren(); len(kids) > 0 {
						if c, ok := kids[0].GetReference().(resolve.Candidate); ok {
							portPrompt(c)
						}
					}
				}
				return nil
			}
		}
		return tabKeys(ev)
	})
	// Enter shows the full detail of a forward. The table truncates the state
	// column, so this is where a failure reason is actually readable.
	ftable.SetSelectedFunc(func(int, int) {
		row, ok := selectedForward()
		if !ok {
			return
		}
		// Re-read from the registry: the rendered row's connErr is only as
		// fresh as the last redraw, and redraws are deliberately rare.
		e, ok := forwards.get(row.id)
		if !ok {
			return
		}
		// A left-aligned TextView, not the info() modal: tview.Modal centers
		// each line on its own, which shears a padded key/value block out of
		// alignment. Values are escaped because dynamic colors are on and an
		// error string can contain "[".
		esc := tview.Escape
		var b strings.Builder
		fmt.Fprintf(&b, "local:      127.0.0.1:%d\n", e.boundPort())
		fmt.Fprintf(&b, "remote:     %d\n", e.remote)
		fmt.Fprintf(&b, "container:  %s\n", esc(shortID(e.cand.ContainerID)))
		fmt.Fprintf(&b, "service:    %s\n", esc(orDash(e.cand.Service)))
		fmt.Fprintf(&b, "node:       %s\n", esc(orDash(e.cand.NodeName)))
		fmt.Fprintf(&b, "state:      %s", esc(e.state.String()))
		if e.err != nil {
			fmt.Fprintf(&b, "\n\n[red]failed:[-] %s", esc(e.err.Error()))
		}
		if e.connErr != nil {
			fmt.Fprintf(&b, "\n\n[yellow]last connection failed:[-]\n%s", esc(e.connErr.Error()))
		}
		b.WriteString("\n\n[gray]d[-] stop   [gray]Esc[-] close")

		tv := tview.NewTextView().SetDynamicColors(true).SetText(b.String())
		tv.SetBorder(true).SetTitle(fmt.Sprintf(" forward #%d ", e.id))
		closeDetail := func() { pages.RemovePage("fwddetail"); app.SetFocus(ftable) }
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape, ev.Key() == tcell.KeyEnter:
				closeDetail()
				return nil
			case ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i'):
				closeDetail()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
				// Stop straight from the detail view — the operator is already
				// looking at what they are about to kill.
				forwards.remove(e.id)
				closeDetail()
				refreshForwardViews()
				flash(fmt.Sprintf(" [green]stopped[white] forward to %s:%d", shortID(e.cand.ContainerID), e.remote))
				return nil
			}
			return ev
		})
		// Height tracks the content so a short forward gets a snug box and a
		// failed one grows to fit its reason.
		lines := strings.Count(b.String(), "\n") + 1
		pages.AddPage("fwddetail", centered(tv, 66, lines+2), true, true)
		app.SetFocus(tv)
	})
	// On the forwards table: d stops the selected forward, o copies its URL.
	ftable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case 'd':
				if e, ok := selectedForward(); ok {
					forwards.remove(e.id)
					refreshForwardViews()
					flash(fmt.Sprintf(" [green]stopped[white] forward to %s:%d", shortID(e.cand.ContainerID), e.remote))
				}
				return nil
			case 'o':
				// Copy rather than launch a browser: the UI often runs over
				// ssh, where opening a local browser would target the wrong
				// machine — and the forward is bound on the operator's side.
				if e, ok := selectedForward(); ok && e.state == forwardActive {
					url := fmt.Sprintf("http://127.0.0.1:%d", e.boundPort())
					if screen != nil {
						screen.SetClipboard([]byte(url))
					}
					flash(" [green]copied[white] " + url)
				}
				return nil
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
	// On the networks table, "i" (like the volumes tab) shows the attached
	// services/containers; Enter does the same.
	nettable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune && ev.Rune() == 'i' {
			if n, ok := selectedNetwork(); ok {
				showNetworkMembers(n)
			}
			return nil
		}
		return tabKeys(ev)
	})
	sectable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		return tabKeys(ev)
	})
	// On the contexts table: "n" creates, "d" removes, "u" (or Enter) activates.
	cxtable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case 'n':
				showCreateContext()
				return nil
			case 'd':
				if c, ok := selectedContext(); ok {
					deleteContext(c)
				}
				return nil
			case 'u':
				if c, ok := selectedContext(); ok {
					activateContext(c)
				}
				return nil
			}
		}
		return tabKeys(ev)
	})
	cxtable.SetSelectedFunc(func(int, int) {
		if c, ok := selectedContext(); ok {
			activateContext(c)
		}
	})

	loadContainers()
	setTab("containers")
	refreshCluster()

	// Own the screen so we can post to the system clipboard (OSC52) on yank.
	scr, serr := tcell.NewScreen()
	if serr != nil {
		return "", &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: init screen: %w", serr)}
	}
	screen = scr
	app.SetScreen(screen)

	if err := app.SetRoot(pages, true).EnableMouse(true).Run(); err != nil {
		return "", &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: %w", err)}
	}
	return switchTo, nil
}

// svcRef marks a service (group) node and carries its name. It is deliberately
// NOT a resolve.Candidate, so isServiceNode (and every other
// GetReference().(resolve.Candidate) check) still treats it as a group node; it
// only lets a re-render restore the cursor and expansion state by service name.
type svcRef struct{ name string }

// isServiceNode reports whether a tree node is a service (group) node rather
// than a container leaf (containers carry a resolve.Candidate reference).
func isServiceNode(n *tview.TreeNode) bool {
	_, ok := n.GetReference().(resolve.Candidate)
	return !ok
}

// trimFoldMarker strips the leading ▸/▾ fold marker (or its blank padding) from
// a service node's text, recovering the plain "name  running/desired" label.
func trimFoldMarker(s string) string {
	return strings.TrimLeft(s, "▸▾ ")
}

// networkType summarizes a network's role for the TYPE column. The flags are
// mutually meaningful but rarely combine, so the most operationally salient one
// wins: ingress (routing mesh) over internal (no egress) over attachable.
func networkType(n swarmNetwork) string {
	switch {
	case n.Ingress:
		return "ingress"
	case n.Internal:
		return "internal"
	case n.Attachable:
		return "attachable"
	default:
		return "-"
	}
}

// serviceParent returns the service node that owns leaf, or nil. tview.TreeNode
// exposes no parent pointer, so we scan the (shallow, two-level) tree.
func serviceParent(root, leaf *tview.TreeNode) *tview.TreeNode {
	for _, svc := range root.GetChildren() {
		for _, c := range svc.GetChildren() {
			if c == leaf {
				return svc
			}
		}
	}
	return nil
}

// serviceColor maps a service's running/desired task counts to a health color
// for the containers tree: grey when scaled to zero (0/0), red when down (0/n),
// orange when partial (e.g. 1/3), aqua when healthy (n/n).
func serviceColor(running, desired int) tcell.Color {
	switch {
	case desired == 0:
		return tcell.ColorGray
	case running == 0:
		return tcell.ColorRed
	case running != desired:
		return tcell.ColorOrange
	default:
		return tcell.ColorAqua
	}
}

// svcColumns holds the padding widths that align the service rows in the
// containers tree, computed once per render across all services.
type svcColumns struct {
	name, mode, repl, image int
}

// serviceRow renders a service group node like a docker service ls line —
// "name  mode  running/desired  image  ports" — padded to the shared column
// widths. Image and ports are appended only when present, and trailing padding
// is trimmed so a selected row's highlight does not run past the text.
func serviceRow(s resolve.Service, c svcColumns) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-*s  %-*s",
		c.name, orDash(s.Name), c.mode, orDash(s.Mode), c.repl, fmt.Sprintf("%d/%d", s.Running, s.Desired))
	if c.image > 0 {
		fmt.Fprintf(&b, "  %-*s", c.image, orDash(s.Image))
	}
	if s.Ports != "" {
		fmt.Fprintf(&b, "  %s", s.Ports)
	}
	return strings.TrimRight(b.String(), " ")
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
