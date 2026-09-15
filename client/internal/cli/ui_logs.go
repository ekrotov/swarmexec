// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/logfmt"
	"swarmexec/client/internal/resolve"
)

// logDefaults resolves the log view's initial format+filter from config,
// falling back to sane defaults when the config values are invalid.
func (u *ui) logDefaults() (logfmt.Format, logfmt.Filter) {
	cfg := u.cfg
	format, filter, err := buildLogFilter(cfg.Logs.Format, cfg.Logs.MinLevel, "")
	if err != nil {
		return logfmt.DefaultFormat(), logfmt.Filter{}
	}
	return format, filter
}

// logGrepPrompt asks for a message regexp and applies it to a log view.
func (u *ui) logGrepPrompt(lv *logViewer, back tview.Primitive, after func()) {
	pages, app := u.pages, u.app
	in := tview.NewInputField().SetLabel("grep: ").SetFieldWidth(44).
		SetPlaceholder("regexp on the message — empty clears")
	in.SetDoneFunc(func(key tcell.Key) {
		pages.RemovePage(pageLogGrep)
		app.SetFocus(back)
		if key == tcell.KeyEscape {
			return
		}
		txt := strings.TrimSpace(in.GetText())
		if txt == "" {
			lv.setGrep(nil)
			after()
			return
		}
		re, err := regexp.Compile(txt)
		if err != nil {
			u.info("invalid grep regexp: " + err.Error())
			return
		}
		lv.setGrep(re)
		after()
	})
	in.SetBorder(true).SetTitle(" filter logs ")
	pages.AddPage(pageLogGrep, centeredPrompt(in, 64), true, true)
	app.SetFocus(in)
}

// logFooterText is the log view's footer hint. It appends the mouse state
// because that is what decides whether terminal text-selection works: while
// the app captures the mouse (the default), tview grabs drags for scrolling
// and the terminal cannot select/copy — press m to hand the mouse back.
func (u *ui) logFooterText(following bool) string {
	mouseEnabled := u.mouseEnabled
	m := " [yellow]m[white] mouse: app — press to select/copy in terminal"
	if !mouseEnabled {
		m = " [yellow]m[white] mouse: off — select & copy with your terminal"
	}
	return logViewHelp(following) + "  " + m
}

// logViewKeys is the shared input capture for a log view: close, follow,
// cycle format (F) / min-level (l), grep (/) and mouse capture (m).
func (u *ui) logViewKeys(lv *logViewer, follow *atomic.Bool, tv *tview.TextView, closeLogs, setTitle, refreshHint func()) func(*tcell.EventKey) *tcell.EventKey {
	return func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == 'q'):
			closeLogs()
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'f':
			follow.Store(!follow.Load())
			if follow.Load() {
				tv.ScrollToEnd() // re-enabling: jump to the newest line
			}
			setTitle()
			refreshHint() // the footer names the state too, not just the title
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'F':
			lv.cycleFormat()
			setTitle()
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'l':
			lv.cycleLevel()
			setTitle()
		case ev.Key() == tcell.KeyRune && ev.Rune() == '/':
			u.logGrepPrompt(lv, tv, setTitle)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'm':
			// Same mouse toggle as the tabs, but the tabs' flash lands on the
			// footer hidden behind this overlay, so reflect the state in the
			// log footer instead.
			u.toggleMouse()
			refreshHint()
		default:
			return ev
		}
		return nil
	}
}

// logPage wraps a log TextView with a footer key-hint line — the same place
// every tab shows its shortcuts — so the log view's keys are consistent and
// spelled out, instead of being crammed into the border title. It returns the
// page and a closure that repaints the hint, called whenever a state the hint
// reports changes: the mouse capture, and following.
func (u *ui) logPage(tv *tview.TextView, follow *atomic.Bool) (tview.Primitive, func()) {
	hint := tview.NewTextView().SetDynamicColors(true).SetText(u.logFooterText(follow.Load()))
	page := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tv, 0, 1, true).
		AddItem(hint, 1, 0, false)
	return page, func() { hint.SetText(u.logFooterText(follow.Load())) }
}

func (u *ui) showLogs(c resolve.Candidate) {
	app, ctree, pages := u.app, u.ctree, u.pages
	cfg, r, ctx, f := u.cfg, u.r, u.ctx, u.f
	ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
	follow := &atomic.Bool{}
	follow.Store(true)
	format, filter := u.logDefaults()
	lv := newLogViewer(app, tv, follow, format, filter)
	setTitle := func() {
		state := "on"
		if !follow.Load() {
			state = "off"
		}
		tv.SetTitle(fmt.Sprintf(" logs %s on %s — follow:%s · %s ",
			shortID(c.ContainerID), orDash(c.NodeName), state, lv.status()))
	}
	tv.SetBorder(true)
	setTitle()
	page, refreshHint := u.logPage(tv, follow)
	lctx, lcancel := context.WithCancel(ctx)
	lv.start(lctx) // bounded redraw rate; see logFlushInterval
	closeLogs := func() { lcancel(); pages.RemovePage(pageLogs); app.SetFocus(ctree) }
	tv.SetInputCapture(u.logViewKeys(lv, follow, tv, closeLogs, setTitle, refreshHint))
	target := resolve.FollowTarget{Service: c.Service, Slot: c.Slot, NodeID: c.NodeID}
	go func() {
		lerr := streamServiceLogs(lctx, cfg, r, target, ep,
			logsParams{follow: true, tail: 1000, connectTimeout: f.connectTimeout},
			&logIngest{v: lv}, &logIngest{v: lv, stderr: true}, lv.addNote)
		if lerr != nil && lctx.Err() == nil {
			app.QueueUpdateDraw(func() { fmt.Fprintf(tv, "\n[red]error: %s[-]\n", tview.Escape(lerr.Error())) })
		}
	}()
	pages.AddPage(pageLogs, page, true, true)
	app.SetFocus(tv)
}

// showServiceLogs streams the logs of every container of a service into one
// viewer, each line prefixed with [container@node].
func (u *ui) showServiceLogs(serviceName string, members []resolve.Candidate) {
	app, ctree, pages := u.app, u.ctree, u.pages
	cfg, r, ctx, f := u.cfg, u.r, u.ctx, u.f
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
	follow := &atomic.Bool{}
	follow.Store(true)
	format, filter := u.logDefaults()
	lv := newLogViewer(app, tv, follow, format, filter)
	setTitle := func() {
		state := "on"
		if !follow.Load() {
			state = "off"
		}
		tv.SetTitle(fmt.Sprintf(" service logs %s (%d containers) — follow:%s · %s ",
			serviceName, len(members), state, lv.status()))
	}
	tv.SetBorder(true)
	setTitle()
	page, refreshHint := u.logPage(tv, follow)
	lctx, lcancel := context.WithCancel(ctx)
	lv.start(lctx) // bounded redraw rate; see logFlushInterval
	closeLogs := func() { lcancel(); pages.RemovePage(pageLogs); app.SetFocus(ctree) }
	tv.SetInputCapture(u.logViewKeys(lv, follow, tv, closeLogs, setTitle, refreshHint))
	for _, c := range members {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		target := resolve.FollowTarget{Service: c.Service, Slot: c.Slot, NodeID: c.NodeID}
		// A slot (or node, for a global service) tag stays valid across
		// replacements, unlike the container id — the reconnect notice reports
		// the new container/node.
		prefix := fmt.Sprintf("[slot %d] ", c.Slot)
		if c.Slot == 0 {
			prefix = fmt.Sprintf("[%s] ", orDash(c.NodeName))
		}
		go func(ep resolve.Endpoint, target resolve.FollowTarget, prefix string) {
			lerr := streamServiceLogs(lctx, cfg, r, target, ep,
				logsParams{follow: true, tail: 200, connectTimeout: f.connectTimeout},
				&logIngest{v: lv, prefix: prefix},
				&logIngest{v: lv, prefix: prefix, stderr: true},
				func(msg string) { lv.addNote(prefix + msg) })
			if lerr != nil && lctx.Err() == nil {
				app.QueueUpdateDraw(func() {
					fmt.Fprintf(tv, "[red]%serror: %s[-]\n", prefix, tview.Escape(lerr.Error()))
				})
			}
		}(ep, target, prefix)
	}
	pages.AddPage(pageLogs, page, true, true)
	app.SetFocus(tv)
}

// showLogsForNode opens logs for the tree cursor: a container leaf shows that
// container's logs; a service node shows its containers' aggregated logs.
// Bound to the L key (Enter on a service toggles expand/collapse instead).
func (u *ui) showLogsForNode(node *tview.TreeNode) {
	if c, ok := node.GetReference().(resolve.Candidate); ok {
		u.showLogs(c)
		return
	}
	if !isServiceNode(node) {
		return
	}
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
		u.showServiceLogs(title, members)
	}
}
