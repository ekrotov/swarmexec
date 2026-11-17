// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/rivo/tview"
)

// A tab used to be five `switch u.active` statements — helpFor, setTab,
// yankCurrent, the refresh key in tabKeys and focusActiveTab — plus fields on
// *ui and *clusterState that any method could reach. Adding a tab meant finding
// all five, and forgetting one failed silently: the Configs and Nodes tabs fell
// into the switches' default branches, so their refresh key repainted the
// forwards table and their copy key copied the volume list.
//
// tabView is the alternative: what the session needs from a tab, asked of the
// tab. The registry (newTabViews) maps every key in uiTabList to its view, and
// the former switches are a lookup.
type tabView interface {
	// widget is the tab's content page: what the content Pages shows and what
	// takes the keyboard when the tab is entered or the sidebar lets go.
	widget() tview.Primitive
	// enter runs once the tab is visible and focused: kick its load, or
	// repaint from what is cached.
	enter()
	// refresh is the refresh key on this tab. The context list and the cluster
	// census are refreshed by tabKeys whatever the tab, so a tab only reloads
	// its own data.
	refresh()
	// footer is the tab's key hints, after the universal "? help · q quit"
	// head that helpFor puts in front of every footer.
	footer() string
	// helpSection writes the tab's entries in the "?" overlay.
	helpSection(h *helpWriter)
	// yank writes the tab's list as the clipboard receives it.
	yank(b *strings.Builder)
}

// tabSearcher is a tab the shared "/" bar can filter. The bar is one widget;
// which tab it filters is the tab that opened it (startSearch).
type tabSearcher interface {
	searchText() string
	setSearch(q string) // apply the query: filter locally and repaint
	searchPlaceholder() string
}

// tabStatusNoter is a tab that adds a note to the status line while it is the
// visible tab (the volumes tab's selection count).
type tabStatusNoter interface {
	statusNote() string
}

// newTabViews is the tab registry: one view per uiTabList key.
//
// A function rather than a field on uiTab, deliberately. uiTabList is a
// package-level var, and a constructor reachable from it reaches tabKeys,
// which reads uiTabList — Go refuses that as an initialisation cycle. The
// registry test (TestEveryTabIsRegistered) keeps the two lists in step.
func newTabViews(u *ui) map[string]tabView {
	return map[string]tabView{
		"containers": newTreeTab(u),
		"volumes":    newVolumesTab(u),
		"forwards":   newForwardsTab(u),
		"networks":   newNetworksTab(u),
		"secrets":    newSecretsTab(u),
		"nodes":      newNodesTab(u),
		"configs":    newConfigsTab(u),
	}
}

// tab returns the view registered under key, or nil for a key that is not a
// tab ("contexts" is the sidebar). The registry is built on first use, so a
// test that builds a bare *ui gets it as well as run() does. Not before run(),
// though: every tab builds its widget here, and tview reads the theme's
// palette when a widget is constructed (applyTheme runs just before run).
func (u *ui) tab(key string) tabView {
	if u.tabs == nil {
		u.tabs = newTabViews(u)
	}
	return u.tabs[key]
}

// activeTab is the visible tab's view.
func (u *ui) activeTab() tabView { return u.tab(u.active) }

// perCluster is the per-cluster half of a tab's state.
//
// A tab is ONE value per session: it owns its widget and the operator's
// preferences (a sort order), which a cluster switch must not reset. What it
// shows, the filter and the cursor belong to a cluster, and come back with
// it. So the tab keeps one S per cluster, keyed by the cluster's state
// pointer, and *clusterState knows nothing about tabs. A tab instance per
// cluster would have meant one widget per cluster too — re-adding content
// pages on every switch — for no gain.
type perCluster[S any] struct {
	m    map[*clusterState]*S
	init func() *S
}

// of returns c's state, creating it on first use.
func (p *perCluster[S]) of(c *clusterState) *S {
	if p.m == nil {
		p.m = map[*clusterState]*S{}
	}
	s, ok := p.m[c]
	if !ok {
		s = p.init()
		p.m[c] = s
	}
	return s
}

// helpWriter collects the "?" overlay: a section title, then one key per line.
// It records entries rather than text because the text depends on the width
// it is shown at: a description too long for the box wraps under itself, not
// under the key column (see text).
type helpWriter struct{ entries []helpEntry }

// helpEntry is one line of the help: a section title, or a key and what it does.
type helpEntry struct {
	title      string // a section heading; keys and desc are empty
	keys, desc string
	danger     bool
}

func (h *helpWriter) sec(title string) { h.entries = append(h.entries, helpEntry{title: title}) }

func (h *helpWriter) line(keys, desc string) {
	h.entries = append(h.entries, helpEntry{keys: keys, desc: desc})
}

// danger is line for a destructive key (delete / remove / stop / detach): red, as in
// the footers. One key per line, so the colour never spills onto a harmless
// neighbour sharing the same column.
func (h *helpWriter) danger(keys, desc string) {
	h.entries = append(h.entries, helpEntry{keys: keys, desc: desc, danger: true})
}

// helpKeyCol is the width of the key column; a longer key pushes its
// description right instead of being cut.
const helpKeyCol = 9

// String is the help unwrapped, for tests and anything without a width.
func (h *helpWriter) String() string { return h.text(0) }

// text renders the help for a box width columns wide (0: no wrapping). A
// description that does not fit wraps with a hanging indent — its later lines
// start where its first one did — so the key column stays a column and the
// eye can run down it.
func (h *helpWriter) text(width int) string {
	var b strings.Builder
	for i, e := range h.entries {
		if e.title != "" {
			if i > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "[aqua]%s[-]\n", e.title)
			continue
		}
		color := "yellow"
		if e.danger {
			color = "red"
		}
		kw := tview.TaggedStringWidth(e.keys)
		fmt.Fprintf(&b, "  [%s]%s[white]%s ", color, e.keys, strings.Repeat(" ", max(helpKeyCol-kw, 0)))
		indent := 2 + max(kw, helpKeyCol) + 1
		lines := wrapWords(e.desc, width-indent)
		b.WriteString(strings.Join(lines, "\n"+strings.Repeat(" ", indent)))
		b.WriteByte('\n')
	}
	return b.String()
}

// wrapWords breaks s into lines of at most width columns at spaces (a word
// longer than width gets a line of its own). width < 1 means no wrapping —
// and so does a box too narrow to leave the description a useful column,
// where the TextView's own wrapping does better than a column of one-word
// lines.
func wrapWords(s string, width int) []string {
	if width < 12 {
		return []string{s}
	}
	var lines []string
	cur, curW := "", 0
	for _, w := range strings.Fields(s) {
		ww := tview.TaggedStringWidth(w)
		if curW > 0 && curW+1+ww > width {
			lines = append(lines, cur)
			cur, curW = "", 0
		}
		if curW > 0 {
			cur += " "
			curW++
		}
		cur += w
		curW += ww
	}
	return append(lines, cur)
}
