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
	for i, c := range u.ctxs {
		label, color := "  "+c.Name, tcell.ColorWhite
		if c.Name == u.activeCtx {
			label, color = "▶ "+c.Name, tcell.ColorAqua // the active session context
		}
		cxtable.SetCell(i+1, 0, tview.NewTableCell(label).SetTextColor(color).SetExpansion(1))
		cxtable.SetCell(i+1, 1, tview.NewTableCell(orDash(c.Host)).SetTextColor(tcell.ColorGray).SetExpansion(2))
		if c.Name == selName {
			selRow = i + 1
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
		for c, h := range cxHeaders {
			cxtable.SetCell(0, c, headerCell(h))
		}
		cxtable.SetCell(1, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
		return
	}
	u.ctxs = list
	u.renderContexts()
}

func (u *ui) selectedContext() (dockerctx.Context, bool) {
	cxtable := u.cxtable
	return selectedRow(cxtable, u.ctxs)
}

func (u *ui) showCreateContext() {
	app, pages, ctx, cxtable := u.app, u.pages, u.ctx, u.cxtable
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
	_, restoreHelp := u.pushOverlayHelp(footerKeys("Tab", "move", "Space", "toggle", "Enter", "confirm", "Esc", "cancel"))
	closeForm := func() { restoreHelp(); pages.RemovePage(pageCtxForm); app.SetFocus(cxtable) }

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
	pages.AddPage(pageCtxForm, centered(form, 76, 22), true, true)
	app.SetFocus(form)
}

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

func (u *ui) activateContext(c dockerctx.Context) {
	app := u.app
	if c.Name == u.activeCtx {
		u.flash(" [gray]already on[white] context " + c.Name)
		return
	}
	if err := dockerctx.Use(c.Name); err != nil {
		u.info("switch failed: " + err.Error())
		return
	}
	u.switchTo = c.Name
	app.Stop() // the caller restarts against switchTo
}

func (u *ui) showContextDetail(c dockerctx.Context) {
	app, pages, km, cxtable := u.app, u.pages, u.km, u.cxtable
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
	_, restore := u.pushOverlayHelp(footerKeys(keyLabel(km.CtxUse), "use", "Esc", "close"))
	closeDetail := func() { restore(); pages.RemovePage(pageCtxDetail); app.SetFocus(cxtable) }
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')):
			closeDetail()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == km.CtxUse:
			closeDetail()
			u.activateContext(c)
			return nil
		}
		return ev
	})
	pages.AddPage(pageCtxDetail, centered(tv, 64, 8), true, true)
	app.SetFocus(tv)
}
