// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/dockerctx"
)

// uiTabList drives the tab bar: one list entry per tab instead of five
// hand-aligned strings, so adding a tab is a single edit. The 1-based shortcut
// digit is derived from position by the tab bar, not baked into the label.
// uiTab is one tab: the internal key, the full label, and a short label the tab
// bar falls back to when the terminal is too narrow for the full set. The short
// forms are written out rather than derived, because initials collide
// (Secrets/Stacks, Networks/Nodes).
type uiTab struct{ key, label, short string }

var uiTabList = []uiTab{
	// The label names what the tree shows (stacks → services → containers); the
	// key stays "containers" because it addresses the tab internally — keymap
	// scopes, the search mode and the content page all key off it.
	{"containers", "Stacks/Services", "St/Sv"},
	{"volumes", "Volumes", "Vol"},
	{"forwards", "Forwards", "Fwd"},
	{"networks", "Networks", "Net"},
	{"secrets", "Secrets", "Sec"},
	// Contexts is deliberately NOT a tab. Switching cluster is something you do
	// from wherever you are, not a place you travel to — and since the switch
	// stopped rebuilding the session (F14) it became frequent enough that making
	// it a destination read as a detour. It lives in the sidebar instead.
	{"nodes", "Nodes", "Node"},
	{"configs", "Configs", "Cfg"},
}

// uiTabOrder drives Tab cycling; every tab joins it.
var uiTabOrder = []string{"containers", "volumes", "forwards", "networks", "secrets", "nodes", "configs"}

// Column headers for the per-tab tables — constant data shared by each tab's
// render/load methods, so they live at package scope rather than as run() locals.
var (
	vHeaders  = []string{"VOLUME", "DRIVER", "NODES", "USED BY", "AGE", "SIZE"}
	fHeaders  = []string{"LOCAL", "REMOTE", "CONTAINER", "SERVICE", "NODE", "CLUSTER", "AGE", "STATE"}
	nHeaders  = []string{"NETWORK", "DRIVER", "SCOPE", "TYPE", "ENC", "SERVICES", "AGE"}
	sHeaders  = []string{"SECRET", "USED BY", "AGE", "UPDATED", "LABELS"}
	cfHeaders = []string{"CONFIG", "USED BY", "SIZE", "AGE", "UPDATED", "LABELS"}
	// The sidebar is narrow and its rows are self-explanatory, so it carries no
	// header at all; the endpoint each name resolves to is one keystroke away in
	// the detail overlay. cxHeaders survives for the clipboard export, which is
	// a table and does want columns.
	cxHeaders = []string{"CONTEXT", "DOCKER HOST"}
	noHeaders = []string{"NODE", "ROLE", "AVAIL", "STATE", "ENGINE", "TASKS", "VOLS", "LABELS"}
)

// Volume sort fields and the header column each annotates with ▲/▼.
const (
	volSortName = iota
	volSortNodes
	volSortUsed
	volSortAge
	volSortSize
)

// volSortCol maps a sort field to the header column it annotates with ▲/▼.
var volSortCol = map[int]int{volSortName: 0, volSortNodes: 2, volSortUsed: 3, volSortAge: 4, volSortSize: 5}

// ui holds one UI session's ambient state: the run-wide infrastructure, the
// shared widgets and the per-tab caches that the (formerly closure) methods
// capture. runUI builds it and hands off to (*ui).run; the closures still living
// in run() reference these fields, and the infra methods are methods on *ui.
type ui struct {
	// The visible cluster. Embedded by pointer so `u.dcli`, `u.vols`, `u.filter`
	// and friends keep resolving; switching cluster assigns this one field.
	*clusterState

	// clusters is every cluster this session has visited, by docker context
	// name. A switch away keeps the entry — that is what makes the switch back
	// instant and land where the operator was.
	clusters map[string]*clusterState

	// gen rises on every switch; background loads capture it and drop their
	// result if it has moved. See (*ui).onCluster.
	gen atomic.Uint64

	// eventsLive says whether the manager's topology event stream is currently
	// subscribed. It picks the poll rate, so it lives on the session rather than
	// on a cluster: the ticker is one, and it always follows the visible one.
	eventsLive atomic.Bool

	// run-wide infrastructure (created in runUI, before the event loop)
	app      *tview.Application
	pages    *tview.Pages // overlays: menus, terminal, logs, dialogs
	content  *tview.Pages // the per-tab pages
	km       keybinds
	f        *uiFlags
	g        *globalFlags
	cmd      *cobra.Command // kept: a later cluster's config is resolved from the same flags
	service  string
	selStyle tcell.Style

	// runCtx outlives a cluster switch and ends only when the UI does. Port
	// forwards take it, so switching cluster no longer kills them.
	runCtx    context.Context
	cancelRun context.CancelFunc

	// forwards is shared across clusters on purpose: a forward is a local
	// listener the operator started and expects to keep, and tearing it down
	// because they looked at another cluster is the behaviour this feature
	// exists to remove. Each entry records which cluster it belongs to.
	forwards *forwardRegistry

	// shared widgets (built in run(), once the tree/tables exist)
	ctree                                                *tview.TreeView
	croot                                                *tview.TreeNode
	vtable, ftable, nettable, sectable, cxtable, notable *tview.Table
	cfgtable                                             *tview.Table
	tabBar                                               *tabStrip
	help, status                                         *tview.TextView
	footer, root                                         *tview.Flex
	search                                               *tview.InputField

	// The context sidebar: `body` is the horizontal split holding the tab
	// content and `sidebar` (the framed context list) side by side.
	body, sidebar *tview.Flex
	// sidebarReturn is the footer text to put back when the sidebar gives the
	// keyboard up again; sidebarHasFocus mirrors where the keyboard is (see
	// sidebarFocused — the draw path cannot ask the application).
	sidebarReturn   string
	sidebarHasFocus bool

	// Preferences that belong to the operator's session, not to a cluster —
	// they must survive a switch rather than reset with it.
	groupByStack bool // nest services under their stack (toggled by the stack_group key)
	sortField    int  // volumes sort column
	sortDesc     bool

	// shared tab state
	active       string       // the selected tab
	activeCtx    string       // active docker-context name
	curHelp      string       // current tab's footer help
	footerBase   string       // what the footer's owner wants shown; a flash returns to THIS
	savedHelp    string       // footer help saved while search is open
	searchMode   string       // what the "/" bar filters ("containers"/"volumes")
	mouseEnabled bool         // mirrors app.EnableMouse; toggled by 'm'
	screen       tcell.Screen // owned screen, for OSC52 clipboard on yank

	// overlays names every overlay currently open, so the background refreshers
	// can pause while one is.
	//
	// A SET, not a counter, and that is the whole point. Two notices racing from
	// two background goroutines both call info(): the second AddPage REPLACES
	// the first page, so only one modal is ever dismissed and only one release
	// ever runs. A counter would be incremented twice and decremented once, and
	// would never reach zero again — the tree would stop refreshing for the rest
	// of the session, silently. Keyed by page name, the second open is simply
	// the same entry.
	overlayMu  sync.Mutex
	overlays   map[string]bool
	overlaySeq atomic.Uint64 // unique keys for overlays that share no page name

	// switching guards the window between asking for a cluster and having it:
	// the connect runs off the UI goroutine, and a second switch started in the
	// meantime would race the first into the swap.
	switching bool

	// forwards tab cache: fRows mirrors the rendered table so a row maps to a forward.
	fRows []forwardEntry

	// The docker context list is the same whichever cluster is visible, so it
	// stays here rather than being fetched again per cluster.
	ctxs []dockerctx.Context

	// toggleable client-log overlay
	logViewStop    chan struct{}
	logViewPrev    tview.Primitive
	logViewRestore func()
}
