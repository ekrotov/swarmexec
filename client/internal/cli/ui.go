package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
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
	loadContainers := func() {
		// Remember the selected container so a refresh keeps the cursor on it.
		prevID := ""
		if n := ctree.GetCurrentNode(); n != nil {
			if ref, ok := n.GetReference().(resolve.Candidate); ok {
				prevID = ref.ContainerID
			}
		}
		cands, lerr := r.Candidates(ctx, service)
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].Service != cands[j].Service {
				return cands[i].Service < cands[j].Service
			}
			if cands[i].Slot != cands[j].Slot {
				return cands[i].Slot < cands[j].Slot
			}
			return cands[i].NodeName < cands[j].NodeName
		})
		croot.ClearChildren()
		if lerr != nil {
			croot.AddChild(tview.NewTreeNode("error: " + lerr.Error()).SetColor(tcell.ColorRed).SetSelectable(false))
			return
		}
		var first, target, svcNode *tview.TreeNode
		curService := ""
		for _, c := range cands {
			if svcNode == nil || c.Service != curService {
				curService = c.Service
				svcNode = tview.NewTreeNode(orDash(c.Service)).SetColor(tcell.ColorAqua).SetExpanded(true)
				croot.AddChild(svcNode)
			}
			slotPart := ""
			if c.Slot > 0 {
				slotPart = fmt.Sprintf("slot %d  ", c.Slot)
			}
			label := fmt.Sprintf("%s  %s  %sup %s", shortID(c.ContainerID), orDash(c.NodeName), slotPart, uptime(c.Uptime))
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
			croot.AddChild(tview.NewTreeNode("(no running tasks)").SetColor(tcell.ColorGray).SetSelectable(false))
		}
		switch {
		case target != nil:
			ctree.SetCurrentNode(target)
		case first != nil:
			ctree.SetCurrentNode(first)
		}
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
			return ev
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
		} else {
			node.SetExpanded(!node.IsExpanded()) // toggle a service group
		}
	})

	// ------------------------------------------------------------------- volumes
	vtable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	vtable.SetSelectedStyle(selStyle)
	vHeaders := []string{"VOLUME", "DRIVER", "NODES"}
	var vols []swarmVolume
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
			noAgent := false
			if nerr == nil {
				vs, errs = indexVolumes(ctx, cfg, nodes, f.connectTimeout)
				if len(nodes) > 0 && len(errs) == len(nodes) && !agentDeployed(ctx, dcli) {
					noAgent = true
				}
			}
			app.QueueUpdateDraw(func() {
				vols = vs
				vtable.Clear()
				for c, h := range vHeaders {
					vtable.SetCell(0, c, headerCell(h))
				}
				if nerr != nil {
					vtable.SetCell(1, 0, tview.NewTableCell("error: "+nerr.Error()).SetTextColor(tcell.ColorRed))
					return
				}
				if noAgent {
					vtable.SetCell(1, 0, tview.NewTableCell(errNoAgent.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
					return
				}
				for i, v := range vols {
					vals := []string{v.Name, orDash(v.Driver), fmt.Sprintf("%d: %s", len(v.Nodes), joinNodes(v.Nodes))}
					for col, val := range vals {
						vtable.SetCell(i+1, col, tview.NewTableCell(val).SetExpansion(1))
					}
				}
				if len(vols) > 0 {
					vtable.Select(1, 0)
				}
				if len(errs) > 0 {
					vtable.SetCell(len(vols)+1, 0, tview.NewTableCell(fmt.Sprintf("(%d node(s) unreachable)", len(errs))).SetTextColor(tcell.ColorYellow).SetSelectable(false))
				}
			})
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
		list.SetBorder(true).SetTitle(fmt.Sprintf(" %s on %d node(s) — space select · d delete · a all · ESC back ", v.Name, len(v.Nodes)))
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
			return ev
		})
		pages.AddPage("volnodes", centered(list, 56, len(v.Nodes)+4), true, true)
		app.SetFocus(list)
	}
	vtable.SetSelectedFunc(func(int, int) {
		if v, ok := selectedVolume(); ok && len(v.Nodes) > 0 {
			showVolumeNodes(v)
		}
	})

	// ---------------------------------------------------------------- tabs/chrome
	content.AddPage("containers", ctree, true, true)
	content.AddPage("volumes", vtable, true, false)

	tabBar := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignCenter)
	help := tview.NewTextView().SetDynamicColors(true)
	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tabBar, 1, 0, false).
		AddItem(content, 0, 1, true).
		AddItem(help, 1, 0, false)
	pages.AddPage("main", root, true, true)

	active := "containers"
	setTab := func(name string) {
		active = name
		content.SwitchToPage(name)
		if name == "containers" {
			tabBar.SetText(" [black:teal] Containers (1) [-:-]   Volumes (2) ")
			help.SetText(" [yellow]↑/↓ j/k h/l[white] move  [yellow]Tab/1/2[white] tabs  [yellow]Enter[white] menu  [yellow]r[white] refresh  [yellow]q[white] quit")
			app.SetFocus(ctree)
		} else {
			tabBar.SetText("  Containers (1)   [black:teal] Volumes (2) [-:-] ")
			help.SetText(" [yellow]↑/↓ j/k h/l[white] move  [yellow]Tab/1/2[white] tabs  [yellow]Enter[white] node list  [yellow]r[white] refresh  [yellow]q[white] quit")
			app.SetFocus(vtable)
			loadVolumes()
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
				return nil
			case 'j', 'l':
				return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
			case 'k', 'h':
				return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
			}
		}
		return ev
	}
	ctree.SetInputCapture(tabKeys)
	vtable.SetInputCapture(tabKeys)

	loadContainers()
	setTab("containers")

	if err := app.SetRoot(pages, true).EnableMouse(true).Run(); err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: %w", err)}
	}
	return nil
}

// shellLabel renders a menu shell entry, greying it once a probe confirms the
// container can't start that shell.
func shellLabel(name string, ok, probed bool) string {
	if probed && !ok {
		return "[gray]" + name + " (not available)[-]"
	}
	return name
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
