// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"
	"swarmexec/client/internal/clientlog"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) renderNetworks() {
	nettable := u.nettable
	selName := ""
	if row, _ := nettable.GetSelection(); row >= 1 {
		if c := nettable.GetCell(row, 0); c != nil {
			selName = c.Text
		}
	}
	tableHeaders(nettable, nHeaders)
	if len(u.nets) == 0 {
		nettable.SetCell(1, 0, emptyCell(emptyText("networks", keyHint(u.km.NetNew, "to create one"))))
		return
	}
	selRow := 1
	for i, n := range u.nets {
		svcCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if len(n.Services) > 0 {
			svcCell = tview.NewTableCell(fmt.Sprintf("%d", len(n.Services))).SetTextColor(tcell.ColorGreen).SetExpansion(1)
		}
		typeColor := networkTypeColor(n)
		encCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
		if n.Encrypted {
			encCell = tview.NewTableCell("🔒 yes").SetTextColor(tcell.ColorGreen).SetExpansion(1)
		}
		nettable.SetCell(i+1, 0, tview.NewTableCell(n.Name).SetTextColor(typeColor).SetExpansion(1))
		nettable.SetCell(i+1, 1, tview.NewTableCell(orDash(n.Driver)).SetExpansion(1))
		nettable.SetCell(i+1, 2, tview.NewTableCell(orDash(n.Scope)).SetExpansion(1))
		nettable.SetCell(i+1, 3, tview.NewTableCell(networkType(n)).SetTextColor(typeColor).SetExpansion(1))
		nettable.SetCell(i+1, 4, encCell)
		nettable.SetCell(i+1, 5, svcCell)
		nettable.SetCell(i+1, 6, tview.NewTableCell(volumeAge(n.Created)).SetExpansion(1))
		if n.Name == selName {
			selRow = i + 1
		}
	}
	if len(u.nets) > 0 {
		nettable.Select(selRow, 0)
	}
}

func (u *ui) loadNetworks() {
	dcli, ctx, nettable := u.dcli, u.ctx, u.nettable
	tableState(nettable, nHeaders, loadingCell())
	gen := u.generation()
	go func() {
		start := time.Now()
		list, err := listNetworks(ctx, dcli)
		clientlog.Timed("ui.loadNetworks", start, err)
		u.onCluster(gen, func() {
			if err != nil {
				tableState(nettable, nHeaders, errorCell(err))
				return
			}
			u.nets = list
			u.renderNetworks()
		})
	}()
}

func (u *ui) selectedNetwork() (swarmNetwork, bool) {
	nettable := u.nettable
	return selectedRow(nettable, u.nets)
}

func (u *ui) serviceNamesFromCache() []string {
	names := make([]string, 0, len(u.lastSvcs))
	for _, s := range u.lastSvcs {
		names = append(names, s.Name)
	}
	return names
}

func (u *ui) servicePrompt(title, confirmVerb, actionLabel string, suggestions []string, back tview.Primitive, do func(string) error, onDone func()) {
	app := u.app
	in := tview.NewInputField().SetLabel("service: ").SetFieldWidth(46)
	in.SetPlaceholder("type or ↓ to pick; Enter confirms, Esc cancels")
	in.SetAutocompleteFunc(func(text string) []string {
		text = strings.ToLower(strings.TrimSpace(text))
		var out []string
		for _, s := range suggestions {
			if text == "" || strings.Contains(strings.ToLower(s), text) {
				out = append(out, s)
			}
		}
		return out
	})
	ov := u.overlayFor(pageSvcPrompt, back, "")
	in.SetDoneFunc(func(key tcell.Key) {
		name := strings.TrimSpace(in.GetText())
		if key != tcell.KeyEnter || name == "" {
			ov.Close()
			return
		}
		// Closed without restoring focus: the confirm below takes it, and
		// bouncing focus off `back` on the way would flicker the selection.
		ov.Close()
		u.confirm(fmt.Sprintf("%s %q?\n\nThis triggers a rolling update of the service.", confirmVerb, name), actionLabel, back, func() {
			go func() {
				err := do(name)
				app.QueueUpdateDraw(func() {
					if err != nil {
						u.info(strings.ToLower(actionLabel) + " failed: " + err.Error())
						return
					}
					onDone()
					u.info(fmt.Sprintf("%q updated — rolling update started", name))
				})
			}()
		})
	})
	in.SetBorder(true).SetTitle(" " + title + " ")
	ov.show(centeredPrompt(in, 66), in)
}

// netMembersView is the "attached services" overlay for one network: a
// collapsible list of services (with their containers and aliases) plus attach/
// detach/alias actions. Its state was formerly locals shared by the closures
// inside showNetworkMembers; promoting them onto a struct splits the render
// (build) half from handleKey (input), mirroring inspectView.
type netMembersView struct {
	u        *ui
	n        swarmNetwork
	list     *tview.List
	members  []netService
	rowSvc   []string // list row -> service it belongs to (for Enter/A)
	expanded map[string]bool
	loaded   bool

	ov *overlay
}

func (u *ui) showNetworkMembers(n swarmNetwork) {
	list := tview.NewList().ShowSecondaryText(false)
	badge := ""
	if n.Encrypted {
		badge += " · 🔒 encrypted"
	}
	if n.MTU != "" {
		badge += " · mtu " + n.MTU
	}
	list.SetBorder(true).SetTitle(fmt.Sprintf(" %s%s — attached services ", n.Name, badge))
	v := &netMembersView{u: u, n: n, list: list, expanded: map[string]bool{}}
	// Seed with the services already known from the list; containers/aliases
	// fill in once the task/spec lookup returns.
	v.members = make([]netService, 0, len(n.Services))
	for _, s := range n.Services {
		v.members = append(v.members, netService{Name: s})
	}
	v.open()
}

// render rebuilds the list from the current members/expanded state and the
// row→service map — the view half of the overlay.
func (v *netMembersView) render() {
	list, n := v.list, v.n
	cur := list.GetCurrentItem()
	list.Clear()
	v.rowSvc = v.rowSvc[:0]
	meta := func(svc string) { v.rowSvc = append(v.rowSvc, svc) }
	// The network's own labels (read-only) at the top — Docker has no
	// network-update API, so they can't be edited here.
	if lbls := kvPairs(n.Labels); len(lbls) > 0 {
		list.AddItem("[gray]labels[-]", "", 0, nil)
		meta("")
		for _, l := range lbls {
			list.AddItem("    "+tview.Escape(l), "", 0, nil)
			meta("")
		}
		list.AddItem("", "", 0, nil)
		meta("")
	}
	if len(v.members) == 0 {
		if v.loaded {
			list.AddItem("(no services attached)", "", 0, nil)
		} else {
			list.AddItem(loadingText, "", 0, nil)
		}
		meta("")
	}
	// Pad the id/node columns to the widest across every service so the
	// node and IP columns line up down the whole list, not just per row.
	idW, nodeW := 0, 0
	for _, s := range v.members {
		for _, c := range s.Containers {
			if w := len(c.ID); w > idW {
				idW = w
			}
			if w := len(orDash(c.Node)); w > nodeW {
				nodeW = w
			}
		}
	}
	for _, s := range v.members {
		head := tview.Escape(s.Name) + " …"
		if v.loaded {
			head = fmt.Sprintf("%s (%d)", tview.Escape(s.Name), len(s.Containers))
			switch {
			case len(s.Aliases) == 0:
				head += "  [gray]no aliases[-]"
			case v.expanded[s.Name]:
				head += fmt.Sprintf("  [aqua]- %d aliases[-]", len(s.Aliases))
			default:
				head += fmt.Sprintf("  [aqua]+ %d aliases[-]", len(s.Aliases))
			}
		}
		list.AddItem(head, "", 0, nil)
		meta(s.Name)
		if v.loaded && v.expanded[s.Name] {
			for _, a := range s.Aliases {
				list.AddItem("        [gray]alias:[-] "+tview.Escape(a), "", 0, nil)
				meta(s.Name)
			}
		}
		for _, c := range s.Containers {
			list.AddItem(fmt.Sprintf("    %-*s  %-*s  %s", idW, c.ID, nodeW, orDash(c.Node), orDash(c.IPv4)), "", 0, nil)
			meta(s.Name)
		}
	}
	if cur < list.GetItemCount() {
		list.SetCurrentItem(cur)
	}
}

func (v *netMembersView) close() { v.ov.Close() }

func (v *netMembersView) reload() {
	u := v.u
	go func() {
		m := networkMembers(u.ctx, u.dcli, v.n)
		u.app.QueueUpdateDraw(func() {
			// Only repaint if this overlay is still the one on screen.
			if u.pages.HasPage(pageNetMembers) {
				v.members, v.loaded = m, true
				v.render()
			}
		})
	}()
}

func (v *netMembersView) curSvc() string {
	i := v.list.GetCurrentItem()
	if i < 0 || i >= len(v.rowSvc) {
		return ""
	}
	return v.rowSvc[i]
}

// handleKey is the list's input capture — the input half of the overlay.
func (v *netMembersView) handleKey(ev *tcell.EventKey) *tcell.EventKey {
	u, n, list := v.u, v.n, v.list
	switch {
	case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == u.km.Quit || ev.Rune() == 'i')):
		v.close()
		return nil
	case ev.Key() == tcell.KeyEnter:
		// Toggle the current row's service, so Enter anywhere in a service's
		// block expands/collapses its aliases.
		if svc := v.curSvc(); svc != "" {
			v.expanded[svc] = !v.expanded[svc]
			v.render()
		}
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'A':
		svc := v.curSvc()
		if svc == "" {
			return nil
		}
		if !v.loaded {
			u.info("still loading — try again in a moment")
			return nil
		}
		// Same staged alias editor as the inspect view; applies via
		// setNetworkAliases, then reloads this view to show the new aliases.
		u.openAliasEditorForNet(svc, n.Name, n.ID, list, v.reload)
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
		suggestions := servicesExcluding(u.serviceNamesFromCache(), n.Services)
		u.servicePrompt(
			fmt.Sprintf("attach a service to network %q", n.Name),
			fmt.Sprintf("Attach network %q to service", n.Name), "Attach",
			suggestions, list,
			func(svc string) error { return attachServiceToNetwork(u.ctx, u.dcli, svc, n.ID, n.Name) },
			func() { v.close(); u.loadNetworks() },
		)
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
		if len(n.Services) == 0 {
			u.info("no services are attached to this network")
			return nil
		}
		u.servicePrompt(
			fmt.Sprintf("detach a service from network %q", n.Name),
			fmt.Sprintf("Detach network %q from service", n.Name), "Detach",
			n.Services, list,
			func(svc string) error { return detachServiceFromNetwork(u.ctx, u.dcli, svc, n.ID, n.Name) },
			func() { v.close(); u.loadNetworks() },
		)
		return nil
	}
	return vimListKeys(ev)
}

// open wires the list to its input/footer and kicks off the initial fetch.
func (v *netMembersView) open() {
	u, n, list := v.u, v.n, v.list
	v.render()
	v.ov = u.overlayFor(pageNetMembers, u.nettable, footerKeys("a", "attach", "d", "detach", "Enter", "aliases", "A", "add alias", "j/k", "move", "Esc", "back"))
	list.SetInputCapture(v.handleKey)
	height := len(n.Services) + 6
	if height > 22 {
		height = 22
	}
	v.ov.show(centered(list, 78, height), list)
	v.reload()
}

func (u *ui) showCreateNetwork() {
	dcli, ctx, nettable := u.dcli, u.ctx, u.nettable
	o := newNetworkOpts{Driver: "overlay"}
	var labels string
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" new network ")
	form.AddInputField("Name", "", 32, nil, func(t string) { o.Name = t })
	form.AddInputField("Driver", "overlay", 20, nil, func(t string) { o.Driver = t })
	form.AddCheckbox("Attachable (standalone containers may join)", false, func(c bool) { o.Attachable = c })
	form.AddCheckbox("Encrypted (overlay data-plane encryption)", false, func(c bool) { o.Encrypted = c })
	form.AddCheckbox("Internal (no external routing)", false, func(c bool) { o.Internal = c })
	form.AddCheckbox("Enable IPv6", false, func(c bool) { o.IPv6 = c })
	form.AddInputField("MTU (optional)", "", 8, tview.InputFieldInteger, func(t string) { o.MTU = t })
	form.AddInputField("Subnet (optional, e.g. 10.10.0.0/24)", "", 22, nil, func(t string) { o.Subnet = t })
	form.AddInputField("Gateway (optional)", "", 22, nil, func(t string) { o.Gateway = t })
	form.AddInputField("Labels (optional, k=v,k=v)", "", 40, nil, func(t string) { labels = t })
	ov := u.overlayFor(pageNetForm, nettable, footerKeys("Tab", "next field", "Enter", "confirm", "Esc", "cancel"))
	closeForm := ov.Close
	form.AddButton("Create", func() {
		lbls, err := parseKVList(labels)
		if err != nil {
			u.info("invalid labels: " + err.Error())
			return
		}
		o.Labels = lbls
		if err := createNetwork(ctx, dcli, o); err != nil {
			u.info("create failed: " + err.Error())
			return
		}
		name := strings.TrimSpace(o.Name)
		closeForm()
		u.loadNetworks()
		u.flash(" [green]created[white] network " + name)
	})
	form.AddButton("Cancel", closeForm)
	form.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			closeForm()
			return nil
		}
		return ev
	})
	ov.show(centered(form, formWidth, 22), form)
}
