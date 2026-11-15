// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmops"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// treeTab is the Stacks/Services tab: the tree of stacks, services and their
// running containers, and the entry point to logs, exec, port-forward and the
// service inspect.
//
// It converted last (review A-02): the tree is the session's home widget, so
// more of the UI leans on it than on any table. The tree is still reachable as
// u.ctree for that reason — every dialog opened from it hands the keyboard
// back there — but it is built and owned here.
type treeTab struct {
	u    *ui
	tree *tview.TreeView
	root *tview.TreeNode // synthetic, hidden: stacks or services hang under it

	// Nest services under their stack. A preference of the operator's, not a
	// property of a cluster: it survives a switch. Only takes effect once a
	// service carries a stack label (see anyStacked).
	groupByStack bool
}

func newTreeTab(u *ui) *treeTab {
	t := &treeTab{u: u, groupByStack: true}
	t.tree = tview.NewTreeView()
	t.root = tview.NewTreeNode("")
	t.tree.SetRoot(t.root).SetTopLevel(1) // hide the synthetic root; services are top-level
	t.tree.SetSelectedFunc(t.onSelect)
	t.tree.SetInputCapture(t.keys)
	u.ctree = t.tree
	return t
}

// treeTab is the session's Stacks/Services tab.
func (u *ui) treeTab() *treeTab { return u.tab("containers").(*treeTab) }

func (t *treeTab) widget() tview.Primitive { return t.tree }
func (t *treeTab) enter()                  {}

func (t *treeTab) refresh() {
	t.load()
	t.u.loadUsage()
}

// footer is the widest footer and already fills a ~117-column terminal;
// anything past the edge wraps onto a hidden second row. So labels here are
// kept terse to make room for X (destructive, and it must not be the thing
// that silently falls off the end).
func (t *treeTab) footer() string {
	km, kl := t.u.km, keyLabel
	return fmt.Sprintf("[yellow]j/k[white] move  [yellow]Enter[white] open  [yellow]%s[white] logs  [yellow]%s/%s[white] fold  [yellow]%s[white] inspect  [yellow]%s[white] search  [yellow]%s[white] forward  [yellow]%s[white] risks  [yellow]%s[white] stacks  %s remove",
		kl(km.Logs), kl(km.Fold), kl(km.Unfold), kl(km.ContainerInspect), kl(km.Search), kl(km.Forward), kl(km.SecurityRisks), kl(km.StackGroup), keyTag(dangerKey("X")))
}

func (t *treeTab) helpSection(h *helpWriter) {
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
func (t *treeTab) yank(b *strings.Builder) {
	for _, stackOrSvc := range t.root.GetChildren() {
		fmt.Fprintln(b, trimFoldMarker(stackOrSvc.GetText()))
		for _, svc := range stackOrSvc.GetChildren() {
			fmt.Fprintf(b, "  %s\n", trimFoldMarker(svc.GetText()))
			for _, c := range svc.GetChildren() {
				fmt.Fprintf(b, "    %s\n", c.GetText())
			}
		}
	}
}

func (t *treeTab) searchText() string        { return t.u.filter }
func (t *treeTab) searchPlaceholder() string { return "filter services / containers / nodes" }

func (t *treeTab) setSearch(q string) {
	t.u.filter = q
	t.render()
}

// onSelect is Enter on a row: a container opens its action menu, a service or
// a stack folds or unfolds. Logs are on their own key.
func (t *treeTab) onSelect(node *tview.TreeNode) {
	if ref, ok := node.GetReference().(resolve.Candidate); ok {
		t.u.containerMenu(ref)
		return
	}
	switch {
	case isServiceNode(node):
		node.SetExpanded(!node.IsExpanded())
		t.markService(node)
	case isStackNode(node):
		node.SetExpanded(!node.IsExpanded())
		t.markStack(node)
	}
}

// remarkServices re-renders every service row in place: a background version
// check landed, and only the "↑ 2.12.0" annotation changes.
func (t *treeTab) remarkServices() { eachServiceNode(t.root, t.markService) }

func (t *treeTab) matchesFilter(c resolve.Candidate) bool {
	u := t.u
	if u.filter == "" {
		return true
	}
	q := strings.ToLower(u.filter)
	return strings.Contains(strings.ToLower(c.Service), q) ||
		strings.Contains(strings.ToLower(c.ContainerID), q) ||
		strings.Contains(strings.ToLower(c.NodeName), q)
}

// versionSuffixFor is the "↑ 2.12.0" annotation for a service, or "". One
// helper because the width calculation and the row rendering must agree: when
// they disagreed, the suffix overflowed the cell it was measured out of.
func (t *treeTab) versionSuffixFor(s resolve.Service) string {
	u := t.u
	if u.regCache == nil {
		return ""
	}
	return swarmops.VersionSuffix(u.regCache.Status(u.ctx, s.ImageRef))
}

func (t *treeTab) markService(n *tview.TreeNode) {
	u := t.u
	ref, ok := n.GetReference().(svcRef)
	if !ok {
		return
	}
	svc := u.svcByName[ref.name]
	row := serviceRow(svc, u.svcCols, t.versionSuffixFor(svc), t.svcUsageBadge(ref.name))
	switch {
	case len(n.GetChildren()) == 0:
		n.SetText("  " + row)
	case n.IsExpanded():
		n.SetText("▾ " + row)
	default:
		n.SetText("▸ " + row)
	}
}

func (t *treeTab) render() {
	u := t.u
	forwards, ctree, croot := u.forwards, t.tree, t.root
	// Where to put the cursor and what to leave unfolded. Normally that is
	// whatever the tree shows right now — this runs on every refresh, and the
	// operator must not be moved by one.
	//
	// After a cluster switch it is NOT: the widget still holds the cluster we
	// just left, and restoring from it would drop the cursor on a service of the
	// wrong cluster (or nowhere). So the switch hands over the position it saved
	// for THIS cluster, and that wins exactly once.
	pos := t.posFromWidget()
	if u.restorePos {
		pos = u.pos
		u.restorePos = false
	}
	prevID, prevSvc, prevStack := pos.container, pos.service, pos.stack
	wasExpanded, stackExpanded := pos.svcFolds, pos.stackFold
	seenStack := map[string]bool{}
	for name := range stackExpanded {
		seenStack[name] = true
	}
	croot.ClearChildren()

	// Group running containers by service for the leaves.
	byService := map[string][]resolve.Candidate{}
	for _, c := range u.lastCands {
		byService[c.Service] = append(byService[c.Service], c)
	}
	// Pre-compute column widths so every container row lines up, regardless
	// of node-name length or whether a task carries a slot.
	slotStr := func(c resolve.Candidate) string {
		if c.Slot > 0 {
			return fmt.Sprintf("slot %d", c.Slot)
		}
		return ""
	}
	nodeW, slotW := 0, 0
	for _, c := range u.lastCands {
		if w := len(orDash(c.NodeName)); w > nodeW {
			nodeW = w
		}
		if w := len(slotStr(c)); w > slotW {
			slotW = w
		}
	}

	// Column widths for the docker service ls-style service rows, computed over
	// every service so the alignment stays stable while filtering — and merged
	// into the widths already in use, so it stays stable over time too. Only an
	// explicit reload starts over.
	fresh := svcColumns{}
	if u.resetCols {
		u.svcCols = svcColumns{}
		u.resetCols = false
	}
	u.svcByName = make(map[string]resolve.Service, len(u.lastSvcs))
	// Rebuilt with the tree, so a container that has gone leaves no entry behind.
	u.leafBase = make(map[string]string, len(u.lastCands))
	for _, s := range u.lastSvcs {
		u.svcByName[s.Name] = s
		if w := len(orDash(s.Name)); w > fresh.name {
			fresh.name = w
		}
		if w := len(orDash(s.Mode)); w > fresh.mode {
			fresh.mode = w
		}
		if w := len(progressCount(s)); w > fresh.repl {
			fresh.repl = w
		}
		// The image cell renders image+suffix (the "↑ 2.12.0" annotation), so the
		// width has to measure both. Measuring only the image let the suffix
		// overflow its cell and shove that row's ports sideways — and since the
		// registry cache re-checks a failed lookup every minute, the suffix could
		// appear and vanish on its own.
		if w := len(s.Image) + len(t.versionSuffixFor(s)); w > fresh.image {
			fresh.image = w
		}
	}
	u.svcCols.growTo(fresh)

	q := strings.ToLower(strings.TrimSpace(u.filter))
	var firstSvc, targetSvc, targetLeaf, targetStack *tview.TreeNode

	// Group by stack unless the operator turned it off, and only when at least
	// one service actually carries a stack label — otherwise every row would
	// hang under a single pointless "(no stack)" parent.
	grouped := t.groupByStack && anyStacked(u.lastSvcs)
	stackNodes := map[string]*tview.TreeNode{}
	if grouped {
		// Create the stack nodes up front in groupByStack's order (alphabetical,
		// unstacked last) so the tree order does not depend on which service
		// happened to be seen first. Stacks left empty by the filter are dropped
		// again below.
		for _, row := range groupByStack(u.lastSvcs) {
			expanded := true // a stack the operator has not touched starts open
			if seenStack[row.Name] {
				expanded = stackExpanded[row.Name]
			}
			n := tview.NewTreeNode("").
				SetReference(stackRef{name: row.Name}).
				SetExpanded(expanded)
			stackNodes[row.Name] = n
			croot.AddChild(n)
			if row.Name == prevStack {
				targetStack = n
			}
		}
	}
	// parentFor returns the node a service hangs under: its stack node when
	// grouped, else the root.
	parentFor := func(s resolve.Service) *tview.TreeNode {
		if !grouped {
			return croot
		}
		if n, ok := stackNodes[stackNameOf(s)]; ok {
			return n
		}
		return croot
	}

	for _, s := range u.lastSvcs {
		nameMatch := q == "" || strings.Contains(strings.ToLower(s.Name), q)
		// Which running containers to list: all when the service name matches,
		// otherwise only the containers that match the filter themselves.
		var shown []resolve.Candidate
		for _, c := range byService[s.Name] {
			if nameMatch || t.matchesFilter(c) {
				shown = append(shown, c)
			}
		}
		// Hide a service only if it neither matches by name nor has any
		// matching container.
		if !nameMatch && len(shown) == 0 {
			continue
		}
		// Collapsed by default (spec); keep a service the operator expanded.
		svcNode := tview.NewTreeNode(serviceRow(s, u.svcCols, "", t.svcUsageBadge(s.Name))).
			SetColor(t.svcColor(s)).
			SetReference(svcRef{name: s.Name}).
			SetExpanded(wasExpanded[s.Name])
		parentFor(s).AddChild(svcNode)
		if firstSvc == nil {
			firstSvc = svcNode
		}
		if s.Name == prevSvc {
			targetSvc = svcNode
		}
		for _, c := range shown {
			var label string
			if slotW > 0 {
				label = fmt.Sprintf("%-12s  %-*s  %-*s  up %s", shortID(c.ContainerID), nodeW, orDash(c.NodeName), slotW, slotStr(c), uptime(c.Uptime))
			} else {
				label = fmt.Sprintf("%-12s  %-*s  up %s", shortID(c.ContainerID), nodeW, orDash(c.NodeName), uptime(c.Uptime))
			}
			label = annotateForwards(label, forwards.forContainer(u.activeCtx, c.ContainerID))
			// Keep the badge-free label: re-marking a row when new readings land
			// then just re-appends, instead of trying to cut the old marker back
			// off a string that also carries the forward and update markers.
			u.leafBase[c.ContainerID] = label
			usage := u.usage[c.ContainerID]
			leaf := tview.NewTreeNode(label + containerHealthBadge(usage.Health) + usageBadge(usage)).SetReference(c)
			svcNode.AddChild(leaf)
			if c.ContainerID == prevID {
				targetLeaf = leaf
				svcNode.SetExpanded(true) // reveal the previously-selected leaf
			}
		}
		// Marker depends on the final child count / expanded state, so set it
		// once the leaves are attached.
		t.markService(svcNode)
	}
	if grouped {
		// Drop stacks the filter emptied, and label the rest — the summary counts
		// what actually ended up under each, so it follows the filter.
		kept := make([]*tview.TreeNode, 0, len(croot.GetChildren()))
		for _, n := range croot.GetChildren() {
			if isStackNode(n) && len(n.GetChildren()) == 0 {
				continue
			}
			t.markStack(n)
			kept = append(kept, n)
		}
		croot.SetChildren(kept)
	}
	if len(croot.GetChildren()) == 0 {
		empty := emptyText("services", "nothing is deployed on this swarm")
		if q != "" {
			empty = fmt.Sprintf("(no matches for %q)", u.filter)
		}
		croot.AddChild(tview.NewTreeNode(empty).SetColor(tcell.ColorGray).SetSelectable(false))
	}
	switch {
	case targetLeaf != nil:
		ctree.SetCurrentNode(targetLeaf)
	case targetSvc != nil:
		ctree.SetCurrentNode(targetSvc)
	case targetStack != nil:
		ctree.SetCurrentNode(targetStack)
	case firstSvc != nil:
		ctree.SetCurrentNode(firstSvc)
	case len(croot.GetChildren()) > 0:
		// Grouped and every stack collapsed: land on the first stack row.
		ctree.SetCurrentNode(croot.GetChildren()[0])
	}
}

// markStack (re)renders a stack group row from the services currently under it:
// name, service count, aggregate tasks, and the update/risk counts. Called after
// the children are attached and again on a fold, like markService.
func (t *treeTab) markStack(n *tview.TreeNode) {
	u := t.u
	ref, ok := n.GetReference().(stackRef)
	if !ok {
		return
	}
	var members []resolve.Service
	for _, child := range n.GetChildren() {
		if sref, ok := child.GetReference().(svcRef); ok {
			if s, found := u.svcByName[sref.name]; found {
				members = append(members, s)
			}
		}
	}
	rows := groupByStack(members)
	summary := stackRow{Name: ref.name}
	if len(rows) > 0 {
		summary = rows[0]
		summary.Name = ref.name // members may be unstacked; keep the node's name
	}

	marker := "▾"
	if !n.IsExpanded() {
		marker = "▸"
	}
	var extra strings.Builder
	if summary.Updating > 0 {
		fmt.Fprintf(&extra, "  [yellow]⟳ %d[-]", summary.Updating)
	}
	if summary.Risky > 0 {
		fmt.Fprintf(&extra, "  [red]🛡 %d[-]", summary.Risky)
	}
	// Health rolls up like the other markers. A stack whose services are all
	// failing their probes would otherwise read as calm at exactly the level an
	// operator scans first — the same misleading "everything is up" one tier
	// above the service rows.
	health := t.stackHealth(summary.Services)
	extra.WriteString(healthBadge(health))
	n.SetText(fmt.Sprintf("%s %s  [gray](%d svc · %d/%d)[-]%s",
		marker, ref.name, len(summary.Services), summary.Running, summary.Desired, extra.String()))
	n.SetColor(healthColor(serviceColor(summary.Running, summary.Desired), health))
}

// removeUnderCursor removes whatever the tree cursor is on, so it can be
// deleted from the tree itself rather than only from inside the inspect overlay
// (i → X). Each row removes what it IS: a container leaf resolves to its owning
// service, because a task alone cannot be removed; a stack row removes the
// stack, with `docker stack rm`'s reach.
func (t *treeTab) removeUnderCursor() {
	u := t.u
	n := t.tree.GetCurrentNode()
	if n == nil {
		return
	}
	switch ref := n.GetReference().(type) {
	case svcRef:
		u.openRemoveService(ref.name, t.tree, t.load)
	case resolve.Candidate:
		// A task cannot be removed on its own — swarm would just reschedule it.
		// Offer the owning service, which is what the operator means.
		if ref.Service == "" {
			return
		}
		u.openRemoveService(ref.Service, t.tree, t.load)
	case stackRef:
		u.openRemoveStack(ref.name, t.tree, t.load)
	}
}

// stackNameOf is a service's stack, or the unstacked bucket label.
func stackNameOf(s resolve.Service) string {
	if name := strings.TrimSpace(s.Stack); name != "" {
		return name
	}
	return noStackLabel
}

// anyStacked reports whether at least one service carries a stack label.
func anyStacked(svcs []resolve.Service) bool {
	for _, s := range svcs {
		if strings.TrimSpace(s.Stack) != "" {
			return true
		}
	}
	return false
}

func sortCands(cands []resolve.Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Service != cands[j].Service {
			return cands[i].Service < cands[j].Service
		}
		if cands[i].Slot != cands[j].Slot {
			return cands[i].Slot < cands[j].Slot
		}
		return cands[i].NodeName < cands[j].NodeName
	})
}

// fetchTree does the two manager round-trips (off the UI goroutine) and
// returns sorted results. It is instrumented so a slow manager shows up in the
// log viewer. ctx and r are the caller's, captured on the UI goroutine: read
// from u here, off the loop, they would be whichever cluster is visible by the
// time the goroutine runs.
func fetchTree(ctx context.Context, r *resolve.Resolver) ([]resolve.Service, []resolve.Candidate, error) {
	svcs, cands, err := swarmops.ServicesAndContainers(ctx, r, "ui.containers")
	if err != nil {
		return nil, nil, err
	}
	sortCands(cands)
	return svcs, cands, nil
}

// apply updates the tree from a fetch result. UI-goroutine only.
func (t *treeTab) apply(svcs []resolve.Service, cands []resolve.Candidate, err error) {
	u := t.u
	croot := t.root
	u.fetchErr = err
	if err != nil {
		croot.ClearChildren()
		croot.AddChild(tview.NewTreeNode(errorText(err)).SetColor(tcell.ColorRed).SetSelectable(false))
		if askpassActive() && isSSHAuthError(err) {
			// After a cancelled sign-in nothing asks again on its own; say how.
			croot.AddChild(tview.NewTreeNode(fmt.Sprintf("ssh could not sign in — press %c to be asked again", u.km.Refresh)).
				SetColor(tcell.ColorYellow).SetSelectable(false))
		}
		u.offerSSHAdd(u.clusterState, err, t.load)
		return
	}
	u.lastSvcs = svcs
	u.lastCands = cands
	u.loaded = true // this cluster has been looked at; "empty" now means empty
	t.render()
}

// loadSync fetches and applies on the caller's goroutine — used at
// startup, before app.Run, where QueueUpdateDraw would deadlock.
func (t *treeTab) loadSync() {
	u := t.u
	u.resetCols = true
	svcs, cands, err := fetchTree(u.ctx, u.r)
	t.apply(svcs, cands, err)
}

// load refreshes without freezing the event loop: it fetches off the
// UI goroutine (two manager round-trips that used to run inline and stall the
// whole TUI) and applies the result via QueueUpdateDraw.
//
// It is the EXPLICIT reload: the refresh key, a removed service, a
// deployed stack, a closed terminal, a cluster becoming visible. Those may
// narrow the columns again; the background poll may not.
func (t *treeTab) load() {
	u := t.u
	u.resetCols = true
	resetAskpassDeclines() // an explicit reload may ask ssh's questions again
	gen, ctx, r := u.generation(), u.ctx, u.r
	go func() {
		svcs, cands, err := fetchTree(ctx, r)
		u.onCluster(gen, func() { t.apply(svcs, cands, err) })
	}()
}

// autoRefresh re-lists services/containers off the UI goroutine on a
// timer so a container replaced by a rolling update (or a scaled service) shows
// up without pressing r. render restores the cursor and expanded
// services, so the refresh is unobtrusive. A transient probe error is ignored
// rather than clobbering the tree with an error node.
//
// It skips while an overlay is open — the tree is hidden, and its periodic
// render on the UI goroutine would compete with keystrokes in the
// overlay (laggy input) — and never overlaps itself (a slow probe over ssh can
// outlast the tick).
func (t *treeTab) autoRefresh() {
	u := t.u
	ctx, r := u.ctx, u.r
	// Cheap gate: skip the fetch when an overlay is known to be open. Best
	// effort by design — the invariant is enforced below, on the UI goroutine.
	if u.anyOverlayOpen() || !u.autoRefreshBusy.CompareAndSwap(false, true) {
		return
	}
	gen := u.generation()
	go func() {
		defer u.autoRefreshBusy.Store(false)
		svcs, cands, err := fetchTree(ctx, r)
		if err != nil || ctx.Err() != nil {
			return // transient error, or the run is ending
		}
		u.onCluster(gen, func() {
			// The gate that matters, asked where the answer is exact: an
			// overlay may have opened while we were fetching, and rebuilding
			// the tree underneath one is what pulls its focus away.
			if u.overlayOpen() {
				return
			}
			t.apply(svcs, cands, nil)
		})
	}()
}

func (u *ui) openTerminal(c resolve.Candidate, command []string, tty bool, user string) {
	app, ac, dcli, f, ctx, ctree := u.app, u.agents, u.dcli, u.f, u.ctx, u.ctree
	ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
	tctx, tcancel := context.WithCancel(ctx)
	tv := newTerminalView(app)
	asUser := ""
	if user != "" {
		asUser = " as " + user
	}
	tv.SetTitle(fmt.Sprintf(" %v%s · %s · %s on %s — Ctrl-] detach ", command, asUser, orDash(c.Service), shortID(c.ContainerID), orDash(c.NodeName)))
	// While the shell is open, turn OFF tview's mouse capture so the local
	// terminal emulator's native click-drag selection / copy works on the
	// pane's output — tcell's mouse reporting otherwise suppresses it, and the
	// pane forwards no mouse of its own. Restored to the operator's setting on
	// close.
	prevMouse := u.mouseEnabled
	ov := u.overlayFor(pageTerm, ctree, "")
	// Close is already idempotent, so the sync.Once this used to need is the
	// overlay's. What stays here is what is specific to a terminal: the mouse
	// goes back to the app, the session is cancelled, the tree reloads.
	closeTerm := func() {
		app.EnableMouse(prevMouse)
		tcancel()
		ov.Close()
		u.treeTab().load()
	}
	tv.detach = closeTerm
	tv.run(tctx, ac, terminalSession{ep: ep, command: command, tty: tty, user: user}, f.connectTimeout, func(code int, rerr error) {
		app.QueueUpdateDraw(func() {
			switch {
			case tctx.Err() != nil:
				closeTerm() // user detached (Ctrl-]) — just close
			case rerr != nil:
				closeTerm()
				u.info(explainSession(ctx, ac.Config(), dcli, rerr).Error())
			case code == 126 || code == 127:
				// The shell could not start (127 = not found, 126 = not
				// executable — e.g. no bash in the image). Keep the pane up so
				// the error output stays readable. Any OTHER exit code means the
				// shell ran and the user ended it (exit / Ctrl-D), possibly with
				// a non-zero last-command status — so close the pane, don't make
				// the user dismiss it a second time.
				tv.showEnded(fmt.Sprintf("[swarmexec] %v exited with code %d — press any key to close", command, code))
			default:
				closeTerm() // shell exited (user typed exit / Ctrl-D) — close
			}
		})
	})
	app.EnableMouse(false) // hand the mouse to the terminal for native copy
	ov.show(tv, tv)
}

func (u *ui) containerMenu(c resolve.Candidate) {
	ac, f, ctree := u.agents, u.f, u.ctree
	ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" %s on %s — checking shells… ", orDash(c.Service), orDash(c.NodeName)))
	ov := u.overlayFor(pageMenu, ctree, footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
	closeMenu := ov.Close

	// Optimistic until the shell probe returns; then unavailable shells grey.
	bashOK, shOK, probed := true, true, false
	render := func() {
		cur := list.GetCurrentItem()
		list.Clear()
		list.AddItem("logs", "", 0, func() { closeMenu(); u.showLogs(c) })
		list.AddItem(shellLabel("bash", bashOK, probed), "", 0, func() {
			if bashOK {
				closeMenu()
				u.openTerminal(c, []string{"bash"}, true, "")
			}
		})
		list.AddItem(shellLabel("sh", shOK, probed), "", 0, func() {
			if shOK {
				closeMenu()
				// tty=true: an interactive sh needs a PTY so its line discipline
				// turns ^C into SIGINT for the foreground process (and gives line
				// editing / job control), same as bash.
				u.openTerminal(c, []string{"sh"}, true, "")
			}
		})
		// Shell in as a specific user/UID (docker exec -u), for images whose
		// default user lacks the tools or permissions you need.
		list.AddItem("shell as user…", "", 0, func() {
			closeMenu()
			u.userPrompt(c, func(usr string) {
				if bashOK {
					u.openTerminal(c, []string{"bash"}, true, usr)
				} else {
					u.openTerminal(c, []string{"sh"}, true, usr) // PTY: ^C → SIGINT, line editing
				}
			})
		})
		list.AddItem("port forward", "", 0, func() { closeMenu(); u.portPrompt(c) })
		list.AddItem("cancel", "", 0, closeMenu)
		if cur >= 0 && cur < list.GetItemCount() {
			list.SetCurrentItem(cur)
		}
	}
	render()
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			closeMenu()
			return nil
		}
		return vimListKeys(ev)
	})

	async(u, "ui.containers.probeShells", func(ctx context.Context) ([2]bool, error) {
		b, s := ac.ProbeShells(ctx, ep, f.connectTimeout)
		return [2]bool{b, s}, nil
	}, func(ok [2]bool, _ error) {
		bashOK, shOK, probed = ok[0], ok[1], true
		list.SetTitle(fmt.Sprintf(" %s on %s — actions ", orDash(c.Service), orDash(c.NodeName)))
		render()
	})

	// Height tracks the item count: 6 items plus the border.
	ov.show(centered(list, 48, 8), list)
}

// svcUsageBadge is the resource marker for a service row: the worst reading
// across its containers. Empty when nothing is known yet or nothing is hot.
func (t *treeTab) svcUsageBadge(service string) string {
	u := t.u
	if len(u.usage) == 0 {
		return ""
	}
	// Health leads: it is the marker that contradicts the running/desired count
	// standing right next to it, so it should not sit behind the resource
	// numbers when the row is long.
	badge := healthBadge(swarmops.HealthOf(u.usage, u.lastCands, service))
	if agg, ok := serviceUsage(u.usage, u.lastCands, service); ok {
		badge += usageBadge(agg)
	}
	return badge
}

// stackHealth folds the healthcheck verdicts of every container under a stack
// into one count.
func (t *treeTab) stackHealth(services []resolve.Service) swarmops.ServiceHealth {
	u := t.u
	var out swarmops.ServiceHealth
	if len(u.usage) == 0 {
		return out
	}
	for _, svc := range services {
		h := swarmops.HealthOf(u.usage, u.lastCands, svc.Name)
		out.Healthy += h.Healthy
		out.Unhealthy += h.Unhealthy
		out.Starting += h.Starting
		out.Unknown += h.Unknown
	}
	return out
}

// svcColor is the tree colour for a service row: the progress verdict,
// overridden when the healthchecks disagree with it.
func (t *treeTab) svcColor(s resolve.Service) tcell.Color {
	u := t.u
	base := progressColor(s)
	if len(u.usage) == 0 {
		return base
	}
	return healthColor(base, swarmops.HealthOf(u.usage, u.lastCands, s.Name))
}

// loadUsage refreshes the live resource readings in the background and re-marks
// the tree when they land.
//
// Deliberately not part of fetchTree: usage comes from the node agents
// rather than the manager, is best-effort, and can be slower than the tree
// fetch. Folding it in would make the whole tree wait on the slowest agent —
// and an agent too old to know the RPC would then break the view instead of
// just leaving the badges off.
func (u *ui) loadUsage() {
	ctx, ac, r, timeout := u.ctx, u.agents, u.r, u.f.connectTimeout
	// Fetched even while an overlay is open: the inspect STATS view shows these
	// readings and promises to refresh, and the history must not have a hole
	// for every minute someone spends in a dialog. The fan-out runs off the
	// main loop; what an overlay must not pay for is remarkUsage's walk of the
	// whole tree, so only that waits until the tree is on screen again.
	if !u.usageBusy.CompareAndSwap(false, true) {
		return
	}
	gen := u.generation()
	go func() {
		defer u.usageBusy.Store(false)
		start := time.Now()
		nodes, err := r.Nodes(ctx)
		if err != nil {
			clientlog.Timed("ui.loadUsage", start, err)
			return
		}
		byContainer, byNode := ac.Usage(ctx, nodes, timeout)
		clientlog.Timed("ui.loadUsage", start, nil)
		if ctx.Err() != nil {
			return
		}
		u.onCluster(gen, func() {
			u.usage, u.nodeUse = byContainer, byNode
			if u.usageHist == nil {
				u.usageHist = map[string]*usageRing{}
			}
			recordUsage(u.usageHist, time.Now(), byContainer)
			if u.onUsage != nil {
				u.onUsage()
			}
			if u.overlayOpen() {
				return // the badges catch up when the tree is back
			}
			// Re-mark rather than rebuild: the readings only change row text, and
			// rebuilding would fight the operator's cursor and fold state.
			u.treeTab().remarkUsage()
		})
	}()
}

// remarkUsage refreshes the badge on every row in place, leaving the tree's
// shape, cursor and fold state alone.
func (t *treeTab) remarkUsage() {
	u := t.u
	t.root.Walk(func(n, parent *tview.TreeNode) bool {
		switch ref := n.GetReference().(type) {
		case svcRef:
			t.markService(n)
			if svc, ok := u.svcByName[ref.name]; ok {
				n.SetColor(t.svcColor(svc))
			}
		case resolve.Candidate:
			if base, ok := u.leafBase[ref.ContainerID]; ok {
				usage := u.usage[ref.ContainerID]
				n.SetText(base + containerHealthBadge(usage.Health) + usageBadge(usage))
			}
		}
		return true
	})
}

// keys is the key handler of the container tree.
//
// On the tree the search key opens search and fold/unfold (h/l by default)
// collapse/expand the service under the cursor; j/k stay down/up via the
// shared keys.
func (t *treeTab) keys(ev *tcell.EventKey) *tcell.EventKey {
	u := t.u
	km, ctree, croot := u.km, t.tree, t.root
	// g / G (and Home / End) jump the SELECTION to the first / last row. tview's
	// tree only scrolls on them and leaves the cursor where it was — off screen,
	// so the next j or Enter acts on a row the operator can no longer see.
	if top, ok := treeEdgeKey(ev); ok {
		if n := treeEdgeNode(croot, top); n != nil {
			ctree.SetCurrentNode(n)
		}
		return nil
	}
	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case km.Search:
			u.startSearch("containers")
			return nil
		case km.ContainerInspect:
			u.inspectCurrent()
			return nil
		case km.Logs:
			if n := ctree.GetCurrentNode(); n != nil {
				u.showLogsForNode(n)
			}
			return nil
		case km.SecurityRisks:
			u.showSecurityRisks()
			return nil
		case 'X':
			// Remove the service under the cursor, without the detour through
			// the inspect overlay. Destructive, so it is a fixed capital key
			// (like X in the inspect) and always behind a confirm.
			t.removeUnderCursor()
			return nil
		case km.Fold:
			// Collapse. tview's TreeView has no fold key — Left/Right only
			// move the cursor — so fold explicitly. On a node that cannot
			// fold (a container leaf, or an already-closed service inside a
			// stack), step out to the parent instead, so repeated presses
			// walk up: container → service → stack.
			if n := ctree.GetCurrentNode(); n != nil {
				switch {
				case isStackNode(n):
					n.SetExpanded(false)
					t.markStack(n)
				case isServiceNode(n) && n.IsExpanded():
					n.SetExpanded(false)
					t.markService(n)
				default:
					if p := parentOf(croot, n); p != nil && p != croot {
						ctree.SetCurrentNode(p)
					}
				}
			}
			return nil
		case km.Unfold:
			// Expand the node under the cursor; if it is already open,
			// descend into it.
			if n := ctree.GetCurrentNode(); n != nil && (isServiceNode(n) || isStackNode(n)) {
				if n.IsExpanded() && len(n.GetChildren()) > 0 {
					ctree.SetCurrentNode(n.GetChildren()[0])
				} else {
					n.SetExpanded(true)
					if isStackNode(n) {
						t.markStack(n)
					} else {
						t.markService(n)
					}
				}
			}
			return nil
		case km.StackFile:
			u.openStackFileMenu()
			return nil
		case km.StackGroup:
			// Toggle stack grouping. Only meaningful once something carries a
			// stack label; say so rather than redrawing an identical tree.
			if !anyStacked(u.lastSvcs) {
				u.flash(" [gray]no service carries a stack label[white]")
				return nil
			}
			t.groupByStack = !t.groupByStack
			t.render()
			if t.groupByStack {
				u.flash(" [green]grouped by stack[white]")
			} else {
				u.flash(" [green]flat service list[white]")
			}
			return nil
		case km.Forward:
			// On a service node, forward to the task under the cursor —
			// exactly one, like kubectl does with a pod. Forwarding "the
			// service" would have to load-balance, which makes debugging
			// misleading.
			if n := ctree.GetCurrentNode(); n != nil {
				if c, ok := n.GetReference().(resolve.Candidate); ok {
					u.portPrompt(c)
				} else if kids := n.GetChildren(); len(kids) > 0 {
					if c, ok := kids[0].GetReference().(resolve.Candidate); ok {
						u.portPrompt(c)
					}
				}
			}
			return nil
		}
	}
	return u.tabKeys(ev)
}

// treeEdgeKey reports whether ev asks for the first (top) or last row: g / Home
// or G / End.
func treeEdgeKey(ev *tcell.EventKey) (top, ok bool) {
	switch {
	case ev.Key() == tcell.KeyHome, ev.Key() == tcell.KeyRune && ev.Rune() == 'g':
		return true, true
	case ev.Key() == tcell.KeyEnd, ev.Key() == tcell.KeyRune && ev.Rune() == 'G':
		return false, true
	}
	return false, false
}

// treeEdgeNode returns the first or last row the tree shows under root (which
// itself is hidden): the first top-level node, or the deepest last node along
// the expanded branches.
func treeEdgeNode(root *tview.TreeNode, top bool) *tview.TreeNode {
	kids := root.GetChildren()
	if len(kids) == 0 {
		return nil
	}
	if top {
		return kids[0]
	}
	n := kids[len(kids)-1]
	for n.IsExpanded() && len(n.GetChildren()) > 0 {
		c := n.GetChildren()
		n = c[len(c)-1]
	}
	return n
}
