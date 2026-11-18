// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"swarmexec/client/internal/portfwd"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/dockerfmt"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// portConflict names the forward already holding a local port, if any, and
// renders it as a sentence the operator can act on.
//
// The cluster is the part that matters and the part the operating system cannot
// know: since forwards survive a cluster switch, the thing occupying port 8080
// is quite often something you started on a cluster that is not on screen.
//
// The bind address is deliberately not part of the match. 0.0.0.0:8080 and
// 127.0.0.1:8080 collide in the kernel anyway, and "this port is taken" is a
// rule an operator can hold in their head; "taken on this address but not that
// one" is not.
func (u *ui) portConflict(local uint32) (string, bool) {
	e, ok := u.forwards.ByLocalPort(local)
	if !ok {
		return "", false
	}
	what := dockerfmt.OrDash(e.Cand.Service)
	if e.Cand.Service == "" {
		what = dockerfmt.ShortID(e.Cand.ContainerID)
	}
	return fmt.Sprintf("port %d is already forwarded to %s on cluster %s", local, what, dockerfmt.OrDash(e.Cluster)), true
}

// startForward brings a forward up off the UI goroutine: dialling the agent
// can take up to the connect timeout, and blocking the UI for that would
// freeze the whole app. The entry is registered immediately in the starting
// state so the operator sees that something is happening.
//
// address is the local bind address; the form defaults it to loopback.
func (u *ui) startForward(c resolve.Candidate, address string, local, remote uint32) {
	// Refuse a port we are already using, before anything is created — the
	// backstop for every caller. The prompt checks too, so a typo can be
	// corrected without this ever being reached.
	if msg, clash := u.portConflict(local); clash {
		u.info(msg + ".\n\nStop it on the Forwards tab, or pick another local port.")
		return
	}
	app, ac, f, forwards := u.app, u.agents, u.f, u.forwards
	// runCtx, NOT the cluster's context: a forward is a local listener the
	// operator started deliberately, and it now survives a switch to another
	// cluster the way it already survived closing the overlay that started it.
	// Hanging it off the cluster context would cancel it the moment they looked
	// somewhere else.
	fctx, fcancel := context.WithCancel(u.runCtx)
	var once sync.Once
	entry := forwards.AddAt(u.activeCtx, address, c, local, remote, func() { once.Do(fcancel) })
	u.refreshForwardViews()

	go func() {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		fw, ferr := portfwd.Start(fctx, ac, ep, portfwd.Params{
			Address:        address,
			LocalPort:      local,
			RemotePort:     remote,
			ConnectTimeout: f.connectTimeout,
		})
		if ferr != nil {
			forwards.MarkFailed(entry.ID, ferr)
			app.QueueUpdateDraw(func() { u.refreshForwardViews() })
			return
		}
		addr := fw.LocalAddr().String()
		// Kept as a local: reading entry.LocalAddr later would race with
		// the registry's own writers.
		boundPort := local
		if _, ps, perr := net.SplitHostPort(addr); perr == nil {
			if n, cerr := strconv.ParseUint(ps, 10, 32); cerr == nil {
				boundPort = uint32(n)
			}
		}
		forwards.MarkActive(entry.ID, addr)
		app.QueueUpdateDraw(func() { u.refreshForwardViews() })

		fw.SetOnConnOK(func() {
			if forwards.ClearConnError(entry.ID) {
				app.QueueUpdateDraw(func() { u.refreshForwardViews() })
			}
		})
		serr := fw.Serve(fctx, func(cerr error) {
			// Announce the first failure only: against an outdated agent
			// every connection fails, and flashing each one would hide the
			// footer behind a stutter of identical messages.
			// Only the first failure changes anything visible (the ⚠
			// marker). Redrawing on every one would rebuild the whole
			// container tree per rejected connection — a browser hammering
			// a broken forward would turn that into a redraw storm.
			if !forwards.NoteConnError(entry.ID, cerr) {
				return
			}
			app.QueueUpdateDraw(func() {
				u.refreshForwardViews()
				u.flash(fmt.Sprintf(" [red]forward %d[white]: %v", boundPort, cerr))
			})
		})
		fw.Close()
		// A cancelled forward was stopped on purpose; anything else is a
		// real failure the operator needs to see in the table.
		if serr != nil && fctx.Err() == nil {
			forwards.MarkFailed(entry.ID, serr)
			app.QueueUpdateDraw(func() { u.refreshForwardViews() })
		}
	}()
}

// userPrompt asks which user/UID to exec as, then calls open with it. Mirrors
// the CLI's `exec -u`: a name, a UID, or UID:GID.
func (u *ui) userPrompt(c resolve.Candidate, open func(user string)) {
	ctree := u.ctree
	input := tview.NewInputField().SetLabel(" user: ").SetFieldWidth(28)
	input.SetBorder(true).SetTitle(fmt.Sprintf(" shell into %s on %s as… ", dockerfmt.OrDash(c.Service), dockerfmt.OrDash(c.NodeName)))
	input.SetPlaceholder("  name or UID[:GID] — e.g. root, 1000, 1000:1000")
	ov := u.overlayFor(pageUserPrompt, ctree, "")
	closePrompt := ov.Close
	input.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			closePrompt()
			return
		}
		usr := strings.TrimSpace(input.GetText())
		if usr == "" {
			input.SetTitle(" enter a username or UID (Esc to cancel) ")
			return
		}
		closePrompt()
		open(usr)
	})
	ov.show(centeredPrompt(input, 62), input)
}

// forwardsTab is the Forwards tab: every port forward of the session.
//
// The one tab with no per-cluster state, on purpose: a forward is a local
// listener the operator started and expects to keep, so the registry
// (u.forwards) is session-wide and the table shows every cluster's forwards
// side by side, with a CLUSTER column.
type forwardsTab struct {
	u     *ui
	table *tview.Table
	// rows mirrors the rendered table so a row maps to a forward.
	rows []portfwd.Entry
}

func newForwardsTab(u *ui) *forwardsTab {
	t := &forwardsTab{u: u}
	t.table = tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	t.table.SetSelectedStyle(u.selStyle)
	// Enter shows the full detail of a forward. The table truncates the state
	// column, so this is where a failure reason is actually readable.
	t.table.SetSelectedFunc(func(int, int) { t.showDetail() })
	t.table.SetInputCapture(t.keys)
	return t
}

// fwdTab is the session's Forwards tab.
func (u *ui) fwdTab() *forwardsTab { return u.tab("forwards").(*forwardsTab) }

func (t *forwardsTab) widget() tview.Primitive { return t.table }

// enter and refresh repaint: the forwards live in this process, there is
// nothing to load.
func (t *forwardsTab) enter()   { t.render() }
func (t *forwardsTab) refresh() { t.render() }

func (t *forwardsTab) footer() string {
	km := t.u.km
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  %s stop  [yellow]%s[white] copy url",
		keyTag(dangerKey(keyLabel(km.FwdStop))), keyLabel(km.FwdCopyURL))
}

func (t *forwardsTab) helpSection(h *helpWriter) {
	km := t.u.km
	h.sec("Forwards")
	h.line("Enter / i", "details")
	h.danger(keyLabel(km.FwdStop), "stop")
	h.line(keyLabel(km.FwdCopyURL), "copy URL")
}

func (t *forwardsTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "LOCAL\tREMOTE\tCONTAINER\tSERVICE\tNODE\tSTATE")
	for _, e := range t.u.forwards.List() {
		local := "-"
		if e.BoundPort() > 0 {
			local = e.LocalEndpoint()
		}
		fmt.Fprintf(b, "%s\t%d\t%s\t%s\t%s\t%s\n",
			local, e.Remote, dockerfmt.ShortID(e.Cand.ContainerID),
			dockerfmt.OrDash(e.Cand.Service), dockerfmt.OrDash(e.Cand.NodeName), e.State)
	}
}

// render repaints the table from the registry, keeping the cursor's row.
func (t *forwardsTab) render() {
	u, ftable := t.u, t.table
	prev, _ := ftable.GetSelection()
	tableHeaders(ftable, fHeaders)
	t.rows = u.forwards.List()
	for i, e := range t.rows {
		row := i + 1
		local := "-"
		if e.BoundPort() > 0 {
			local = e.LocalEndpoint()
		}
		state := e.State.String()
		color := tcell.ColorWhite
		switch e.State {
		case portfwd.StateActive:
			color = tcell.ColorGreen
			if e.ConnErr != nil {
				// Listening, but connections are failing — the operator
				// needs to see that, not a reassuring green "active". Kept
				// to a marker because the column truncates; Enter shows the
				// reason in full, and it is flashed once when it happens.
				color = tcell.ColorYellow
				state = "active ⚠"
			}
		case portfwd.StateStarting:
			color = tcell.ColorYellow
		case portfwd.StateFailed:
			color = tcell.ColorRed
			if e.Err != nil {
				// The reason matters more than the word "failed": it is the
				// only place the operator can learn what went wrong.
				state = "failed: " + e.Err.Error()
			}
		}
		// Escaped: an IPv6 endpoint such as [::1]:8080 is otherwise read as
		// a style tag and vanishes from the cell.
		lcell := tview.NewTableCell(tview.Escape(local))
		if !portfwd.IsLoopback(e.BindAddress()) {
			// Reachable from the network: worth a second look in a column
			// that is otherwise always loopback.
			lcell.SetTextColor(tcell.ColorYellow)
		}
		ftable.SetCell(row, 0, lcell)
		ftable.SetCell(row, 1, tview.NewTableCell(fmt.Sprintf("%d", e.Remote)))
		ftable.SetCell(row, 2, tview.NewTableCell(dockerfmt.ShortID(e.Cand.ContainerID)))
		ftable.SetCell(row, 3, tview.NewTableCell(dockerfmt.OrDash(e.Cand.Service)))
		ftable.SetCell(row, 4, tview.NewTableCell(dockerfmt.OrDash(e.Cand.NodeName)))
		// The cluster column is dimmed for the one you are looking at and plain
		// for the others, so a glance answers "is any of this somewhere else?"
		// without reading names.
		ccell := tview.NewTableCell(dockerfmt.OrDash(e.Cluster))
		if e.Cluster == u.activeCtx {
			ccell.SetTextColor(tcell.ColorGray)
		}
		ftable.SetCell(row, 5, ccell)
		ftable.SetCell(row, 6, tview.NewTableCell(uptime(time.Since(e.Started))))
		ftable.SetCell(row, 7, tview.NewTableCell(state).SetTextColor(color))
	}
	if len(t.rows) == 0 {
		// The state every other tab's empty now follows. It used to name the
		// key as a literal "p", which was quietly wrong for anyone who had
		// remapped it — the hint now comes from the live keymap like every
		// other key this program prints.
		ftable.SetCell(1, 0, emptyCell(emptyText("forwards", keyHint(u.km.Forward, "on a container starts one"))))
		return
	}
	if prev > 0 && prev <= len(t.rows) {
		ftable.Select(prev, 0)
	} else {
		ftable.Select(1, 0)
	}
}

// selected maps the cursor row back to a forward.
func (t *forwardsTab) selected() (portfwd.Entry, bool) {
	return selectedRow(t.table, t.rows)
}

func (t *forwardsTab) showDetail() {
	u := t.u
	forwards, ftable := u.forwards, t.table
	row, ok := t.selected()
	if !ok {
		return
	}
	// Re-read from the registry: the rendered row's connErr is only as
	// fresh as the last redraw, and redraws are deliberately rare.
	e, ok := forwards.Get(row.ID)
	if !ok {
		return
	}
	// A left-aligned TextView, not the u.info() modal: tview.Modal centers
	// each line on its own, which shears a padded key/value block out of
	// alignment. Values are escaped because dynamic colors are on and an
	// error string can contain "[".
	esc := tview.Escape
	var b strings.Builder
	fmt.Fprintf(&b, "local:      %s\n", esc(e.LocalEndpoint()))
	if !portfwd.IsLoopback(e.BindAddress()) {
		b.WriteString("            [yellow]not loopback: reachable from your network[-]\n")
	}
	fmt.Fprintf(&b, "remote:     %d\n", e.Remote)
	fmt.Fprintf(&b, "container:  %s\n", esc(dockerfmt.ShortID(e.Cand.ContainerID)))
	fmt.Fprintf(&b, "service:    %s\n", esc(dockerfmt.OrDash(e.Cand.Service)))
	fmt.Fprintf(&b, "node:       %s\n", esc(dockerfmt.OrDash(e.Cand.NodeName)))
	fmt.Fprintf(&b, "state:      %s", esc(e.State.String()))
	if e.Err != nil {
		fmt.Fprintf(&b, "\n\n[red]failed:[-] %s", esc(e.Err.Error()))
	}
	if e.ConnErr != nil {
		fmt.Fprintf(&b, "\n\n[yellow]last connection failed:[-]\n%s", esc(e.ConnErr.Error()))
	}
	b.WriteString("\n\n[gray]Esc[-] back   " + keyTag(dangerKey(keyLabel(u.km.FwdStop))) + " stop")

	tv := tview.NewTextView().SetDynamicColors(true).SetText(b.String())
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" forward #%d ", e.ID))
	ov := u.overlayFor(pageFwdDetail, ftable, "")
	closeDetail := ov.Close
	cases := u.forwardDetailCases(closeDetail, func() {
		// Stop straight from the detail view — the operator is already
		// looking at what they are about to kill. Same key as the table.
		forwards.Remove(e.ID)
		closeDetail()
		u.refreshForwardViews()
		u.flash(fmt.Sprintf(" [green]stopped[white] forward to %s:%d", dockerfmt.ShortID(e.Cand.ContainerID), e.Remote))
	})
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyEnter {
			closeDetail()
			return nil
		}
		if runRune(ev, cases) {
			return nil
		}
		return ev
	})
	// Height tracks the content so a short forward gets a snug box and a
	// failed one grows to fit its reason.
	lines := strings.Count(b.String(), "\n") + 1
	ov.show(centered(tv, 66, lines+2), tv)
}

// forwardDetailCases is the forward detail's rune keys. stop removes the
// forward, closes the detail and reports it.
func (u *ui) forwardDetailCases(closeDetail, stop func()) []runeCase {
	return []runeCase{
		{u.km.Quit, "quit", closeDetail},
		{'i', "", closeDetail},
		{u.km.FwdStop, "forward_stop", stop},
	}
}

// keys is the key handler of the Forwards table.
//
// On the forwards table: Enter/i details, forward_stop (X) stops the forward,
// forward_copy_url (o) copies its URL.
func (t *forwardsTab) keys(ev *tcell.EventKey) *tcell.EventKey {
	u := t.u
	km, forwards, screen := u.km, u.forwards, u.screen
	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case 'i':
			t.showDetail()
			return nil
		case km.FwdStop:
			if e, ok := t.selected(); ok {
				forwards.Remove(e.ID)
				u.refreshForwardViews()
				u.flash(fmt.Sprintf(" [green]stopped[white] forward to %s:%d", dockerfmt.ShortID(e.Cand.ContainerID), e.Remote))
			}
			return nil
		case km.FwdCopyURL:
			// Copy rather than launch a browser: the UI often runs over
			// ssh, where opening a local browser would target the wrong
			// machine — and the forward is bound on the operator's side.
			if e, ok := t.selected(); ok && e.State == portfwd.StateActive {
				url := e.URL()
				if screen != nil {
					screen.SetClipboard([]byte(url))
				}
				u.flash(" [green]copied[white] " + url)
			}
			return nil
		}
	}
	return u.tabKeys(ev)
}
