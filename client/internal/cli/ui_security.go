// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/secscan"
)

// showSecurityRisks opens a modal listing every service the security scan
// flagged, grouped by service, each finding tinted by severity. It reads the
// findings already cached on the service list (no extra manager call), so new
// analyzers surface here automatically. Bound to '!' on the containers tab. The
// service under the cursor, if flagged, is pre-highlighted and scrolled into view.
func (u *ui) showSecurityRisks() {
	app, pages := u.app, u.pages

	var flagged []resolve.Service
	for _, s := range u.lastSvcs {
		if secscan.Actionable(s.Risks) {
			flagged = append(flagged, s)
		}
	}

	tv := tview.NewTextView().SetDynamicColors(true).SetRegions(true).SetScrollable(true)
	tv.SetBorder(true).SetTitle(" security risks ")

	switch {
	case len(u.lastSvcs) == 0:
		// No service list yet (first fetch pending, or it failed). Saying "no
		// risks" here would be a false all-clear — we simply have no data.
		tv.SetText("\n  [yellow]No services loaded — nothing has been scanned yet.[-]\n\n" +
			"  [gray]Refresh the containers tab (r) once the manager is reachable.[-]")
	case len(flagged) == 0:
		tv.SetText(fmt.Sprintf("\n  [green]No security risks identified[-] [gray]across %d service(s).[-]\n\n"+
			"  [gray]Checks: containers running as root, secrets in environment variables.[-]", len(u.lastSvcs)))
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "  [gray]%d of %d service(s) flagged — checks: root user, secret in env[-]\n",
			len(flagged), len(u.lastSvcs))
		for i, s := range flagged {
			// Region id (index-based, so odd service names can't break the tag).
			// Every spec-derived string is escaped: Docker does not restrict env
			// key or User characters, so a "[" run would otherwise be parsed as a
			// colour tag and swallow the rest of the overlay.
			fmt.Fprintf(&b, "\n[\"s%d\"][aqua]%s[-]  %s[\"\"]\n", i, tview.Escape(s.Name), sevMarker(secscan.MaxSeverity(s.Risks)))
			for _, f := range s.Risks {
				fmt.Fprintf(&b, "  %s  [white]%s[-] — [gray]%s[-]\n",
					sevMarker(f.Severity), tview.Escape(f.Title), tview.Escape(f.Detail))
			}
		}
		tv.SetText(strings.TrimLeft(b.String(), "\n"))

		// Pre-select the service under the cursor if it is among the flagged ones.
		if sel := u.currentServiceName(); sel != "" {
			for i, s := range flagged {
				if s.Name == sel {
					tv.Highlight(fmt.Sprintf("s%d", i)).ScrollToHighlight()
					break
				}
			}
		}
	}

	prev := app.GetFocus()
	_, restore := u.pushOverlayHelp(footerKeys("j/k", "scroll", "Esc", "close"))
	closeIt := func() { restore(); pages.RemovePage(pageSecurity); app.SetFocus(prev) }
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		// Close on Esc, on the configured quit key, and on the same key that
		// opened the overlay — both are remappable, so read them from the keymap
		// rather than hardcoding 'q' / '!'.
		case ev.Key() == tcell.KeyEscape ||
			(ev.Key() == tcell.KeyRune && (ev.Rune() == u.km.Quit || ev.Rune() == u.km.SecurityRisks)):
			closeIt()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	})
	pages.AddPage(pageSecurity, centered(tv, 84, 26), true, true)
	app.SetFocus(tv)
}

// currentServiceName returns the service name under the tree cursor — the
// service node itself, or the service owning a selected container leaf.
func (u *ui) currentServiceName() string {
	n := u.ctree.GetCurrentNode()
	if n == nil {
		return ""
	}
	switch ref := n.GetReference().(type) {
	case svcRef:
		return ref.name
	case resolve.Candidate:
		return ref.Service
	}
	return ""
}

// sevMarker renders a severity-coloured dot + label for the risks overlay.
func sevMarker(s secscan.Severity) string {
	switch s {
	case secscan.SevHigh:
		return "[red]● high[-]"
	case secscan.SevMedium:
		return "[yellow]● medium[-]"
	default:
		return "[gray]● low[-]"
	}
}
