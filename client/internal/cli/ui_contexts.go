// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
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
	app, ctx, cxtable := u.app, u.ctx, u.cxtable
	var (
		name, desc                      string
		useSSH                          = true
		useJump                         bool
		sshUser, sshHost, sshPort, jump string
		plainHost                       string
	)
	assembleHost := func() (string, error) {
		if useSSH {
			return dockerctx.BuildSSHHost(sshUser, sshHost, sshPort)
		}
		h := strings.TrimSpace(plainHost)
		if h == "" {
			return "", fmt.Errorf("docker host is required (tcp:// or unix://)")
		}
		return h, nil
	}
	proxyJump := func() string {
		if useSSH && useJump {
			return strings.TrimSpace(jump)
		}
		return ""
	}
	form := tview.NewForm()
	form.SetItemPadding(1) // one blank row between fields (the overlay has ample height)
	form.SetBorder(true).SetTitle(" new context ")
	ov := u.overlayFor(pageCtxForm, cxtable, footerKeys("Tab", "move", "Space", "toggle", "Enter", "confirm", "Esc", "cancel"))
	closeForm := ov.Close

	var render func()
	render = func() {
		form.Clear(true)
		form.AddInputField("Name", name, 32, nil, func(t string) { name = t })
		form.AddCheckbox("Connect to Docker over SSH", useSSH, func(c bool) {
			if c != useSSH {
				useSSH = c
				render()
				app.SetFocus(form) // form was rebuilt; restore focus to it
			}
		})
		if useSSH {
			form.AddInputField("SSH user", sshUser, 20, nil, func(t string) { sshUser = t })
			form.AddInputField("SSH host", sshHost, 28, nil, func(t string) { sshHost = t })
			form.AddInputField("SSH port (22)", sshPort, 8, nil, func(t string) { sshPort = t })
			form.AddCheckbox("Use a jump host (bastion)", useJump, func(c bool) {
				if c != useJump {
					useJump = c
					render()
					app.SetFocus(form)
				}
			})
			if useJump {
				form.AddInputField("Jump host(s)", jump, 34, nil, func(t string) { jump = t })
				if hf, ok := form.GetFormItem(form.GetFormItemCount() - 1).(*tview.InputField); ok {
					hf.SetPlaceholder("bastion  (or edge,bastion for multi-hop)")
				}
			}
		} else {
			form.AddInputField("Docker host", plainHost, 34, nil, func(t string) { plainHost = t })
			if hf, ok := form.GetFormItem(form.GetFormItemCount() - 1).(*tview.InputField); ok {
				hf.SetPlaceholder("tcp://host:2376 | unix:///var/run/docker.sock")
			}
		}
		form.AddInputField("Description", desc, 34, nil, func(t string) { desc = t })
		form.AddButton("Test", func() {
			host, err := assembleHost()
			if err != nil {
				u.info(err.Error())
				return
			}
			pj := proxyJump()
			u.flash(" [gray]testing " + host + " …[white]")
			go func() {
				terr := pingDockerHost(ctx, host, pj)
				app.QueueUpdateDraw(func() {
					if terr != nil {
						u.info("test failed: " + terr.Error())
						return
					}
					u.flash(" [green]✓ connection ok[white] — " + host)
				})
			}()
		})
		form.AddButton("Create", func() {
			host, err := assembleHost()
			if err != nil {
				u.info(err.Error())
				return
			}
			if err := dockerctx.Create(dockerctx.CreateOptions{Name: strings.TrimSpace(name), Host: host, Description: strings.TrimSpace(desc), ProxyJump: proxyJump()}); err != nil {
				u.info("create failed: " + err.Error())
				return
			}
			closeForm()
			u.loadContexts()
			u.flash(" [green]created[white] context " + strings.TrimSpace(name))
		})
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
		u.loadContexts()
		u.flash(" [green]removed[white] context " + c.Name)
	})
}

// activateContext makes c the current docker context and restarts the UI so
// it reconnects to that cluster. Restarting (rather than swapping the client
// live) avoids racing the in-flight background loads.
func (u *ui) activateContext(c dockerctx.Context) {
	if c.Name == u.activeCtx {
		u.flash(" [gray]already on[white] context " + c.Name)
		return
	}
	// Docker's stored current context is updated by the switch, once it has
	// actually happened — not here. It used to be written first, which was
	// harmless only because a failure then took the whole UI down with it. Now
	// that a refused switch leaves the operator where they were, writing it
	// first would leave `docker` pointed at a cluster the UI just declined to
	// go to, and the footer and the shell would disagree.
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
	ov := u.overlayFor(pageCtxDetail, cxtable, footerKeys(keyLabel(km.CtxUse), "use", "Esc", "close"))
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
