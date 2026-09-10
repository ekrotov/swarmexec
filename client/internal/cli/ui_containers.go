// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/resolve"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func (u *ui) matchesFilter(c resolve.Candidate) bool {
	if u.filter == "" {
		return true
	}
	q := strings.ToLower(u.filter)
	return strings.Contains(strings.ToLower(c.Service), q) ||
		strings.Contains(strings.ToLower(c.ContainerID), q) ||
		strings.Contains(strings.ToLower(c.NodeName), q)
}

func (u *ui) markService(n *tview.TreeNode) {
	ctx := u.ctx
	ref, ok := n.GetReference().(svcRef)
	if !ok {
		return
	}
	svc := u.svcByName[ref.name]
	suffix := ""
	if u.regCache != nil {
		suffix = versionSuffix(u.regCache.status(ctx, svc.ImageRef))
	}
	row := serviceRow(svc, u.svcCols, suffix)
	switch {
	case len(n.GetChildren()) == 0:
		n.SetText("  " + row)
	case n.IsExpanded():
		n.SetText("▾ " + row)
	default:
		n.SetText("▸ " + row)
	}
}

func (u *ui) renderContainers() {
	forwards, ctree, croot := u.forwards, u.ctree, u.croot
	// Remember the cursor (a leaf by container id, else a service by name)
	// and which services were expanded, so a refresh keeps both.
	prevID, prevSvc := "", ""
	if n := ctree.GetCurrentNode(); n != nil {
		if ref, ok := n.GetReference().(resolve.Candidate); ok {
			prevID = ref.ContainerID
		} else if ref, ok := n.GetReference().(svcRef); ok {
			prevSvc = ref.name
		}
	}
	prevStack := ""
	if n := ctree.GetCurrentNode(); n != nil {
		if ref, ok := n.GetReference().(stackRef); ok {
			prevStack = ref.name
		}
	}
	// Remember fold state for both levels. Stacks default to expanded (so the
	// grouped tree shows the same services the flat one did), services stay
	// collapsed by default as before.
	wasExpanded := map[string]bool{}
	stackExpanded := map[string]bool{}
	seenStack := map[string]bool{}
	croot.Walk(func(n, _ *tview.TreeNode) bool {
		switch ref := n.GetReference().(type) {
		case svcRef:
			wasExpanded[ref.name] = n.IsExpanded()
			return false
		case stackRef:
			stackExpanded[ref.name] = n.IsExpanded()
			seenStack[ref.name] = true
		}
		return true
	})
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

	// Column widths for the docker service ls-style service rows, computed
	// over every service so the alignment stays stable while filtering.
	u.svcCols = svcColumns{}
	u.svcByName = make(map[string]resolve.Service, len(u.lastSvcs))
	for _, s := range u.lastSvcs {
		u.svcByName[s.Name] = s
		if w := len(orDash(s.Name)); w > u.svcCols.name {
			u.svcCols.name = w
		}
		if w := len(orDash(s.Mode)); w > u.svcCols.mode {
			u.svcCols.mode = w
		}
		if w := len(fmt.Sprintf("%d/%d", s.Running, s.Desired)); w > u.svcCols.repl {
			u.svcCols.repl = w
		}
		if w := len(s.Image); w > u.svcCols.image {
			u.svcCols.image = w
		}
	}

	q := strings.ToLower(strings.TrimSpace(u.filter))
	var firstSvc, targetSvc, targetLeaf, targetStack *tview.TreeNode

	// Group by stack unless the operator turned it off, and only when at least
	// one service actually carries a stack label — otherwise every row would
	// hang under a single pointless "(no stack)" parent.
	grouped := u.groupByStack && anyStacked(u.lastSvcs)
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
			if nameMatch || u.matchesFilter(c) {
				shown = append(shown, c)
			}
		}
		// Hide a service only if it neither matches by name nor has any
		// matching container.
		if !nameMatch && len(shown) == 0 {
			continue
		}
		// Collapsed by default (spec); keep a service the operator expanded.
		svcNode := tview.NewTreeNode(serviceRow(s, u.svcCols, "")).
			SetColor(serviceColor(s.Running, s.Desired)).
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
			leaf := tview.NewTreeNode(annotateForwards(label, forwards.forContainer(c.ContainerID))).SetReference(c)
			svcNode.AddChild(leaf)
			if c.ContainerID == prevID {
				targetLeaf = leaf
				svcNode.SetExpanded(true) // reveal the previously-selected leaf
			}
		}
		// Marker depends on the final child count / expanded state, so set it
		// once the leaves are attached.
		u.markService(svcNode)
	}
	if grouped {
		// Drop stacks the filter emptied, and label the rest — the summary counts
		// what actually ended up under each, so it follows the filter.
		kept := make([]*tview.TreeNode, 0, len(croot.GetChildren()))
		for _, n := range croot.GetChildren() {
			if isStackNode(n) && len(n.GetChildren()) == 0 {
				continue
			}
			u.markStack(n)
			kept = append(kept, n)
		}
		croot.SetChildren(kept)
	}
	if len(croot.GetChildren()) == 0 {
		empty := "(no services)"
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
func (u *ui) markStack(n *tview.TreeNode) {
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
	n.SetText(fmt.Sprintf("%s %s  [gray](%d svc · %d/%d)[-]%s",
		marker, ref.name, len(summary.Services), summary.Running, summary.Desired, extra.String()))
	n.SetColor(serviceColor(summary.Running, summary.Desired))
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

func (u *ui) sortCands(cands []resolve.Candidate) {
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

func (u *ui) fetchContainers() ([]resolve.Service, []resolve.Candidate, error) {
	r, ctx := u.r, u.ctx
	start := time.Now()
	svcs, err := r.Services(ctx)
	clientlog.Timed("ui.containers.Services", start, err)
	if err != nil {
		return nil, nil, err
	}
	// Fetch every running container (not just the CLI-arg service); the tree
	// filters client-side so services with 0 containers still appear.
	start = time.Now()
	cands, err := r.Candidates(ctx, "")
	clientlog.Timed("ui.containers.Candidates", start, err)
	if err != nil {
		return nil, nil, err
	}
	u.sortCands(cands)
	return svcs, cands, nil
}

func (u *ui) applyContainers(svcs []resolve.Service, cands []resolve.Candidate, err error) {
	croot := u.croot
	if err != nil {
		croot.ClearChildren()
		croot.AddChild(tview.NewTreeNode("error: " + err.Error()).SetColor(tcell.ColorRed).SetSelectable(false))
		return
	}
	u.lastSvcs = svcs
	u.lastCands = cands
	u.renderContainers()
}

func (u *ui) loadContainersSync() {
	svcs, cands, err := u.fetchContainers()
	u.applyContainers(svcs, cands, err)
}

func (u *ui) loadContainers() {
	app := u.app
	go func() {
		svcs, cands, err := u.fetchContainers()
		app.QueueUpdateDraw(func() { u.applyContainers(svcs, cands, err) })
	}()
}

func (u *ui) autoRefreshContainers() {
	app, ctx := u.app, u.ctx
	if u.overlayDepth.Load() > 0 || !u.autoRefreshBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer u.autoRefreshBusy.Store(false)
		svcs, cands, err := u.fetchContainers()
		if err != nil || ctx.Err() != nil || u.overlayDepth.Load() > 0 {
			return // transient error, run ending, or an overlay opened meanwhile
		}
		app.QueueUpdateDraw(func() { u.applyContainers(svcs, cands, nil) })
	}()
}

func (u *ui) openTerminal(c resolve.Candidate, command []string, tty bool, user string) {
	app, pages, cfg, dcli, f, ctx, ctree := u.app, u.pages, u.cfg, u.dcli, u.f, u.ctx, u.ctree
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
	var once sync.Once
	closeTerm := func() {
		once.Do(func() {
			app.EnableMouse(prevMouse)
			tcancel()
			pages.RemovePage(pageTerm)
			app.SetFocus(ctree)
			u.loadContainers()
		})
	}
	tv.detach = closeTerm
	tv.run(tctx, cfg, ep, command, tty, user, f.connectTimeout, func(code int, rerr error) {
		app.QueueUpdateDraw(func() {
			switch {
			case tctx.Err() != nil:
				closeTerm() // user detached (Ctrl-]) — just close
			case rerr != nil:
				closeTerm()
				u.info(enrichAgentError(ctx, dcli, rerr).Error())
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
	pages.AddPage(pageTerm, tv, true, true)
	app.SetFocus(tv)
}

func (u *ui) containerMenu(c resolve.Candidate) {
	app, pages, cfg, f, ctx, ctree := u.app, u.pages, u.cfg, u.f, u.ctx, u.ctree
	ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" %s on %s — checking shells… ", orDash(c.Service), orDash(c.NodeName)))
	_, restoreHelp := u.pushOverlayHelp(footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
	closeMenu := func() { restoreHelp(); pages.RemovePage(pageMenu); app.SetFocus(ctree) }

	// Optimistic until the shell probe returns; then unavailable shells grey.
	bashOK, shOK, probed := true, true, false
	render := func() {
		cur := list.GetCurrentItem()
		list.Clear()
		list.AddItem("Logs", "", 0, func() { closeMenu(); u.showLogs(c) })
		list.AddItem(shellLabel("Bash", bashOK, probed), "", 0, func() {
			if bashOK {
				closeMenu()
				u.openTerminal(c, []string{"bash"}, true, "")
			}
		})
		list.AddItem(shellLabel("Sh", shOK, probed), "", 0, func() {
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
		list.AddItem("Shell as user…", "", 0, func() {
			closeMenu()
			u.userPrompt(c, func(usr string) {
				if bashOK {
					u.openTerminal(c, []string{"bash"}, true, usr)
				} else {
					u.openTerminal(c, []string{"sh"}, true, usr) // PTY: ^C → SIGINT, line editing
				}
			})
		})
		list.AddItem("Port forward", "", 0, func() { closeMenu(); u.portPrompt(c) })
		list.AddItem("Cancel", "", 0, closeMenu)
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

	go func() {
		b, s := probeShells(ctx, cfg, ep, f.connectTimeout)
		app.QueueUpdateDraw(func() {
			bashOK, shOK, probed = b, s, true
			list.SetTitle(fmt.Sprintf(" %s on %s — actions ", orDash(c.Service), orDash(c.NodeName)))
			render()
		})
	}()

	// Height tracks the item count: 6 items plus the border.
	pages.AddPage(pageMenu, centered(list, 48, 8), true, true)
	app.SetFocus(list)
}
