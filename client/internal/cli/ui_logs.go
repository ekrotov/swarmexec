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
// falling back to sane defaults when the config values are invalid. The bool
// reports `logs.format: auto` — the view then decides from the lines instead of
// from the config.
func (u *ui) logDefaults() (logfmt.Format, logfmt.Filter, bool) {
	cfg := u.cfg
	format, filter, err := buildLogFilter(cfg.Logs.Format, cfg.Logs.MinLevel, "")
	if err != nil {
		return logfmt.DefaultFormat(), logfmt.Filter{}, false
	}
	return format, filter, logFormatAuto(cfg.Logs.Format)
}

// newLogView builds the viewer both log views share: format+filter from the
// config, detection armed when the config asks for it, and the title kept in
// step with a format the detector changes on its own.
func (u *ui) newLogView(tv *tview.TextView, follow *atomic.Bool) *logViewer {
	format, filter, auto := u.logDefaults()
	lv := newLogViewer(u.app, tv, follow, format, filter)
	if auto {
		lv.armAutoDetect()
	}
	return lv
}

// logGrepPrompt asks for a message regexp and applies it to a log view.
func (u *ui) logGrepPrompt(lv *logViewer, back tview.Primitive, after func()) {
	in := tview.NewInputField().SetLabel("grep: ").SetFieldWidth(44).
		SetPlaceholder("regexp on the message — empty clears")
	ov := u.overlayFor(pageLogGrep, back, "")
	in.SetDoneFunc(func(key tcell.Key) {
		ov.Close()
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
	ov.show(centeredPrompt(in, 64), in)
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
// cycle format (F) / auto-detect it (a) / min-level (l), grep (/) and mouse
// capture (m).
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
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
			// Five formats and no way to know which one this container uses is
			// a question the output can answer better than the operator can.
			u.info(lv.autoDetectNow())
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
	app, ctree := u.app, u.ctree
	cfg, r, ctx, f := u.cfg, u.r, u.ctx, u.f
	ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
	follow := &atomic.Bool{}
	follow.Store(true)
	lv := u.newLogView(tv, follow)
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
	lv.onFormat = setTitle // the detector can change the format without a keypress
	page, refreshHint := u.logPage(tv, follow)
	lctx, lcancel := context.WithCancel(ctx)
	lv.start(lctx) // bounded redraw rate; see logFlushInterval
	ov := u.overlayFor(pageLogs, ctree, "")
	// The stream dies with the view, so the cancel rides along with the close
	// rather than being a second thing every exit has to remember.
	closeLogs := func() { lcancel(); ov.Close() }
	tv.SetInputCapture(u.logViewKeys(lv, follow, tv, closeLogs, setTitle, refreshHint))
	target := resolve.FollowTarget{Service: c.Service, Slot: c.Slot, NodeID: c.NodeID}
	// The container's own account of what happened to it, woven into the same
	// buffer as its output: health transitions, OOM kills, exits with their
	// code. None of that is in the log stream, and none of it reaches the
	// manager — a task reads "running" while its container fails every probe.
	//
	// Only in the single-container view. The service view already holds one log
	// stream per replica; a second stream each would double that for a signal
	// that is mostly noise when it arrives from one of fifty containers.
	// Buffered by one: the follower only ever needs to know THAT something
	// happened, not how often.
	wake := make(chan struct{}, 1)
	go watchContainerEvents(lctx, cfg, ep, f.connectTimeout, lv.addNote, wake)
	go func() {
		lerr := streamServiceLogs(lctx, cfg, r, target, ep,
			logsParams{follow: true, tail: 1000, connectTimeout: f.connectTimeout, wake: wake},
			&logIngest{v: lv}, &logIngest{v: lv, stderr: true}, lv.addNote)
		if lerr != nil && lctx.Err() == nil {
			app.QueueUpdateDraw(func() { fmt.Fprintf(tv, "\n[red]error: %s[-]\n", tview.Escape(lerr.Error())) })
		}
	}()
	ov.show(page, tv)
}

// showServiceLogs streams the logs of every container of a service into one
// viewer, each line prefixed with [container@node].
func (u *ui) showServiceLogs(serviceName string, members []resolve.Candidate) {
	app, ctree := u.app, u.ctree
	cfg, r, ctx, f := u.cfg, u.r, u.ctx, u.f
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
	follow := &atomic.Bool{}
	follow.Store(true)
	lv := u.newLogView(tv, follow)
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
	lv.onFormat = setTitle // the detector can change the format without a keypress
	page, refreshHint := u.logPage(tv, follow)
	lctx, lcancel := context.WithCancel(ctx)
	lv.start(lctx) // bounded redraw rate; see logFlushInterval
	ov := u.overlayFor(pageLogs, ctree, "")
	// The stream dies with the view, so the cancel rides along with the close
	// rather than being a second thing every exit has to remember.
	closeLogs := func() { lcancel(); ov.Close() }
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
	ov.show(page, tv)
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
