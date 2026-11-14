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
		"containers": containersTab{u},
		"volumes":    newVolumesTab(u),
		"forwards":   forwardsTab{u},
		"networks":   newNetworksTab(u),
		"secrets":    newSecretsTab(u),
		"nodes":      nodesTab{u},
		"configs":    newConfigsTab(u),
	}
}

// tab returns the view registered under key, or nil for a key that is not a
// tab ("contexts" is the sidebar). The registry is built on first use, so a
// test that builds a bare *ui gets it as well as run() does — run() asks for
// the widgets only after the shared ones the adapters read exist.
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

// helpWriter writes the "?" overlay: a section title, then one key per line.
type helpWriter struct{ b strings.Builder }

func (h *helpWriter) sec(title string) { fmt.Fprintf(&h.b, "\n[aqua]%s[-]\n", title) }

func (h *helpWriter) line(keys, desc string) {
	fmt.Fprintf(&h.b, "  [yellow]%-9s[white] %s\n", keys, desc)
}

// danger is line for a destructive key (delete / remove / stop): red, as in
// the footers. One key per line, so the colour never spills onto a harmless
// neighbour sharing the same column.
func (h *helpWriter) danger(keys, desc string) {
	fmt.Fprintf(&h.b, "  [red]%-9s[white] %s\n", keys, desc)
}
