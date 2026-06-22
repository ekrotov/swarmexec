package cli

import (
	"context"
	"fmt"
	"os"
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

	dcli, err := newDockerClient()
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

	// attach suspends the TUI, runs the exec session on the real terminal, then
	// resumes the TUI.
	attach := func(c resolve.Candidate, command []string, tty bool) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		app.Suspend(func() {
			fmt.Printf("\n\033[2mswarmexec: %v in %s on %s — Ctrl-D / exit to return\033[0m\n",
				command, shortID(c.ContainerID), orDash(c.NodeName))
			if _, eerr := execInto(ctx, cfg, ep, execParams{
				command:        command,
				tty:            tty,
				keepStdin:      true,
				connectTimeout: f.connectTimeout,
			}); eerr != nil {
				fmt.Printf("\033[31mswarmexec: %v\033[0m\n", eerr)
				fmt.Print("press Enter to return… ")
				fmt.Scanln()
			}
		})
		load() // task may have changed while we were away
	}

	info := func(msg string) {
		m := tview.NewModal().SetText(msg).AddButtons([]string{"OK"}).
			SetDoneFunc(func(int, string) { pages.RemovePage("info"); app.SetFocus(table) })
		pages.AddPage("info", m, true, true)
		app.SetFocus(m)
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
					attach(c, []string{"bash"}, true)
				case "Sh":
					attach(c, []string{"sh"}, false)
				case "Logs":
					info("Container logs need a dedicated agent RPC and are coming in a\nseparate step. For now use Bash/Sh.")
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
