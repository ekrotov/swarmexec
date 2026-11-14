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
