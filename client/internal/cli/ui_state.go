// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"sync/atomic"

	"github.com/docker/docker/client"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/resolve"
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
	{"contexts", "Contexts", "Ctx"},
	{"nodes", "Nodes", "Node"},
	{"configs", "Configs", "Cfg"},
}

// uiTabOrder drives Tab cycling; every tab joins it.
var uiTabOrder = []string{"containers", "volumes", "forwards", "networks", "secrets", "contexts", "nodes", "configs"}

// Column headers for the per-tab tables — constant data shared by each tab's
// render/load methods, so they live at package scope rather than as run() locals.
var (
	vHeaders  = []string{"VOLUME", "DRIVER", "NODES", "USED BY", "AGE", "SIZE"}
	fHeaders  = []string{"LOCAL", "REMOTE", "CONTAINER", "SERVICE", "NODE", "AGE", "STATE"}
	nHeaders  = []string{"NETWORK", "DRIVER", "SCOPE", "TYPE", "ENC", "SERVICES", "AGE"}
	sHeaders  = []string{"SECRET", "USED BY", "AGE", "UPDATED", "LABELS"}
	cfHeaders = []string{"CONFIG", "USED BY", "SIZE", "AGE", "UPDATED", "LABELS"}
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
	// run-wide infrastructure (created in runUI, before the event loop)
	app         *tview.Application
	pages       *tview.Pages // overlays: menus, terminal, logs, dialogs
	content     *tview.Pages // the per-tab pages
	dcli        *client.Client
	r           *resolve.Resolver
	cfg         config.Config
	km          keybinds
	f           *uiFlags
	g           *globalFlags
	ctx         context.Context
	cancelRun   context.CancelFunc
	ctxOverride string
	service     string
	switchTo    string // set when a Contexts-tab activation asks for a restart
	selStyle    tcell.Style
	forwards    *forwardRegistry

	// shared widgets (built in run(), once the tree/tables exist)
	ctree                                                *tview.TreeView
	croot                                                *tview.TreeNode
	vtable, ftable, nettable, sectable, cxtable, notable *tview.Table
	cfgtable                                             *tview.Table
	tabBar                                               *tabStrip
	help, status                                         *tview.TextView
	footer, root                                         *tview.Flex
	search                                               *tview.InputField

	// containers tab caches
	lastCands       []resolve.Candidate        // most recent candidate fetch (leaves)
	lastSvcs        []resolve.Service          // most recent service fetch (tree rows)
	svcByName       map[string]resolve.Service // current services, for fold re-marking
	svcCols         svcColumns                 // service-row column widths
	groupByStack    bool                       // nest services under their stack (toggled by the stack_group key)
	regCache        *registryCache             // :latest version / newer-tag resolver
	autoRefreshBusy atomic.Bool                // guards against overlapping tree refreshes

	// Live resource usage, from the node agents (see stats.go). Kept separate
	// from the tree fetch because it comes from a different source and must
	// never hold the tree up: it is an overlay on the rows, and an empty map
	// simply means no badges.
	usage     map[string]containerUsage // by container id
	leafBase  map[string]string         // container id -> tree label without the badge
	nodeUse   map[string]nodeUsage      // by node name
	usageBusy atomic.Bool               // guards against overlapping usage fetches
	statsGate *statsGate                // nodes whose agent cannot serve stats, so we stop asking

	// shared tab state / caches
	active       string       // the selected tab
	activeCtx    string       // active docker-context name
	clusterText  string       // last cluster-probe summary for the footer
	curHelp      string       // current tab's footer help
	savedHelp    string       // footer help saved while search is open
	searchMode   string       // what the "/" bar filters ("containers"/"volumes")
	filter       string       // container-tree "/" query
	volFilter    string       // volumes "/" query
	mouseEnabled bool         // mirrors app.EnableMouse; toggled by 'm'
	screen       tcell.Screen // owned screen, for OSC52 clipboard on yank
	overlayDepth atomic.Int32 // open overlays (pauses the tree auto-refresh)

	// volumes tab caches
	selectedVols    map[string]bool
	shownVols       []swarmVolume
	vols            []swarmVolume
	volUsage        map[string][]volumeConsumer
	volSizes        map[string]int64
	volErrs         map[string]error
	volSizesLoading bool
	sortField       int
	sortDesc        bool

	// forwards tab cache: fRows mirrors the rendered table so a row maps to a forward.
	fRows []forwardEntry

	// networks / secrets / contexts tab caches
	nets []swarmNetwork
	secs []swarmSecret
	cfgs []swarmConfig
	ctxs []dockerctx.Context

	// nodes tab caches
	nodeInfos      []swarmNodeInfo
	nodeVolCounts  map[string]int
	nodeVolsLoaded bool

	// toggleable client-log overlay
	logViewStop    chan struct{}
	logViewPrev    tview.Primitive
	logViewRestore func()
}
