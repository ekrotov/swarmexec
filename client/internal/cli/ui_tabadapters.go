// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/rivo/tview"
)

// Thin tab-registry adapters for the tabs that do not have a type of their
// own yet (review A-02 converts one tab per MR, Volumes first). Each one only
// forwards to the methods on *ui that the former `switch u.active` statements
// called, and carries the footer, "?" entries and copy format those switches
// held. A tab that gets its own type takes its adapter with it.

// containersTab is the Stacks/Services tree. It is the largest tab, so it
// converts last.
type containersTab struct{ u *ui }

func (t containersTab) widget() tview.Primitive { return t.u.ctree }
func (t containersTab) enter()                  {}

func (t containersTab) refresh() {
	t.u.loadContainers()
	t.u.loadUsage()
}

// footer is the widest footer and already fills a ~117-column terminal;
// anything past the edge wraps onto a hidden second row. So labels here are
// kept terse to make room for X (destructive, and it must not be the thing
// that silently falls off the end).
func (t containersTab) footer() string {
	km, kl := t.u.km, keyLabel
	return fmt.Sprintf("[yellow]j/k[white] move  [yellow]Enter[white] open  [yellow]%s[white] logs  [yellow]%s/%s[white] fold  [yellow]%s[white] inspect  [yellow]%s[white] search  [yellow]%s[white] forward  [yellow]%s[white] risks  [yellow]%s[white] stacks  %s remove",
		kl(km.Logs), kl(km.Fold), kl(km.Unfold), kl(km.ContainerInspect), kl(km.Search), kl(km.Forward), kl(km.SecurityRisks), kl(km.StackGroup), keyTag(dangerKey("X")))
}

func (t containersTab) helpSection(h *helpWriter) {
	km, kl := t.u.km, keyLabel
	h.sec("Stacks/Services")
	h.line("Enter", "expand a service · open a container's menu")
	h.line(kl(km.Logs), "logs (container, service, or a whole stack)")
	h.line(kl(km.ContainerInspect), "inspect: service/task detail + editors")
	h.line(kl(km.Fold)+"/"+kl(km.Unfold), "fold / unfold")
	h.line(kl(km.Forward), "port-forward the task under the cursor (form: container port, local port, address; Enter forwards from any field)")
	h.line(kl(km.SecurityRisks), "security-risks overlay (spec hardening checks)")
	h.line(kl(km.StackGroup), "group services by stack / flat list")
	h.line(kl(km.StackFile), "stack file: export the stack, or compare one against it")
	h.danger("X", "remove: service · container’s service · whole stack (confirmed)")
	h.line(kl(km.Search), "search services / containers / nodes")

	h.sec("Service inspect (" + kl(km.ContainerInspect) + ")")
	h.line("a", "actions menu (all edits below, no Shift needed)")
	h.line("Enter", "NETWORKS: expand a network → its containers")
	h.line("1 2 3", "table · stats (cpu/memory) · raw JSON")
	h.line("t", "cycle those three views")
	h.line("d / D", "diff spec · why (placement)")
	h.line("R", "roll back to the previous version")
	h.line("s / f", "scale · force-update")
	h.line("u", "set the image version (update / pin / revert)")
	h.line("p l e", "edit ports · labels · env")
	h.line("n S v", "edit networks · secrets · mounts")
	h.line("r P", "edit resources · placement")
	h.danger("X", "remove the service")
}

// yank copies the tree as displayed, indenting by depth so a stack-grouped
// tree copies as a grouped tree (and an ungrouped one is unchanged).
func (t containersTab) yank(b *strings.Builder) {
	for _, stackOrSvc := range t.u.croot.GetChildren() {
		fmt.Fprintln(b, trimFoldMarker(stackOrSvc.GetText()))
		for _, svc := range stackOrSvc.GetChildren() {
			fmt.Fprintf(b, "  %s\n", trimFoldMarker(svc.GetText()))
			for _, c := range svc.GetChildren() {
				fmt.Fprintf(b, "    %s\n", c.GetText())
			}
		}
	}
}

func (t containersTab) searchText() string        { return t.u.filter }
func (t containersTab) searchPlaceholder() string { return "filter services / containers / nodes" }

func (t containersTab) setSearch(q string) {
	t.u.filter = q
	t.u.renderContainers()
}

// forwardsTab: the forwards are session-wide (forwardRegistry), so the tab
// has nothing to load — entering and refreshing repaint.
type forwardsTab struct{ u *ui }

func (t forwardsTab) widget() tview.Primitive { return t.u.ftable }
func (t forwardsTab) enter()                  { t.u.renderForwards() }
func (t forwardsTab) refresh()                { t.u.renderForwards() }

func (t forwardsTab) footer() string {
	km := t.u.km
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  %s stop  [yellow]%s[white] copy url",
		keyTag(dangerKey(keyLabel(km.FwdStop))), keyLabel(km.FwdCopyURL))
}

func (t forwardsTab) helpSection(h *helpWriter) {
	km := t.u.km
	h.sec("Forwards")
	h.line("Enter / i", "details")
	h.danger(keyLabel(km.FwdStop), "stop")
	h.line(keyLabel(km.FwdCopyURL), "copy URL")
}

func (t forwardsTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "LOCAL\tREMOTE\tCONTAINER\tSERVICE\tNODE\tSTATE")
	for _, e := range t.u.forwards.list() {
		local := "-"
		if e.boundPort() > 0 {
			local = e.localEndpoint()
		}
		fmt.Fprintf(b, "%s\t%d\t%s\t%s\t%s\t%s\n",
			local, e.remote, shortID(e.cand.ContainerID),
			orDash(e.cand.Service), orDash(e.cand.NodeName), e.state)
	}
}

type networksTab struct{ u *ui }

func (t networksTab) widget() tview.Primitive { return t.u.nettable }
func (t networksTab) enter()                  { t.u.loadNetworks() }
func (t networksTab) refresh()                { t.u.loadNetworks() }

func (t networksTab) footer() string {
	km := t.u.km
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/%s[white] attached  [yellow]%s[white] new", keyLabel(km.NetAttached), keyLabel(km.NetNew))
}

func (t networksTab) helpSection(h *helpWriter) {
	km := t.u.km
	h.sec("Networks")
	h.line("Enter/"+keyLabel(km.NetAttached), "attached services")
	h.line(keyLabel(km.NetNew), "new network")
}

func (t networksTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "NETWORK\tDRIVER\tSCOPE\tTYPE\tSERVICES\tAGE")
	for _, n := range t.u.nets {
		fmt.Fprintf(b, "%s\t%s\t%s\t%s\t%d\t%s\n",
			n.Name, orDash(n.Driver), orDash(n.Scope), networkType(n), len(n.Services), volumeAge(n.Created))
	}
}

type secretsTab struct{ u *ui }

func (t secretsTab) widget() tview.Primitive { return t.u.sectable }
func (t secretsTab) enter()                  { t.u.loadSecrets() }
func (t secretsTab) refresh()                { t.u.loadSecrets() }

func (t secretsTab) footer() string {
	km := t.u.km
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] new  %s delete", keyLabel(km.SecNew), keyTag(dangerKey(keyLabel(km.SecDelete))))
}

func (t secretsTab) helpSection(h *helpWriter) {
	km := t.u.km
	h.sec("Secrets")
	h.line("Enter / i", "details")
	h.line("  a · d", "in details: attach to · detach from a service")
	h.line("  r", "in details: rotate — new content, same name (services restart twice)")
	h.line(keyLabel(km.SecNew), "new (vim editor, :r file)")
	h.danger(keyLabel(km.SecDelete), "delete")
}

func (t secretsTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "SECRET\tUSED BY\tAGE\tUPDATED\tLABELS")
	for _, s := range t.u.secs {
		fmt.Fprintf(b, "%s\t%d\t%s\t%s\t%d\n", s.Name, len(s.Services), volumeAge(s.Created), volumeAge(s.Updated), len(s.Labels))
	}
}

type nodesTab struct{ u *ui }

func (t nodesTab) widget() tview.Primitive { return t.u.notable }
func (t nodesTab) enter()                  { t.u.loadNodes() }

func (t nodesTab) refresh() {
	t.u.loadNodes()
	t.u.loadUsage() // the node detail shows measured usage next to reservations
}

func (t nodesTab) footer() string {
	km, kl := t.u.km, keyLabel
	return fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] edit labels  [yellow]%s[white] availability  %s prune  [yellow]%s[white] prune schedule (all nodes)",
		kl(km.NodeLabels), kl(km.NodeAvail), keyTag(dangerKey(kl(km.NodePrune))), kl(km.NodePruneSched))
}

func (t nodesTab) helpSection(h *helpWriter) {
	km, kl := t.u.km, keyLabel
	h.sec("Nodes")
	h.line("Enter / i", "details")
	h.line(kl(km.NodeLabels), "edit labels")
	h.line(kl(km.NodeAvail), "availability: active / pause / drain")
	h.danger(kl(km.NodePrune), "prune the node: containers, images, build cache, networks, volumes (choose + confirm)")
	h.line(kl(km.NodePruneSched), "prune schedule for ALL nodes (restarts the agents on save)")
	h.line("m", "in the details: measure reclaimable space now")
}

// yank copies the node list. Before the registry the Nodes tab fell into
// yankCurrent's default branch and copied the VOLUME list.
func (t nodesTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "NODE\tROLE\tAVAIL\tSTATE\tENGINE\tTASKS\tLABELS")
	for _, n := range t.u.nodeInfos {
		name := n.Hostname
		if n.Leader {
			name += " (leader)"
		}
		fmt.Fprintf(b, "%s\t%s\t%s\t%s\t%s\t%d\t%d\n", name, orDash(n.Role), orDash(n.Availability),
			orDash(n.State), orDash(n.EngineVersion), n.Tasks, len(n.Labels))
	}
}

type configsTab struct{ u *ui }

func (t configsTab) widget() tview.Primitive { return t.u.cfgtable }
func (t configsTab) enter()                  { t.u.loadConfigs() }

// refresh reloads the configs. Before the registry the refresh key's switch
// had no configs case, and on this tab it repainted the forwards table.
func (t configsTab) refresh() { t.u.loadConfigs() }

func (t configsTab) footer() string {
	return "[yellow]j/k[white] up/down  [yellow]Enter/i[white] details (with content)"
}

func (t configsTab) helpSection(h *helpWriter) {
	h.sec("Configs")
	h.line("Enter / i", "details, including the config's content")
}

// yank copies the config list. Before the registry the Configs tab fell into
// yankCurrent's default branch and copied the VOLUME list.
func (t configsTab) yank(b *strings.Builder) {
	fmt.Fprintln(b, "CONFIG\tUSED BY\tSIZE\tAGE\tUPDATED\tLABELS")
	for _, c := range t.u.cfgs {
		fmt.Fprintf(b, "%s\t%d\t%s\t%s\t%s\t%d\n", c.Name, len(c.Services), humanBytes(int64(c.Size)),
			volumeAge(c.Created), volumeAge(c.Updated), len(c.Labels))
	}
}
