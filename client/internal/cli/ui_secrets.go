// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"sort"
	"strings"
	"swarmexec/client/internal/clientlog"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) renderSecrets() {
	sectable := u.sectable
	selName := ""
	if row, _ := sectable.GetSelection(); row >= 1 {
		if c := sectable.GetCell(row, 0); c != nil {
			selName = c.Text
		}
	}
	tableHeaders(sectable, sHeaders)
	if len(u.secs) == 0 {
		sectable.SetCell(1, 0, emptyCell(emptyText("secrets", keyHint(u.km.SecNew, "to create one"))))
		return
	}
	selRow := 1
	for i, s := range u.secs {
		usedCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if len(s.Services) > 0 {
			usedCell = tview.NewTableCell(fmt.Sprintf("%d", len(s.Services))).SetTextColor(tcell.ColorGreen).SetExpansion(1)
		}
		labels := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if len(s.Labels) > 0 {
			labels = tview.NewTableCell(fmt.Sprintf("%d", len(s.Labels))).SetExpansion(1)
		}
		sectable.SetCell(i+1, 0, tview.NewTableCell(s.Name).SetExpansion(1))
		sectable.SetCell(i+1, 1, usedCell)
		sectable.SetCell(i+1, 2, tview.NewTableCell(volumeAge(s.Created)).SetExpansion(1))
		sectable.SetCell(i+1, 3, tview.NewTableCell(volumeAge(s.Updated)).SetExpansion(1))
		sectable.SetCell(i+1, 4, labels)
		if s.Name == selName {
			selRow = i + 1
		}
	}
	if len(u.secs) > 0 {
		sectable.Select(selRow, 0)
	}
}

func (u *ui) loadSecrets() {
	dcli, ctx, sectable := u.dcli, u.ctx, u.sectable
	tableState(sectable, sHeaders, loadingCell())
	gen := u.generation()
	go func() {
		start := time.Now()
		list, err := listSecrets(ctx, dcli)
		clientlog.Timed("ui.loadSecrets", start, err)
		u.onCluster(gen, func() {
			if err != nil {
				tableState(sectable, sHeaders, errorCell(err))
				return
			}
			u.secs = list
			u.renderSecrets()
		})
	}()
}

func (u *ui) selectedSecret() (swarmSecret, bool) {
	sectable := u.sectable
	return selectedRow(sectable, u.secs)
}

func (u *ui) showSecretDetail(s swarmSecret) {
	app, pages, dcli, ctx, sectable := u.app, u.pages, u.dcli, u.ctx, u.sectable
	ts := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04:05")
	}
	tv := tview.NewTextView().SetDynamicColors(true)
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" secret %s ", s.Name))
	render := func(members []netService, loading bool) {
		var b strings.Builder
		fmt.Fprintf(&b, "Name:     %s\n", s.Name)
		fmt.Fprintf(&b, "ID:       %s\n", s.ID)
		fmt.Fprintf(&b, "Created:  %s\n", ts(s.Created))
		fmt.Fprintf(&b, "Updated:  %s\n", ts(s.Updated))
		if len(s.Labels) == 0 {
			fmt.Fprintf(&b, "Labels:   -\n")
		} else {
			fmt.Fprintf(&b, "Labels:\n")
			keys := make([]string, 0, len(s.Labels))
			for k := range s.Labels {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(&b, "  %s=%s\n", k, s.Labels[k])
			}
		}
		fmt.Fprintf(&b, "\nused by:\n")
		if len(members) == 0 {
			if loading {
				fmt.Fprintf(&b, "  …\n")
			} else {
				fmt.Fprintf(&b, "  (no services)\n")
			}
		}
		for _, m := range members {
			head := m.Name + " …"
			if !loading {
				head = fmt.Sprintf("%s (%d)", m.Name, len(m.Containers))
			}
			fmt.Fprintf(&b, "  %s\n", head)
			for _, c := range m.Containers {
				fmt.Fprintf(&b, "      %s  %s\n", c.ID, orDash(c.Node))
			}
		}
		fmt.Fprintf(&b, "\n[gray]the secret value is not retrievable via the Docker API[white]")
		tv.SetText(b.String())
	}
	// Seed with the service names already known from the list; containers pending.
	init := make([]netService, 0, len(s.Services))
	for _, name := range s.Services {
		init = append(init, netService{Name: name})
	}
	render(init, true)
	ov := u.overlayFor(pageSecDetail, sectable, footerKeys("a", "attach", "d", "detach", "j/k", "scroll", "Esc", "back"))
	closeSecret := ov.Close
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == u.km.Quit || ev.Rune() == 'i')):
			closeSecret()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
			suggestions := servicesExcluding(u.serviceNamesFromCache(), s.Services)
			u.servicePrompt(
				fmt.Sprintf("attach secret %q to a service", s.Name),
				fmt.Sprintf("Attach secret %q to service", s.Name), "Attach",
				suggestions, tv,
				func(svc string) error { return attachSecretToService(ctx, dcli, svc, s.ID, s.Name) },
				func() { closeSecret(); u.loadSecrets() },
			)
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
			if len(s.Services) == 0 {
				u.info("no services use this secret")
				return nil
			}
			u.servicePrompt(
				fmt.Sprintf("detach secret %q from a service", s.Name),
				fmt.Sprintf("Detach secret %q from service", s.Name), "Detach",
				s.Services, tv,
				func(svc string) error { return detachSecretFromService(ctx, dcli, svc, s.ID, s.Name) },
				func() { closeSecret(); u.loadSecrets() },
			)
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	})
	height := 11 + len(s.Labels) + len(s.Services)
	if height > 24 {
		height = 24
	}
	ov.show(centered(tv, 72, height), tv)
	go func() {
		members := secretMembers(ctx, dcli, s)
		app.QueueUpdateDraw(func() {
			if pages.HasPage(pageSecDetail) {
				render(members, false)
			}
		})
	}()
}

// openDeleteSecret permanently removes a secret after a confirm. Docker refuses
// to remove a secret a service still references, so warn up front when in use.
func (u *ui) openDeleteSecret(s swarmSecret) {
	app, dcli, ctx, sectable := u.app, u.dcli, u.ctx, u.sectable
	msg := fmt.Sprintf("Remove secret %q?\n\nThis permanently deletes the secret; it cannot be undone.", s.Name)
	if len(s.Services) > 0 {
		msg += fmt.Sprintf("\n\n⚠ Still referenced by %d service(s): %s\nDocker will refuse to remove a secret in use — detach it from those services first.", len(s.Services), strings.Join(s.Services, ", "))
	}
	u.confirm(msg, "Delete", sectable, func() {
		go func() {
			err := removeSecret(ctx, dcli, s.Name)
			app.QueueUpdateDraw(func() {
				if err != nil {
					u.info("remove failed: " + err.Error())
					app.SetFocus(sectable)
					return
				}
				u.loadSecrets()
				u.flash(" [green]removed[white] secret " + s.Name)
			})
		}()
	})
}

// showCreateSecret creates a new swarm secret from a name, a (multi-line)
// value and optional labels. The value is entered in a text area so certs and
// keys can be pasted as-is.
func (u *ui) showCreateSecret() {
	dcli, ctx, sectable := u.dcli, u.ctx, u.sectable
	var name, value, labels string
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" new secret ")
	form.AddInputField("Name", "", 32, nil, func(t string) { name = t })
	form.AddTextArea("Value", "", 40, 6, 0, func(t string) { value = t })
	form.AddInputField("Labels (optional, k=v,k=v)", "", 40, nil, func(t string) { labels = t })
	ov := u.overlayFor(pageSecForm, sectable, footerKeys("Tab", "next field", "Esc", "cancel"))
	closeForm := ov.Close
	form.AddButton("Create", func() {
		lbls, err := parseKVList(labels)
		if err != nil {
			u.info("invalid labels: " + err.Error())
			return
		}
		nm := strings.TrimSpace(name)
		if err := createSecret(ctx, dcli, nm, []byte(value), lbls); err != nil {
			u.info("create failed: " + err.Error())
			return
		}
		closeForm()
		u.loadSecrets()
		u.flash(" [green]created[white] secret " + nm)
	})
	form.AddButton("Cancel", closeForm)
	form.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			closeForm()
			return nil
		}
		return ev
	})
	ov.show(centered(form, formWidth, 18), form)
}

// secretKeys is the key handler of the Secrets table.
func (u *ui) secretKeys(ev *tcell.EventKey) *tcell.EventKey {
	km := u.km
	if ev.Key() == tcell.KeyRune && ev.Rune() == km.SecNew {
		u.showCreateSecret()
		return nil
	}
	if ev.Key() == tcell.KeyRune && ev.Rune() == km.SecDelete {
		if s, ok := u.selectedSecret(); ok {
			u.openDeleteSecret(s)
		}
		return nil
	}
	// "i" opens the same detail as Enter — inspect means the same on every tab.
	if ev.Key() == tcell.KeyRune && ev.Rune() == 'i' {
		if s, ok := u.selectedSecret(); ok {
			u.showSecretDetail(s)
		}
		return nil
	}
	return u.tabKeys(ev)
}
