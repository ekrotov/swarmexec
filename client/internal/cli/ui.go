package cli

import (
	"context"
	"fmt"
	"os"
	"sync"
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
	ctable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	ctable.SetSelectedStyle(selStyle)
	cHeaders := []string{"SERVICE", "SLOT", "CONTAINER", "NODE", "IP", "UPTIME"}
	var cands []resolve.Candidate
	loadContainers := func() {
		cs, lerr := r.Candidates(ctx, service)
		cands = cs
		ctable.Clear()
		for c, h := range cHeaders {
			ctable.SetCell(0, c, headerCell(h))
		}
		if lerr != nil {
			ctable.SetCell(1, 0, tview.NewTableCell("error: "+lerr.Error()).SetTextColor(tcell.ColorRed))
			return
		}
		for i, c := range cands {
			slot := "-"
			if c.Slot > 0 {
				slot = fmt.Sprintf("%d", c.Slot)
			}
			vals := []string{orDash(c.Service), slot, shortID(c.ContainerID), orDash(c.NodeName), orDash(c.NodeAddr), uptime(c.Uptime)}
			for col, v := range vals {
				ctable.SetCell(i+1, col, tview.NewTableCell(v).SetExpansion(1))
			}
		}
		if len(cands) > 0 {
			ctable.Select(1, 0)
		}
	}
	selectedContainer := func() (resolve.Candidate, bool) {
		row, _ := ctable.GetSelection()
		i := row - 1
		if i < 0 || i >= len(cands) {
			return resolve.Candidate{}, false
		}
		return cands[i], true
	}

	openTerminal := func(c resolve.Candidate, command []string, tty bool) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		tctx, tcancel := context.WithCancel(ctx)
		tv := newTerminalView(app)
		tv.SetTitle(fmt.Sprintf(" %v in %s on %s — Ctrl-] detach ", command, shortID(c.ContainerID), orDash(c.NodeName)))
		var once sync.Once
		closeTerm := func() {
			once.Do(func() {
				tcancel()
				pages.RemovePage("term")
				app.SetFocus(ctable)
				loadContainers()
			})
		}
		tv.detach = closeTerm
		tv.run(tctx, cfg, ep, command, tty, f.connectTimeout, func(int, error) { app.QueueUpdateDraw(closeTerm) })
		pages.AddPage("term", tv, true, true)
		app.SetFocus(tv)
	}

	showLogs := func(c resolve.Candidate) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
		tv.SetBorder(true).SetTitle(fmt.Sprintf(" logs %s on %s — ↑/↓ scroll, ESC/q close ", shortID(c.ContainerID), orDash(c.NodeName)))
		lctx, lcancel := context.WithCancel(ctx)
		closeLogs := func() { lcancel(); pages.RemovePage("logs"); app.SetFocus(ctable) }
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == 'q') {
				closeLogs()
				return nil
			}
			return ev
		})
		go func() {
			lerr := streamLogs(lctx, cfg, ep, logsParams{follow: true, tail: 1000, connectTimeout: f.connectTimeout},
				tvLogWriter{app: app, tv: tv}, tvLogWriter{app: app, tv: tv, stderr: true})
			if lerr != nil && lctx.Err() == nil {
				app.QueueUpdateDraw(func() { fmt.Fprintf(tv, "\n[red]error: %s[-]\n", tview.Escape(lerr.Error())) })
			}
		}()
		pages.AddPage("logs", tv, true, true)
		app.SetFocus(tv)
	}

	containerMenu := func(c resolve.Candidate) {
		m := tview.NewModal().
			SetText(fmt.Sprintf("%s   on %s\ncontainer %s", orDash(c.Service), orDash(c.NodeName), shortID(c.ContainerID))).
			AddButtons([]string{"Logs", "Bash", "Sh", "Cancel"}).
			SetDoneFunc(func(_ int, label string) {
				pages.RemovePage("menu")
				app.SetFocus(ctable)
				switch label {
				case "Bash":
					openTerminal(c, []string{"bash"}, true)
				case "Sh":
					openTerminal(c, []string{"sh"}, false)
				case "Logs":
					showLogs(c)
				}
			})
		pages.AddPage("menu", m, true, true)
		app.SetFocus(m)
	}
	ctable.SetSelectedFunc(func(int, int) {
		if c, ok := selectedContainer(); ok {
			containerMenu(c)
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
			if nerr == nil {
				vs, errs = indexVolumes(ctx, cfg, nodes, f.connectTimeout)
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
	content.AddPage("containers", ctable, true, true)
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
			app.SetFocus(ctable)
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
	ctable.SetInputCapture(tabKeys)
	vtable.SetInputCapture(tabKeys)

	loadContainers()
	setTab("containers")

	if err := app.SetRoot(pages, true).EnableMouse(true).Run(); err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: %w", err)}
	}
	return nil
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
}

func (w tvLogWriter) Write(p []byte) (int, error) {
	s := tview.Escape(string(p))
	w.app.QueueUpdateDraw(func() {
		if w.stderr {
			fmt.Fprintf(w.tv, "[red]%s[-]", s)
		} else {
			fmt.Fprint(w.tv, s)
		}
		w.tv.ScrollToEnd()
	})
	return len(p), nil
}
