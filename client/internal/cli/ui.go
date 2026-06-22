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
		Short: "Interactive table of containers; pick one for logs/bash/sh",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUI(cmd, g, f, args)
		},
	}
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to the agent")
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
		return &cliError{code: usageExitCode, err: fmt.Errorf("ui needs an interactive terminal (use plain `ps` when piping)")}
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
	pages := tview.NewPages()

	table := tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	table.SetSelectedStyle(tcell.StyleDefault.Background(tcell.ColorTeal).Foreground(tcell.ColorWhite))

	var cands []resolve.Candidate

	headers := []string{"SERVICE", "SLOT", "CONTAINER", "NODE", "IP", "UPTIME"}
	load := func() {
		cs, lerr := r.Candidates(ctx, service)
		cands = cs
		table.Clear()
		for c, h := range headers {
			table.SetCell(0, c, tview.NewTableCell(h).
				SetTextColor(tcell.ColorYellow).
				SetAttributes(tcell.AttrBold).
				SetSelectable(false))
		}
		if lerr != nil {
			table.SetCell(1, 0, tview.NewTableCell("error: "+lerr.Error()).SetTextColor(tcell.ColorRed))
			return
		}
		for i, c := range cands {
			slot := "-"
			if c.Slot > 0 {
				slot = fmt.Sprintf("%d", c.Slot)
			}
			vals := []string{orDash(c.Service), slot, shortID(c.ContainerID), orDash(c.NodeName), orDash(c.NodeAddr), uptime(c.Uptime)}
			for col, v := range vals {
				table.SetCell(i+1, col, tview.NewTableCell(v).SetExpansion(1))
			}
		}
		if len(cands) > 0 {
			table.Select(1, 0)
		}
	}
	load()

	help := tview.NewTextView().SetDynamicColors(true).SetText(
		" [yellow]↑/↓ j/k h/l[white] navigate   [yellow]Enter[white] menu   [yellow]r[white] refresh   [yellow]q[white] quit")

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(table, 0, 1, true).
		AddItem(help, 1, 0, false)
	pages.AddPage("main", root, true, true)

	selected := func() (resolve.Candidate, bool) {
		row, _ := table.GetSelection()
		idx := row - 1
		if idx < 0 || idx >= len(cands) {
			return resolve.Candidate{}, false
		}
		return cands[idx], true
	}

	// openTerminal runs an interactive shell inside a modal terminal pane (a
	// vt10x emulator bridged to the exec stream). Ctrl-] detaches.
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
				app.SetFocus(table)
				load() // the task may have changed while we were attached
			})
		}
		tv.detach = closeTerm
		tv.run(tctx, cfg, ep, command, tty, f.connectTimeout, func(int, error) {
			app.QueueUpdateDraw(closeTerm)
		})

		pages.AddPage("term", tv, true, true)
		app.SetFocus(tv)
	}

	// showLogs opens a scrollable, live (follow) logs viewer for a container.
	showLogs := func(c resolve.Candidate) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
		tv.SetBorder(true).SetTitle(fmt.Sprintf(" logs %s on %s — ↑/↓ scroll, ESC/q close ", shortID(c.ContainerID), orDash(c.NodeName)))

		lctx, lcancel := context.WithCancel(ctx)
		closeLogs := func() {
			lcancel()
			pages.RemovePage("logs")
			app.SetFocus(table)
		}
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == 'q') {
				closeLogs()
				return nil
			}
			return ev
		})

		go func() {
			err := streamLogs(lctx, cfg, ep,
				logsParams{follow: true, tail: 1000, connectTimeout: f.connectTimeout},
				tvLogWriter{app: app, tv: tv}, tvLogWriter{app: app, tv: tv, stderr: true})
			if err != nil && lctx.Err() == nil {
				app.QueueUpdateDraw(func() { fmt.Fprintf(tv, "\n[red]error: %s[-]\n", tview.Escape(err.Error())) })
			}
		}()

		pages.AddPage("logs", tv, true, true)
		app.SetFocus(tv)
	}

	menu := func(c resolve.Candidate) {
		m := tview.NewModal().
			SetText(fmt.Sprintf("%s   on %s\ncontainer %s", orDash(c.Service), orDash(c.NodeName), shortID(c.ContainerID))).
			AddButtons([]string{"Logs", "Bash", "Sh", "Cancel"}).
			SetDoneFunc(func(_ int, label string) {
				pages.RemovePage("menu")
				app.SetFocus(table)
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

	table.SetSelectedFunc(func(int, int) {
		if c, ok := selected(); ok {
			menu(c)
		}
	})
	table.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case 'q':
				app.Stop()
				return nil
			case 'r':
				load()
				return nil
			case 'j', 'l':
				return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
			case 'k', 'h':
				return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
			}
		}
		return ev
	})

	if err := app.SetRoot(pages, true).SetFocus(table).EnableMouse(true).Run(); err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: %w", err)}
	}
	return nil
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
