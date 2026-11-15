// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strings"

	"swarmexec/client/internal/swarmops"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// networksTab is the Networks tab: the swarm's networks with the services
// attached to each, and the create / attach / detach actions.
type networksTab struct {
	*listTab[swarmops.Network]
}

func newNetworksTab(u *ui) *networksTab {
	t := &networksTab{newListTab(u, nHeaders, func(n swarmops.Network) string { return n.Name })}
	t.row = t.renderRow
	t.empty = func() string { return emptyText("networks", keyHint(u.km.NetNew, "to create one")) }
	t.table.SetSelectedFunc(func(int, int) {
		if n, ok := t.selected(); ok {
			t.showMembers(n)
		}
	})
	t.table.SetInputCapture(t.keys)
	return t
}

// netTab is the session's Networks tab.
func (u *ui) netTab() *networksTab { return u.tab("networks").(*networksTab) }

func (t *networksTab) enter()   { t.reload() }
func (t *networksTab) refresh() { t.reload() }

// reload fetches the network list.
func (t *networksTab) reload() {
	dcli := t.u.dcli
	t.load("ui.loadNetworks", func(ctx context.Context) ([]swarmops.Network, error) { return swarmops.ListNetworks(ctx, dcli) })
}

func (t *networksTab) footer() string {
	km := t.u.km
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/%s[white] attached  [yellow]%s[white] new", keyLabel(km.NetAttached), keyLabel(km.NetNew))
}

func (t *networksTab) helpSection(h *helpWriter) {
	km := t.u.km
	h.sec("Networks")
	h.line("Enter/"+keyLabel(km.NetAttached), "attached services")
	h.line(keyLabel(km.NetNew), "new network")
}

func (t *networksTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "NETWORK\tDRIVER\tSCOPE\tTYPE\tSERVICES\tAGE")
	for _, n := range t.rows() {
		fmt.Fprintf(b, "%s\t%s\t%s\t%s\t%d\t%s\n",
			n.Name, orDash(n.Driver), orDash(n.Scope), networkType(n), len(n.Services), volumeAge(n.Created))
	}
}

func (t *networksTab) renderRow(r int, n swarmops.Network) {
	svcCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
	if len(n.Services) > 0 {
		svcCell = tview.NewTableCell(fmt.Sprintf("%d", len(n.Services))).SetTextColor(tcell.ColorGreen).SetExpansion(1)
	}
	typeColor := networkTypeColor(n)
	encCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
	if n.Encrypted {
		encCell = tview.NewTableCell("🔒 yes").SetTextColor(tcell.ColorGreen).SetExpansion(1)
	}
	t.table.SetCell(r, 0, tview.NewTableCell(n.Name).SetTextColor(typeColor).SetExpansion(1))
	t.table.SetCell(r, 1, tview.NewTableCell(orDash(n.Driver)).SetExpansion(1))
	t.table.SetCell(r, 2, tview.NewTableCell(orDash(n.Scope)).SetExpansion(1))
	t.table.SetCell(r, 3, tview.NewTableCell(networkType(n)).SetTextColor(typeColor).SetExpansion(1))
	t.table.SetCell(r, 4, encCell)
	t.table.SetCell(r, 5, svcCell)
	t.table.SetCell(r, 6, tview.NewTableCell(volumeAge(n.Created)).SetExpansion(1))
}

// serviceNamesFromCache returns the known service names (the containers-tab
// cache), used to seed the attach autocomplete. It is only a suggestion list —
// the actual attach resolves the name live, so a just-created service that is
// not cached yet can still be typed in.
func (u *ui) serviceNamesFromCache() []string {
	svcs := u.treeTab().st().svcs
	names := make([]string, 0, len(svcs))
	for _, s := range svcs {
		names = append(names, s.Name)
	}
	return names
}

// servicePrompt opens an autocomplete input to choose a service, then a
// confirmation (the change triggers a rolling update of that service), then
// runs do(service) off the UI goroutine and calls onDone on success. It backs
// both attach and detach: actionLabel is the confirm button ("Attach" /
// "Detach"), confirmVerb the sentence lead-in, suggestions feed the
// autocomplete only, and back gets focus when the operator cancels.
func (u *ui) servicePrompt(title, confirmVerb, actionLabel string, suggestions []string, back tview.Primitive, do func(ctx context.Context, svc string) error, onDone func()) {
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
			asyncDo(u, "ui.servicePrompt."+strings.ToLower(actionLabel), func(ctx context.Context) error {
				return do(ctx, name)
			}, func(err error) {
				if err != nil {
					u.info(strings.ToLower(actionLabel) + " failed: " + err.Error())
					return
				}
				onDone()
				u.info(fmt.Sprintf("%q updated — rolling update started", name))
			})
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
	n        swarmops.Network
	list     *tview.List
	members  []swarmops.NetService
	rowSvc   []string // list row -> service it belongs to (for Enter/A)
	expanded map[string]bool
	loaded   bool

	ov *overlay
}

// showMembers lists the services attached to a network with the containers of
// each service nested under it. Service membership is known synchronously
// (from the list); the per-service containers need a task lookup, so they fill
// in lazily after the overlay is up.
func (t *networksTab) showMembers(n swarmops.Network) {
	u := t.u
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
	v.members = make([]swarmops.NetService, 0, len(n.Services))
	for _, s := range n.Services {
		v.members = append(v.members, swarmops.NetService{Name: s})
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
	if lbls := swarmops.KVPairs(n.Labels); len(lbls) > 0 {
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
	u, dcli, n := v.u, v.u.dcli, v.n
	async(u, "ui.networks.members", func(ctx context.Context) ([]swarmops.NetService, error) {
		return swarmops.NetworkMembers(ctx, dcli, n), nil
	}, func(m []swarmops.NetService, _ error) {
		// Only repaint if this overlay is still the one on screen.
		if u.pages.HasPage(pageNetMembers) {
			v.members, v.loaded = m, true
			v.render()
		}
	})
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
	u, n, list, dcli := v.u, v.n, v.list, v.u.dcli
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
		// swarmops.SetNetworkAliases, then reloads this view to show the new aliases.
		u.openAliasEditorForNet(svc, n.Name, n.ID, list, v.reload)
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
		suggestions := swarmops.ServicesExcluding(u.serviceNamesFromCache(), n.Services)
		u.servicePrompt(
			fmt.Sprintf("attach a service to network %q", n.Name),
			fmt.Sprintf("Attach network %q to service", n.Name), "Attach",
			suggestions, list,
			func(ctx context.Context, svc string) error {
				return swarmops.AttachServiceToNetwork(ctx, dcli, svc, n.ID, n.Name)
			},
			func() { v.close(); u.netTab().reload() },
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
			func(ctx context.Context, svc string) error {
				return swarmops.DetachServiceFromNetwork(ctx, dcli, svc, n.ID, n.Name)
			},
			func() { v.close(); u.netTab().reload() },
		)
		return nil
	}
	return vimListKeys(ev)
}

// open wires the list to its input/footer and kicks off the initial fetch.
func (v *netMembersView) open() {
	u, n, list := v.u, v.n, v.list
	v.render()
	v.ov = u.overlayFor(pageNetMembers, u.netTab().table, footerKeys("a", "attach", "d", "detach", "Enter", "aliases", "A", "add alias", "j/k", "move", "Esc", "back"))
	list.SetInputCapture(v.handleKey)
	height := len(n.Services) + 6
	if height > 22 {
		height = 22
	}
	v.ov.show(centered(list, 78, height), list)
	v.reload()
}

// showCreate opens a form to create a network (default driver overlay) with
// the common swarm options — attachable, encrypted, internal, IPv6, MTU and an
// optional subnet/gateway and labels.
func (t *networksTab) showCreate() {
	u := t.u
	dcli, ctx, nettable := u.dcli, u.ctx, t.table
	o := swarmops.NewNetworkOpts{Driver: "overlay"}
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
		lbls, err := swarmops.ParseKVList(labels)
		if err != nil {
			u.info("invalid labels: " + err.Error())
			return
		}
		o.Labels = lbls
		if err := swarmops.CreateNetwork(ctx, dcli, o); err != nil {
			u.info("create failed: " + err.Error())
			return
		}
		name := strings.TrimSpace(o.Name)
		closeForm()
		t.reload()
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

// keys is the key handler of the Networks table.
//
// On the networks table, "i" (like the volumes tab) shows the attached
// services/containers; Enter does the same.
func (t *networksTab) keys(ev *tcell.EventKey) *tcell.EventKey {
	u, km := t.u, t.u.km
	if ev.Key() == tcell.KeyRune && ev.Rune() == km.NetAttached {
		if n, ok := t.selected(); ok {
			t.showMembers(n)
		}
		return nil
	}
	if ev.Key() == tcell.KeyRune && ev.Rune() == km.NetNew {
		t.showCreate()
		return nil
	}
	return u.tabKeys(ev)
}
