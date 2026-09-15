// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/stackfile"
)

// The stacks tab can write a deployed stack out as a compose file, and compare
// one against a file on disk. Both are the same rendering read two ways, and
// both run off the ui goroutine: they list every service, network, secret and
// config in the cluster, which on a slow manager takes long enough to freeze
// the interface mid-keystroke if done inline.

// stackUnderCursor is the stack the current row belongs to — the stack row
// itself, a service in it, or the service owning a selected container. Without
// the last two, the key would only work on a row the operator may have folded
// away.
func (u *ui) stackUnderCursor() string {
	n := u.ctree.GetCurrentNode()
	if n == nil {
		return ""
	}
	switch ref := n.GetReference().(type) {
	case stackRef:
		return ref.name
	case svcRef:
		return u.stackOfService(ref.name)
	case resolve.Candidate:
		return u.stackOfService(ref.Service)
	}
	return ""
}

// stackOfService reads the stack from the cached service list rather than
// splitting the name on "_": a service called "a_b_c" is ambiguous, and the
// manager already told us which stack it belongs to.
func (u *ui) stackOfService(name string) string {
	for _, s := range u.lastSvcs {
		if s.Name == name {
			return s.Stack
		}
	}
	return ""
}

// openStackFileMenu offers the two things that can be done with a stack file.
func (u *ui) openStackFileMenu() {
	stack := u.stackUnderCursor()
	if stack == "" {
		u.flash("no stack under the cursor — select a stack, service or container")
		return
	}
	app, pages, ctree := u.app, u.pages, u.ctree
	list := tview.NewList().ShowSecondaryText(true)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" stack %s ", stack))
	_, restoreHelp := u.pushOverlayHelp(footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
	closeMenu := func() { restoreHelp(); pages.RemovePage(pageMenu); app.SetFocus(ctree) }

	list.AddItem("Export to a file", "write the deployed stack as compose YAML", 0, func() {
		closeMenu()
		u.promptStackPath(" export "+stack+" to ", defaultStackPath(stack), func(path string) {
			u.exportStack(stack, path)
		})
	})
	list.AddItem("Compare with a file", "show what deploying that file would change", 0, func() {
		closeMenu()
		u.promptStackPath(" compare "+stack+" with ", defaultStackPath(stack), func(path string) {
			u.diffStack(stack, path)
		})
	})
	list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == u.km.Quit) {
			closeMenu()
			return nil
		}
		return ev
	})
	pages.AddPage(pageMenu, centered(list, 56, 8), true, true)
	app.SetFocus(list)
}

func (u *ui) promptStackPath(title, preset string, then func(string)) {
	pages, app, ctree := u.pages, u.app, u.ctree
	in := tview.NewInputField().SetLabel("file: ").SetFieldWidth(52).SetText(preset)
	in.SetDoneFunc(func(key tcell.Key) {
		pages.RemovePage(pageStackFile)
		app.SetFocus(ctree)
		if key == tcell.KeyEscape {
			return
		}
		if path := strings.TrimSpace(in.GetText()); path != "" {
			then(path)
		}
	})
	in.SetBorder(true).SetTitle(title)
	pages.AddPage(pageStackFile, centeredPrompt(in, 72), true, true)
	app.SetFocus(in)
}

func (u *ui) exportStack(stack, path string) {
	ctx, app, dcli := u.ctx, u.app, u.dcli
	u.flash("exporting " + stack + "…")
	go func() {
		st, err := stackfile.FromSwarm(ctx, dcli, stack)
		var doc string
		if err == nil {
			doc, err = st.Document()
		}
		if err == nil {
			err = writeReportFile(path, doc)
		}
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("export failed: " + err.Error())
				return
			}
			// The notes matter more than the count: an export is a description
			// of the stack, not a backup, and this is the moment to say so.
			msg := fmt.Sprintf("wrote %s — %d service(s)", path, len(st.Services))
			if len(st.Notes) > 0 {
				msg += "\n\n" + strings.Join(st.Notes, "\n\n")
			}
			u.info(msg)
		})
	}()
}

func (u *ui) diffStack(stack, path string) {
	ctx, app, dcli := u.ctx, u.app, u.dcli
	u.flash("comparing " + stack + " with " + path + "…")
	go func() {
		deployed, err := stackfile.FromSwarm(ctx, dcli, stack)
		var d *stackfile.Diff
		if err == nil {
			var file *stackfile.Stack
			file, err = stackfile.FromFile(ctx, dcli, path, stack)
			if err == nil {
				d, err = stackfile.Compare(deployed, file, path)
			}
		}
		app.QueueUpdateDraw(func() {
			if err != nil {
				u.info("compare failed: " + err.Error())
				return
			}
			u.showStackDiff(stack, path, d)
		})
	}()
}

// showStackDiff renders the diff with the colouring a reader expects from a
// diff: additions green, removals red, hunk headers dim. The text is escaped —
// a stack file is user data and may contain anything, including tview's own
// colour tags.
func (u *ui) showStackDiff(stack, path string, d *stackfile.Diff) {
	app, pages, ctree := u.app, u.pages, u.ctree
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
	tv.SetBorder(true).SetTitle(fmt.Sprintf(" %s  vs  deployed %s ", path, stack))

	var b strings.Builder
	if d.Empty() {
		fmt.Fprintf(&b, "\n  [green]No differences[-] [gray]between %s and the deployed stack.[-]\n",
			tview.Escape(path))
	} else {
		fmt.Fprintf(&b, "  [gray]- deployed %s    + %s[-]\n\n", tview.Escape(stack), tview.Escape(path))
		for _, h := range d.Hunks {
			fmt.Fprintf(&b, "[gray]@@ -%d,%d +%d,%d @@[-]\n", h.DeployStart, h.DeployCount, h.FileStart, h.FileCount)
			for _, l := range h.Lines {
				text := tview.Escape(l.Text)
				switch l.Op {
				case '+':
					fmt.Fprintf(&b, "[green]+%s[-]\n", text)
				case '-':
					fmt.Fprintf(&b, "[red]-%s[-]\n", text)
				default:
					fmt.Fprintf(&b, "[gray] %s[-]\n", text)
				}
			}
			b.WriteString("\n")
		}
	}
	// The caveats belong with the verdict, especially the clean one.
	for _, n := range d.Notes {
		fmt.Fprintf(&b, "  [gray]not compared: %s[-]\n", tview.Escape(n))
	}
	tv.SetText(b.String())

	_, restore := u.pushOverlayHelp(footerKeys("j/k", "scroll", "Esc", "close"))
	closeIt := func() { restore(); pages.RemovePage(pageStackDiff); app.SetFocus(ctree) }
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == u.km.Quit):
			closeIt()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	})
	pages.AddPage(pageStackDiff, centered(tv, 100, 32), true, true)
	app.SetFocus(tv)
}

// defaultStackPath is what the prompt offers: the stack's own name, in the
// working directory, so the common case is one keypress.
func defaultStackPath(stack string) string { return safeFileName(stack) + ".yml" }
