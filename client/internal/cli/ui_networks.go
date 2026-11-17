// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmapi"
	"swarmexec/client/internal/swarmops"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// networksTab is the Networks tab: the swarm's networks with the services
// attached to each, and the create / attach / detach actions.
type networksTab struct {
	*listTab[swarmops.Network]

	// ep is each cluster's endpoint analysis: the nodes' own view of every
	// overlay network (agent NetworkEndpoints) set against the manager's
	// tasks. It loads in the background next to the list and only adds to
	// the CONTAINERS cell and the detail overlay.
	ep perCluster[netEndpointsState]
	// members is the detail overlay while it is open, so an endpoint load
	// that finishes later can fill it in.
	members *netMembersView

	// all shows the manager's node-local networks too (bridge, host, none,
	// docker_gwbridge, a compose project's networks). Off by default: the
	// list reads as cluster-wide, and those exist on the manager node only —
	// every other node has its own set. A session preference, like a sort
	// order: a cluster switch keeps it.
	all bool
}

// netEndpointsState is one cluster's endpoint analysis as last loaded.
type netEndpointsState struct {
	loading time.Time // when the load in flight started; zero when none is
	loaded  bool
	byNet   map[string]*swarmops.NetEndpoints // by network ID
	failed  int                               // nodes that could not answer
	err     error                             // the whole load failed (node or task list)
}

// endpointsStuck is how long a load may be in flight before another one is
// allowed: one dropped by a cluster switch never reports back.
const endpointsStuck = time.Minute

func newNetworksTab(u *ui) *networksTab {
	t := &networksTab{listTab: newListTab(u, nHeaders, func(n swarmops.Network) string { return n.Name })}
	t.ep.init = func() *netEndpointsState { return &netEndpointsState{} }
	t.row = t.renderRow
	t.empty = func() string {
		if !t.all {
			return emptyText("swarm networks", keyHint(u.km.NetNew, "to create one, ")+keyHint(u.km.NetAll, "for the manager's local ones"))
		}
		return emptyText("networks", keyHint(u.km.NetNew, "to create one"))
	}
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

// reload fetches the network list, with the overlap analysis against the
// nodes' addresses and this context's configured underlay, and — on its own,
// as an agent fan-out — the nodes' endpoint views.
//
// Unless all is on, only swarm-scope networks are listed. They are filtered
// after the overlap analysis, so a swarm subnet that overlaps a local bridge
// keeps its marker with the bridge hidden.
func (t *networksTab) reload() {
	dcli, underlay, all := t.u.dcli, t.u.underlay(), t.all
	t.load("ui.loadNetworks", func(ctx context.Context) ([]swarmops.Network, error) {
		list, err := swarmops.ListNetworks(ctx, dcli, underlay)
		if err != nil || all {
			return list, err
		}
		return swarmScoped(list), nil
	})
	t.loadEndpoints()
}

// swarmScoped is the cluster-wide part of a network list: scope "swarm".
func swarmScoped(list []swarmops.Network) []swarmops.Network {
	out := make([]swarmops.Network, 0, len(list))
	for _, n := range list {
		if n.Scope == "swarm" {
			out = append(out, n)
		}
	}
	return out
}

// toggleAll switches between the swarm's networks and every network the
// manager knows, and says which is showing — the status line keeps saying it
// while the local ones are in.
func (t *networksTab) toggleAll() {
	t.all = !t.all
	t.reload()
	t.u.updateStatus()
	t.u.curHelp = t.u.helpFor("networks") // the footer names the other mode
	t.u.setFooter(t.u.curHelp)
	if t.all {
		t.u.flash(" [yellow]all networks[white] — the local ones (bridge, host, …) are the manager node's only")
		return
	}
	t.u.flash(" [green]swarm networks[white] only")
}

// statusNote says, while it applies, that local networks are listed (and
// whose they are), and what "n/a" in CONTAINERS means.
func (t *networksTab) statusNote() string {
	var parts []string
	if t.all {
		parts = append(parts, "[yellow]+ local networks[white] [gray]of the manager node only[white]")
	}
	if t.endpointsUnknown() {
		parts = append(parts, "[gray]n/a: node agent not reachable[white]")
	}
	return strings.Join(parts, "  ·  ")
}

// endpointsUnknown reports whether some node's endpoint view is missing, so
// some CONTAINERS cell carries "n/a".
func (t *networksTab) endpointsUnknown() bool {
	st := t.endpoints()
	if !st.loaded {
		return false
	}
	if st.err != nil || st.failed > 0 {
		return true
	}
	for _, e := range st.byNet {
		if e.Unknown > 0 {
			return true
		}
	}
	return false
}

// endpoints is the visible cluster's endpoint analysis.
func (t *networksTab) endpoints() *netEndpointsState { return t.ep.of(t.u.clusterState) }

// netEndpointsResult is what one endpoint load hands to the UI goroutine.
type netEndpointsResult struct {
	byNet  map[string]*swarmops.NetEndpoints
	failed int
}

// loadEndpoints asks every node's agent for its view of the overlay networks
// (one NetworkEndpoints call per node, all networks at once) and compares it
// with the manager's tasks. One load at a time per cluster; the table and an
// open detail overlay repaint when it lands.
func (t *networksTab) loadEndpoints() {
	u := t.u
	if u.r == nil || u.agents == nil || u.f == nil || u.dcli == nil {
		return // no agents to ask (a cluster not connected yet)
	}
	st := t.endpoints()
	if !st.loading.IsZero() && time.Since(st.loading) < endpointsStuck {
		return
	}
	st.loading = time.Now()
	r, ac, dcli, timeout := u.r, u.agents, u.dcli, u.f.connectTimeout
	async(u, "ui.networks.endpoints", func(ctx context.Context) (netEndpointsResult, error) {
		return fetchNetEndpoints(ctx, r, ac, dcli, timeout)
	}, func(res netEndpointsResult, err error) {
		t.endpointsLoaded(st, res, err)
	})
}

// fetchNetEndpoints is the load's off-loop half: every node's endpoint view,
// then the manager's tasks, analysed together.
func fetchNetEndpoints(ctx context.Context, r *resolve.Resolver, ac *agentcall.Client, dcli swarmapi.Client, timeout time.Duration) (netEndpointsResult, error) {
	nodes, err := r.Nodes(ctx)
	if err != nil {
		return netEndpointsResult{}, err
	}
	res := ac.NodesNetworkEndpoints(ctx, nodes, nil, timeout)
	// The manager's tasks are read after the nodes answered: a task
	// that started meanwhile is then known (no false stale); only one
	// that stopped meanwhile can still show, until the next refresh.
	tasks, err := swarmops.NetworkTaskAddrs(ctx, dcli)
	if err != nil {
		return netEndpointsResult{}, err
	}
	out := netEndpointsResult{byNet: swarmops.AnalyzeEndpoints(tasks, res)}
	for _, n := range res {
		if n.Err != nil {
			out.failed++
		}
	}
	return out, nil
}

// endpointsLoaded is the load's UI-goroutine half: store the result on st
// (the cluster it was started for) and repaint what shows it.
func (t *networksTab) endpointsLoaded(st *netEndpointsState, res netEndpointsResult, err error) {
	u := t.u
	st.loading = time.Time{}
	st.loaded, st.err = true, err
	if err == nil {
		st.byNet, st.failed = res.byNet, res.failed
	}
	if t.shownFor == u.clusterState {
		t.render()
	}
	if u.activeTab() == tabView(t) {
		u.updateStatus() // the "n/a" legend
	}
	if v := t.members; v != nil && u.pages.HasPage(pageNetMembers) {
		v.endpointsArrived()
	}
}

// endpointNote is what the endpoint analysis adds to the CONTAINERS cell —
// "· 14 addr", "⚠2 stale", "⚠1 dup", "n/a" when no node could answer and
// "+2 n/a" for nodes that could not (the status line says what n/a means; it
// used to be a bare "?") — and the colour the cell should take (0: keep its own). Only swarm-scope
// networks have one: the agents report overlay networks only.
func (t *networksTab) endpointNote(n swarmops.Network) (string, tcell.Color) {
	if n.Scope != "swarm" {
		return "", 0
	}
	st := t.endpoints()
	switch {
	case !st.loaded && !st.loading.IsZero():
		return "· …", 0
	case !st.loaded:
		return "", 0
	case st.err != nil:
		return "· n/a", tcell.ColorGray
	}
	e := st.byNet[n.ID]
	if e == nil {
		// No live task and no node has joined it — unless some nodes could
		// not say.
		if st.failed > 0 {
			return "· n/a", tcell.ColorGray
		}
		return "· 0 addr", 0
	}
	if e.Answered == 0 {
		return "· n/a", tcell.ColorGray
	}
	s := fmt.Sprintf("· %d addr", e.Addrs)
	if len(e.Stale) > 0 {
		s += fmt.Sprintf(" ⚠%d stale", len(e.Stale))
	}
	if d := len(e.DupIPs) + len(e.DupMACs); d > 0 {
		s += fmt.Sprintf(" ⚠%d dup", d)
	}
	if e.Unknown > 0 {
		s += fmt.Sprintf(" +%d n/a", e.Unknown)
	}
	if e.Warn() {
		return s, tcell.ColorYellow
	}
	return s, 0
}

// underlay is the visible cluster's configured underlay CIDRs (config
// contexts.<name>.underlay); nil when none is set.
func (u *ui) underlay() []netip.Prefix { return u.cfg.Underlay(u.name) }

// subnetText is the SUBNET cell: the network's subnets, comma-joined.
func subnetText(n swarmops.Network) string {
	if len(n.Subnets) == 0 {
		return "-"
	}
	parts := make([]string, len(n.Subnets))
	for i, p := range n.Subnets {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

// overlapColor is the marker colour for a severity: red when node IPs sit
// inside the subnet, yellow for an underlay/network overlap.
func overlapColor(s swarmops.OverlapSeverity) (tcell.Color, string) {
	switch s {
	case swarmops.OverlapHigh:
		return tcell.ColorRed, "red"
	case swarmops.OverlapWarn:
		return tcell.ColorYellow, "yellow"
	}
	return tcell.ColorWhite, "white"
}

// containersText is the CONTAINERS cell: running tasks attached, plus what
// the nodes' endpoint views add (endpointNote).
func (t *networksTab) containersText(n swarmops.Network) string {
	s := fmt.Sprintf("%d", n.Containers)
	if note, _ := t.endpointNote(n); note != "" {
		s += " " + note
	}
	return s
}

func (t *networksTab) footer() string {
	km := t.u.km
	scope := "all"
	if t.all {
		scope = "swarm only"
	}
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/%s[white] attached  [yellow]%s[white] new  [yellow]%s[white] %s", keyLabel(km.NetAttached), keyLabel(km.NetNew), keyLabel(km.NetAll), scope)
}

func (t *networksTab) helpSection(h *helpWriter) {
	km := t.u.km
	h.sec("Networks")
	h.line("Enter/"+keyLabel(km.NetAttached), "attached services")
	h.line(keyLabel(km.NetNew), "new network")
	h.line(keyLabel(km.NetAll), "swarm networks only (default) / all — the local ones (bridge, host, …) are the manager node's only")
	h.line("  a", "in attached: attach a service")
	h.danger("  d", "in attached: detach a service (rolling update)")
	h.line("⚠ SUBNET", "red: node IPs inside the subnet; yellow: overlaps the underlay or another network (details: Enter)")
	h.line("CONTAINERS", "running tasks · addresses the nodes know; ⚠ stale/dup: an address no live task has, or one held twice (details: Enter); n/a: node agent not reachable")
	h.line("AGE", "compact past 30 days: 3mo, 4y")
}

func (t *networksTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "NETWORK\tDRIVER\tSCOPE\tTYPE\tSUBNET\tSERVICES\tCONTAINERS\tOVERLAP\tAGE")
	for _, n := range t.rows() {
		overlap := "-"
		if s := n.OverlapSeverity(); s != swarmops.OverlapNone {
			overlap = s.String()
		}
		fmt.Fprintf(b, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			n.Name, orDash(n.Driver), orDash(n.Scope), networkType(n), subnetText(n), len(n.Services),
			t.containersText(n), overlap, volumeAge(n.Created))
	}
}

func (t *networksTab) renderRow(r int, n swarmops.Network) {
	subnetCell := tview.NewTableCell(subnetText(n)).SetExpansion(1)
	if sev := n.OverlapSeverity(); sev != swarmops.OverlapNone {
		c, _ := overlapColor(sev)
		subnetCell = tview.NewTableCell("⚠ " + subnetText(n)).SetTextColor(c).SetExpansion(1)
	}
	ctrCell := tview.NewTableCell(t.containersText(n)).SetTextColor(tcell.ColorGray).SetExpansion(1)
	if n.Containers > 0 {
		ctrCell.SetTextColor(tcell.ColorGreen)
	}
	if _, c := t.endpointNote(n); c != 0 {
		ctrCell.SetTextColor(c)
	}
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
	t.table.SetCell(r, 4, subnetCell)
	t.table.SetCell(r, 5, encCell)
	t.table.SetCell(r, 6, svcCell)
	t.table.SetCell(r, 7, ctrCell)
	t.table.SetCell(r, 8, tview.NewTableCell(volumeAge(n.Created)).SetExpansion(1))
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

	// frame holds the addressing header (subnet, gateway, overlap findings
	// and the remedy) above the list; headerLines is its wrapped height.
	frame       *tview.Flex
	headerText  string
	headerLines int
	header      *tview.TextView
	// swarmNet: the network is swarm-scope, so the header carries the
	// endpoint summary line and the list the per-node endpoint block.
	swarmNet bool

	ov *overlay
}

// netMembersWidth is the members overlay's width (border included).
const netMembersWidth = 92

// netHeader is the members overlay's addressing header: subnets and gateways,
// then every overlap finding (red/yellow) and, if there are any, the remedy.
// It returns the text and its height wrapped to width columns (0 lines when
// the network has no address config — host, none).
func netHeader(n swarmops.Network, width int) (string, int) {
	var lines []string
	if len(n.Subnets) > 0 {
		l := "[gray]subnet[-] " + subnetText(n)
		if len(n.Gateways) > 0 {
			gws := make([]string, len(n.Gateways))
			for i, g := range n.Gateways {
				gws[i] = g.String()
			}
			l += "  [gray]gateway[-] " + strings.Join(gws, ",")
		}
		l += fmt.Sprintf("  [gray]containers[-] %d", n.Containers)
		lines = append(lines, l)
	}
	for _, f := range n.Overlaps {
		_, tag := overlapColor(f.Severity)
		lines = append(lines, fmt.Sprintf("[%s]⚠ %s[-]", tag, tview.Escape(f.Message)))
	}
	if len(n.Overlaps) > 0 {
		lines = append(lines, "[gray]fix:[-] "+tview.Escape(swarmops.OverlapRemedy))
	}
	if len(lines) == 0 {
		return "", 0
	}
	h := 1 // blank separator line before the list
	for _, l := range lines {
		h += wrappedHeight(tview.TaggedStringWidth(l), width)
	}
	return strings.Join(lines, "\n") + "\n", h
}

// wrappedHeight is how many rows a line of w cells takes at width columns
// (approximate under word wrap, which may break a little earlier — one row
// of slack is added for long lines).
func wrappedHeight(w, width int) int {
	if width <= 0 || w <= width {
		return 1
	}
	return (w+width-1)/width + 1
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
	v := &netMembersView{u: u, n: n, list: list, expanded: map[string]bool{}}
	v.frame = tview.NewFlex().SetDirection(tview.FlexRow)
	v.frame.SetBorder(true).SetTitle(fmt.Sprintf(" %s%s — attached services ", n.Name, badge))
	v.headerText, v.headerLines = netHeader(n, netMembersWidth-2)
	v.swarmNet = n.Scope == "swarm"
	if v.swarmNet {
		// One more header line, the endpoint summary (kept short enough not
		// to wrap); a header that had nothing else gets its blank line too.
		v.headerLines = max(v.headerLines, 1) + 1
	}
	if v.headerLines > 0 {
		v.header = tview.NewTextView().SetDynamicColors(true).SetWrap(true).SetWordWrap(true)
		v.header.SetText(v.headerContent())
		v.frame.AddItem(v.header, v.headerLines, 0, false)
	}
	v.frame.AddItem(list, 0, 1, true)
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
	list := v.list
	cur := list.GetCurrentItem()
	list.Clear()
	v.rowSvc = v.rowSvc[:0]
	v.addLabelRows()
	v.addServiceRows()
	if v.swarmNet {
		for _, l := range endpointBlock(v.u.netTab().endpoints().byNet[v.n.ID], v.n.Name, netMembersWidth-2) {
			v.addRow(l, "")
		}
	}
	if cur < list.GetItemCount() {
		list.SetCurrentItem(cur)
	}
}

// addRow adds one list row and the service it belongs to ("" for none).
func (v *netMembersView) addRow(text, svc string) {
	v.list.AddItem(text, "", 0, nil)
	v.rowSvc = append(v.rowSvc, svc)
}

// addLabelRows puts the network's own labels (read-only) at the top —
// Docker has no network-update API, so they can't be edited here.
func (v *netMembersView) addLabelRows() {
	lbls := swarmops.KVPairs(v.n.Labels)
	if len(lbls) == 0 {
		return
	}
	v.addRow("[gray]labels[-]", "")
	for _, l := range lbls {
		v.addRow("    "+tview.Escape(l), "")
	}
	v.addRow("", "")
}

// addServiceRows lists each service: its head, its aliases when unfolded,
// and its containers.
func (v *netMembersView) addServiceRows() {
	if len(v.members) == 0 {
		if v.loaded {
			v.addRow("(no services attached)", "")
		} else {
			v.addRow(loadingText, "")
		}
	}
	idW, nodeW := memberColumns(v.members)
	for _, s := range v.members {
		v.addRow(v.serviceHead(s), s.Name)
		if v.loaded && v.expanded[s.Name] {
			for _, a := range s.Aliases {
				v.addRow("        [gray]alias:[-] "+tview.Escape(a), s.Name)
			}
		}
		for _, c := range s.Containers {
			v.addRow(fmt.Sprintf("    %-*s  %-*s  %s", idW, c.ID, nodeW, orDash(c.Node), orDash(c.IPv4)), s.Name)
		}
	}
}

// memberColumns is the id and node column widths: the widest across every
// service, so the node and IP columns line up down the whole list, not just
// per row.
func memberColumns(members []swarmops.NetService) (idW, nodeW int) {
	for _, s := range members {
		for _, c := range s.Containers {
			idW = max(idW, len(c.ID))
			nodeW = max(nodeW, len(orDash(c.Node)))
		}
	}
	return idW, nodeW
}

// serviceHead is a service's row: its name, and once the lookup is in, its
// container count and whether its aliases are folded (+) or shown (-).
func (v *netMembersView) serviceHead(s swarmops.NetService) string {
	if !v.loaded {
		return tview.Escape(s.Name) + " …"
	}
	head := fmt.Sprintf("%s (%d)", tview.Escape(s.Name), len(s.Containers))
	switch {
	case len(s.Aliases) == 0:
		head += "  [gray]no aliases[-]"
	case v.expanded[s.Name]:
		head += fmt.Sprintf("  [aqua]- %d aliases[-]", len(s.Aliases))
	default:
		head += fmt.Sprintf("  [aqua]+ %d aliases[-]", len(s.Aliases))
	}
	return head
}

func (v *netMembersView) close() { v.ov.Close() }

// headerContent is the addressing header plus, for a swarm network, the
// endpoint summary line.
func (v *netMembersView) headerContent() string {
	text := v.headerText
	if v.swarmNet {
		text += endpointSummary(v.u.netTab().endpoints(), v.n) + "\n"
	}
	return text
}

// endpointsArrived repaints what the endpoint analysis feeds: the header
// line and the per-node block in the list.
func (v *netMembersView) endpointsArrived() {
	if v.header != nil {
		v.header.SetText(v.headerContent())
	}
	v.render()
}

// endpointSummary is the detail header's endpoint line: addresses the nodes
// know, and whether any look stale (listed below the services).
func endpointSummary(st *netEndpointsState, n swarmops.Network) string {
	const lead = "[gray]endpoints[-] "
	switch {
	case !st.loaded && !st.loading.IsZero():
		return lead + loadingText
	case !st.loaded:
		return lead + "[gray]not loaded (agents not reachable from here)[-]"
	case st.err != nil:
		return lead + "[gray]? " + tview.Escape(st.err.Error()) + "[-]"
	}
	e := st.byNet[n.ID]
	if e == nil {
		if st.failed > 0 {
			return lead + "[gray]? some agents could not answer — see below[-]"
		}
		return lead + "none — no node has joined this network"
	}
	if e.Answered == 0 {
		return lead + "[gray]? no agent could answer — see below[-]"
	}
	s := lead + fmt.Sprintf("%d running · %d addresses · %d LB", e.Running, e.Addrs, e.LB)
	var warn []string
	if len(e.Stale) > 0 {
		warn = append(warn, fmt.Sprintf("%d stale", len(e.Stale)))
	}
	if d := len(e.DupIPs) + len(e.DupMACs); d > 0 {
		warn = append(warn, fmt.Sprintf("%d duplicate", d))
	}
	if len(warn) > 0 {
		s += "  [yellow]⚠ " + strings.Join(warn, ", ") + " — listed below[-]"
	}
	if e.Unknown > 0 {
		s += fmt.Sprintf("  [gray]? %d node(s) unknown[-]", e.Unknown)
	}
	return s
}

// endpointBlock is the per-node endpoint view the detail list ends with: one
// row per node (running tasks the manager places there against the node's
// own endpoints), then every stale candidate and duplicate, then the fix.
// Nodes that have not joined the network and expect nothing are left out.
func endpointBlock(e *swarmops.NetEndpoints, netName string, width int) []string {
	if e == nil {
		return nil
	}
	rows := []string{"", "[gray]endpoints per node — running (manager) · local containers + LB · MACs · networkdb entries[-]"}
	nodeW := 0
	for _, n := range e.Nodes {
		nodeW = max(nodeW, len(n.Node))
	}
	for _, n := range e.Nodes {
		if line, ok := endpointNodeRow(n, nodeW); ok {
			rows = append(rows, line)
		}
	}
	if len(e.Stale) > 0 {
		rows = append(rows, "", fmt.Sprintf("[yellow]stale candidates (%d) — no live task has the address[-]", len(e.Stale)))
		for _, s := range e.Stale {
			rows = append(rows, staleEndpointRow(s))
		}
	}
	rows = append(rows, duplicateRows("duplicate IPs — one address on several endpoints", e.DupIPs)...)
	rows = append(rows, duplicateRows("duplicate MACs", e.DupMACs)...)
	if e.Warn() {
		rows = append(rows, endpointFixRows(netName, width)...)
	}
	return rows
}

// endpointNodeRow is one node's row of the endpoint block, its name padded
// to nodeW; false for a node off the network that expects nothing there.
func endpointNodeRow(n swarmops.NodeEndpoints, nodeW int) (string, bool) {
	name := fmt.Sprintf("    %-*s  ", nodeW, tview.Escape(n.Node))
	switch n.State {
	case swarmops.NodeEndpointsOK:
		line := fmt.Sprintf("%srunning %d · local %d + %d LB · MACs %d · networkdb %d",
			name, n.Expected, n.Containers, n.LB, n.MACs, n.Known)
		if n.Stale > 0 {
			line += fmt.Sprintf("  [yellow]⚠ %d stale[-]", n.Stale)
		}
		if n.Containers != n.Expected {
			line += "  [gray](local ≠ running)[-]"
		}
		return line, true
	case swarmops.NodeNotOnNetwork:
		if n.Expected > 0 {
			return fmt.Sprintf("%s[yellow]running %d, but the node reports no such network[-]", name, n.Expected), true
		}
		return "", false
	case swarmops.NodeNetworkError, swarmops.NodeUnreachable:
		return fmt.Sprintf("%s[gray]%s: %s[-]", name, n.State, tview.Escape(n.Err)), true
	default:
		return fmt.Sprintf("%s[gray]%s[-]", name, n.State), true
	}
}

// staleEndpointRow is one stale candidate: address, MAC, endpoint name, and
// where it was found.
func staleEndpointRow(s swarmops.StaleEndpoint) string {
	line := fmt.Sprintf("    %-15s %-17s %s", s.IP, orDash(s.MAC), tview.Escape(orDash(s.Name)))
	var where []string
	if s.Owner != "" {
		where = append(where, "on "+tview.Escape(s.Owner))
	}
	if s.Orphan {
		where = append(where, "endpoint without container")
	} else if s.Local {
		where = append(where, "still attached")
	}
	if len(s.SeenBy) > 0 {
		where = append(where, "seen by "+tview.Escape(strings.Join(s.SeenBy, ",")))
	}
	if len(where) > 0 {
		line += "  [gray]" + strings.Join(where, " · ") + "[-]"
	}
	return line
}

// duplicateRows is a titled list of addresses held by several endpoints;
// nothing when there are none.
func duplicateRows(title string, ds []swarmops.Duplicate) []string {
	if len(ds) == 0 {
		return nil
	}
	rows := []string{"", fmt.Sprintf("[yellow]%s (%d)[-]", title, len(ds))}
	for _, d := range ds {
		rows = append(rows, fmt.Sprintf("    %-17s %s", d.Addr, tview.Escape(strings.Join(d.Endpoints, ", "))))
	}
	return rows
}

// endpointFixRows is the remedy under a block that found something.
func endpointFixRows(netName string, width int) []string {
	rows := []string{"",
		"[gray]fix:[-] on a node that lists the entry, remove the leftover endpoint:",
		"       docker network disconnect -f " + tview.Escape(orDash(netName)) + " <endpoint name>"}
	for _, l := range tview.WordWrap("or restart Docker on the node that held it. Refresh first: a task that "+
		"just started or stopped shows here for a moment.", width-6) {
		rows = append(rows, "     [gray]"+tview.Escape(l)+"[-]")
	}
	return rows
}

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
	switch ev.Key() {
	case tcell.KeyEscape:
		v.close()
		return nil
	case tcell.KeyEnter:
		v.toggleAliases()
		return nil
	}
	if ev.Key() != tcell.KeyRune {
		return vimListKeys(ev)
	}
	switch ev.Rune() {
	case v.u.km.Quit, 'i':
		v.close()
	case 'A':
		v.editAliases()
	case 'a':
		v.attach()
	case 'd':
		v.detach()
	default:
		return vimListKeys(ev)
	}
	return nil
}

// toggleAliases folds or unfolds the current row's service, so Enter
// anywhere in a service's block expands/collapses its aliases.
func (v *netMembersView) toggleAliases() {
	if svc := v.curSvc(); svc != "" {
		v.expanded[svc] = !v.expanded[svc]
		v.render()
	}
}

// editAliases opens the current row's service's aliases on this network.
func (v *netMembersView) editAliases() {
	u, n := v.u, v.n
	svc := v.curSvc()
	if svc == "" {
		return
	}
	if !v.loaded {
		u.info("still loading — try again in a moment")
		return
	}
	// Same staged alias editor as the inspect view; applies via
	// swarmops.SetNetworkAliases, then reloads this view to show the new aliases.
	u.openAliasEditorForNet(svc, n.Name, n.ID, v.list, v.reload)
}

// attach asks for a service (those not on the network are suggested) and
// attaches the network to it.
func (v *netMembersView) attach() {
	u, n, dcli := v.u, v.n, v.u.dcli
	suggestions := swarmops.ServicesExcluding(u.serviceNamesFromCache(), n.Services)
	u.servicePrompt(
		fmt.Sprintf("attach a service to network %q", n.Name),
		fmt.Sprintf("Attach network %q to service", n.Name), "Attach",
		suggestions, v.list,
		func(ctx context.Context, svc string) error {
			return swarmops.AttachServiceToNetwork(ctx, dcli, svc, n.ID, n.Name)
		},
		func() { v.close(); u.netTab().reload() },
	)
}

// detach asks which attached service to take off the network.
func (v *netMembersView) detach() {
	u, n, dcli := v.u, v.n, v.u.dcli
	if len(n.Services) == 0 {
		u.info("no services are attached to this network")
		return
	}
	u.servicePrompt(
		fmt.Sprintf("detach a service from network %q", n.Name),
		fmt.Sprintf("Detach network %q from service", n.Name), "Detach",
		n.Services, v.list,
		func(ctx context.Context, svc string) error {
			return swarmops.DetachServiceFromNetwork(ctx, dcli, svc, n.ID, n.Name)
		},
		func() { v.close(); u.netTab().reload() },
	)
}

// open wires the list to its input/footer and kicks off the initial fetch.
func (v *netMembersView) open() {
	u, n, list := v.u, v.n, v.list
	v.render()
	v.ov = u.overlayFor(pageNetMembers, u.netTab().table, footerKeys("a", "attach", dangerKey("d"), "detach", "Enter", "aliases", "A", "add alias", "j/k", "move", "Esc", "back"))
	list.SetInputCapture(v.handleKey)
	height := len(n.Services) + 6
	if v.swarmNet {
		height += 8 // the per-node endpoint block
	}
	if height > 22 {
		height = 22
	}
	height += v.headerLines
	v.ov.show(centered(v.frame, netMembersWidth, height), list)
	u.netTab().members = v
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
	form.AddInputField("Subnet (optional, e.g. 10.100.1.0/24)", "", 22, nil, func(t string) { o.Subnet = t })
	form.AddInputField("Gateway (optional)", "", 22, nil, func(t string) { o.Gateway = t })
	form.AddInputField("Labels (optional, k=v,k=v)", "", 40, nil, func(t string) { labels = t })
	ov := u.overlayFor(pageNetForm, nettable, footerKeys("Tab", "next field", "Enter", "confirm", "Esc", "cancel"))
	closeForm := ov.Close
	create := func(o swarmops.NewNetworkOpts) {
		if err := swarmops.CreateNetwork(ctx, dcli, o); err != nil {
			u.info("create failed: " + err.Error())
			return
		}
		name := strings.TrimSpace(o.Name)
		closeForm()
		t.reload()
		u.flash(" [green]created[white] network " + name)
	}
	checking := false // one address check at a time: a second Create press waits for it
	underlay := u.underlay()
	form.AddButton("Create", func() {
		if checking {
			return
		}
		lbls, err := swarmops.ParseKVList(labels)
		if err != nil {
			u.info("invalid labels: " + err.Error())
			return
		}
		o.Labels = lbls
		opts := o // the form keeps editing o; the check and the create get this snapshot
		// Before creating, check the address plan against the node IPs, the
		// underlay and the existing networks (or, without a subnet, the pool
		// Docker will pick one from). A hit asks; a clean check creates.
		checking = true
		async(u, "ui.networks.precheck", func(ctx context.Context) ([]swarmops.OverlapFinding, error) {
			return swarmops.PreflightNetworkCreate(ctx, dcli, opts, underlay), nil
		}, func(fs []swarmops.OverlapFinding, _ error) {
			checking = false
			if len(fs) == 0 {
				create(opts)
				return
			}
			u.confirm(createOverlapMessage(opts, fs), "Create anyway", form, func() { create(opts) })
		})
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

// createOverlapMessage is the create-anyway question: what the address check
// found for the new network, and how to avoid it.
func createOverlapMessage(o swarmops.NewNetworkOpts, fs []swarmops.OverlapFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Network %q: address check\n\n", strings.TrimSpace(o.Name))
	for _, f := range fs {
		fmt.Fprintf(&b, "⚠ %s\n", f.Message)
	}
	if strings.TrimSpace(o.Subnet) != "" {
		b.WriteString("\nPick a subnet outside the hosts' network (e.g. 10.100.1.0/24).")
	}
	b.WriteString("\n\nCreate anyway?")
	return b.String()
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
	if ev.Key() == tcell.KeyRune && ev.Rune() == km.NetAll {
		t.toggleAll()
		return nil
	}
	return u.tabKeys(ev)
}
