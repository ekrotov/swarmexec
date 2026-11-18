// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strings"
	"swarmexec/client/internal/dockerctx"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) renderContexts() {
	cxtable := u.cxtable
	selName := ""
	if row, _ := cxtable.GetSelection(); row >= 0 {
		if c := cxtable.GetCell(row, 0); c != nil {
			selName = strings.TrimLeft(c.Text, "▶·✗ ")
		}
	}
	cxtable.Clear()
	// No header row and no endpoint column: the sidebar is a navigation list,
	// not a table. A "CONTEXT" header over a framed box titled "contexts" says
	// nothing twice, and the endpoint never fit in this width — it is in the
	// detail overlay (i), where there is room to read it.
	if len(u.ctxs) == 0 {
		cxtable.SetCell(0, 0, emptyCell(emptyText("contexts", keyHint(u.km.CtxNew, "to add one"))))
		return
	}
	selRow := 0
	for i, c := range u.ctxs {
		label, color := "  "+c.Name, tcell.ColorWhite
		switch {
		case c.Name == u.activeCtx:
			label, color = "▶ "+c.Name, tcell.ColorAqua // the active session context
		case u.clusters[c.Name] != nil && u.clusters[c.Name].dcli != nil:
			// Visited and still connected: switching here is instant and lands
			// where the operator left off.
			label, color = "· "+c.Name, tcell.ColorGreen
		case u.clusters[c.Name] != nil && u.clusters[c.Name].err != nil:
			// Tried and refused. Saying so here is the difference between "I
			// wonder why nothing happened" and "that one is down" — the switch
			// itself deliberately leaves the operator where they were.
			label, color = "✗ "+c.Name, tcell.ColorRed
		}
		cxtable.SetCell(i, 0, tview.NewTableCell(label).SetTextColor(color).SetExpansion(1))
		if c.Name == selName {
			selRow = i
		}
	}
	if len(u.ctxs) > 0 {
		cxtable.Select(selRow, 0)
	}
}

func (u *ui) loadContexts() {
	cxtable := u.cxtable
	list, err := dockerctx.List()
	if err != nil {
		cxtable.Clear()
		cxtable.SetCell(0, 0, errorCell(err))
		return
	}
	u.ctxs = list
	u.renderContexts()
	u.updateStatusRight() // the hint needs to know there are contexts to show
}

// selectedContext maps the cursor row to a context.
//
// Deliberately NOT the shared selectedRow: that one subtracts the header row
// every other table has, and the sidebar has none. Reusing it here would return
// the context ABOVE the one under the cursor — switching to the wrong cluster
// on every keystroke, which is the worst possible way to be off by one.
func (u *ui) selectedContext() (dockerctx.Context, bool) {
	row, _ := u.cxtable.GetSelection()
	if row < 0 || row >= len(u.ctxs) {
		return dockerctx.Context{}, false
	}
	return u.ctxs[row], true
}

// showCreateContext opens a guided form to add a docker context. The operator
// explicitly decides whether to connect over SSH and, if so, whether to go
// through a jump host — the relevant fields appear only when opted in. For SSH
// the jump host(s) are stored on the context and injected as -J into both the
// Docker-API and agent-tunnel ssh connections (no ~/.ssh/config needed).
// "Test" verifies the assembled endpoint (a live daemon ping) before saving.
func (u *ui) showCreateContext() {
	app, cxtable := u.app, u.cxtable
	c := &newContextFields{useSSH: true}
	form := tview.NewForm()
	form.SetItemPadding(1) // one blank row between fields (the overlay has ample height)
	form.SetBorder(true).SetTitle(" new context ")
	ov := u.overlayFor(pageCtxForm, cxtable, footerKeys("Tab", "move", "Space", "toggle", "Enter", "confirm", "Esc", "cancel"))
	closeForm := ov.Close

	var render func()
	// toggle is a checkbox that shows or hides fields: the form is rebuilt,
	// and focus restored to it.
	toggle := func(flag *bool) func(bool) {
		return func(on bool) {
			if on != *flag {
				*flag = on
				render()
				app.SetFocus(form)
			}
		}
	}
	render = func() {
		form.Clear(true)
		form.AddInputField("Name", c.name, 32, nil, func(t string) { c.name = t })
		form.AddCheckbox("Connect to Docker over SSH", c.useSSH, toggle(&c.useSSH))
		if c.useSSH {
			form.AddInputField("SSH user", c.sshUser, 20, nil, func(t string) { c.sshUser = t })
			form.AddInputField("SSH host", c.sshHost, 28, nil, func(t string) { c.sshHost = t })
			form.AddInputField("SSH port (22)", c.sshPort, 8, nil, func(t string) { c.sshPort = t })
			form.AddCheckbox("Use a jump host (bastion)", c.useJump, toggle(&c.useJump))
			if c.useJump {
				addPlaceholderField(form, "Jump host(s)", c.jump, "bastion  (or edge,bastion for multi-hop)", func(t string) { c.jump = t })
			}
		} else {
			addPlaceholderField(form, "Docker host", c.plainHost, "tcp://host:2376 | unix:///var/run/docker.sock", func(t string) { c.plainHost = t })
		}
		form.AddInputField("Description", c.desc, 34, nil, func(t string) { c.desc = t })
		form.AddButton("Test", func() { u.testNewContext(c) })
		form.AddButton("Create", func() { u.createNewContext(c, closeForm) })
		form.AddButton("Cancel", closeForm)
	}
	render()
	form.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			closeForm()
			return nil
		}
		return ev
	})
	ov.show(centered(form, 76, 22), form)
}

// newContextFields is what the new-context form has collected so far.
type newContextFields struct {
	name, desc                      string
	useSSH, useJump                 bool
	sshUser, sshHost, sshPort, jump string
	plainHost                       string
}

// host is the Docker host the fields describe: an ssh:// URL, or the typed
// tcp:// / unix:// one.
func (c *newContextFields) host() (string, error) {
	if c.useSSH {
		return dockerctx.BuildSSHHost(c.sshUser, c.sshHost, c.sshPort)
	}
	h := strings.TrimSpace(c.plainHost)
	if h == "" {
		return "", fmt.Errorf("docker host is required (tcp:// or unix://)")
	}
	return h, nil
}

// proxyJump is the jump host(s), when SSH goes through one.
func (c *newContextFields) proxyJump() string {
	if c.useSSH && c.useJump {
		return strings.TrimSpace(c.jump)
	}
	return ""
}

// addPlaceholderField adds a 34-wide input field with a placeholder.
func addPlaceholderField(form *tview.Form, label, value, placeholder string, changed func(string)) {
	form.AddInputField(label, value, 34, nil, changed)
	if hf, ok := form.GetFormItem(form.GetFormItemCount() - 1).(*tview.InputField); ok {
		hf.SetPlaceholder(placeholder)
	}
}

// testNewContext is the form's Test button: a live daemon ping of the
// assembled endpoint, off the event loop.
func (u *ui) testNewContext(c *newContextFields) {
	host, err := c.host()
	if err != nil {
		u.info(err.Error())
		return
	}
	pj := c.proxyJump()
	u.flash(" [gray]testing " + host + " …[white]")
	asyncDo(u, "ui.contexts.test", func(ctx context.Context) error {
		return pingDockerHost(ctx, host, pj)
	}, func(terr error) {
		if terr != nil {
			u.info("test failed: " + terr.Error())
			return
		}
		u.flash(" [green]✓ connection ok[white] — " + host)
	})
}

// createNewContext is the form's Create button: save the context, close the
// form, reload the list.
func (u *ui) createNewContext(c *newContextFields, closeForm func()) {
	host, err := c.host()
	if err != nil {
		u.info(err.Error())
		return
	}
	if err := dockerctx.Create(dockerctx.CreateOptions{Name: strings.TrimSpace(c.name), Host: host, Description: strings.TrimSpace(c.desc), ProxyJump: c.proxyJump()}); err != nil {
		u.info("create failed: " + err.Error())
		return
	}
	closeForm()
	u.loadContexts()
	u.flash(" [green]created[white] context " + strings.TrimSpace(c.name))
}

// deleteContext removes the selected context behind a confirm. "default" is
// protected; removing the current one resets the selection to default.
func (u *ui) deleteContext(c dockerctx.Context) {
	cxtable := u.cxtable
	if c.Name == "default" {
		u.info("the built-in \"default\" context cannot be removed")
		return
	}
	force := c.Name == dockerctx.Current()
	msg := fmt.Sprintf("Remove context %q?\n%s", c.Name, orDash(c.Host))
	if force {
		msg += "\n\nIt is the current context — its selection resets to \"default\"."
	}
	u.confirm(msg, "Delete", cxtable, func() {
		if err := dockerctx.Remove(c.Name, force); err != nil {
			u.info("remove failed: " + err.Error())
			return
		}
		u.forgetCluster(c.Name)
		u.loadContexts()
		u.flash(" [green]removed[white] context " + c.Name)
	})
}

// activateContext switches the UI to c's cluster, behind a confirm when the
// config marks c as one to ask about (contexts.<name>.confirm_switch) — a
// production cluster, typically, where a stray Enter in the sidebar should not
// be enough to put every following keystroke on it.
func (u *ui) activateContext(c dockerctx.Context) {
	if c.Name == u.activeCtx {
		u.flash(" [gray]already on[white] context " + c.Name)
		return
	}
	// Docker's stored current context is written (with ui.sync_docker_context
	// only) by the switch, once it has actually happened — not here. Writing it
	// first would leave `docker` pointed at a cluster the UI just declined to go
	// to, and the footer and the shell would disagree.
	if !u.switching && u.cfg.ConfirmSwitch(c.Name) {
		msg := fmt.Sprintf("Switch to context %q?\n%s\n\n(contexts.%s.confirm_switch is set)", c.Name, orDash(c.Host), c.Name)
		u.confirm(msg, "Switch", u.cxtable, func() { u.switchCluster(c.Name) })
		return
	}
	u.switchCluster(c.Name)
}

func (u *ui) showContextDetail(c dockerctx.Context) {
	km, cxtable := u.km, u.cxtable
	active := "no"
	if c.Name == u.activeCtx {
		active = "yes (this session)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "name:         %s\n", tview.Escape(c.Name))
	fmt.Fprintf(&b, "docker host:  %s\n", tview.Escape(orDash(c.Host)))
	fmt.Fprintf(&b, "active:       %s", active)
	if c.Current {
		b.WriteString("\n\n[gray]stored current docker context[-]")
	}
	tv := tview.NewTextView().SetDynamicColors(true).SetText(b.String())
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" context %s ", c.Name))
	ov := u.overlayFor(pageCtxDetail, cxtable, footerKeys(keyLabel(km.CtxUse), "use", "Esc", "back"))
	closeDetail := ov.Close
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == u.km.Quit || ev.Rune() == 'i')):
			closeDetail()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == km.CtxUse:
			closeDetail()
			u.activateContext(c)
			return nil
		}
		return ev
	})
	ov.show(centered(tv, 64, 8), tv)
}

// contextKeys is the key handler of the context sidebar.
//
// The context sidebar's keys: Enter/u switch cluster, i details, n creates,
// d removes, Esc hands the keyboard back to the tab you were on.
func (u *ui) contextKeys(ev *tcell.EventKey) *tcell.EventKey {
	km := u.km
	if ev.Key() == tcell.KeyEscape {
		u.blurSidebar()
		return nil
	}
	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case 'i':
			if c, ok := u.selectedContext(); ok {
				u.showContextDetail(c)
			}
			return nil
		case km.CtxNew:
			u.showCreateContext()
			return nil
		case km.CtxDelete:
			if c, ok := u.selectedContext(); ok {
				u.deleteContext(c)
			}
			return nil
		case km.CtxUse:
			if c, ok := u.selectedContext(); ok {
				u.activateContext(c)
			}
			return nil
		}
	}
	return u.tabKeys(ev)
}
