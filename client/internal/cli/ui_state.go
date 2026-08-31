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
// hand-aligned strings, so adding a tab is a single edit.
var uiTabList = []struct{ key, label string }{
	{"containers", "Containers (1)"},
	{"volumes", "Volumes (2)"},
	{"forwards", "Forwards (3)"},
	{"networks", "Networks (4)"},
	{"secrets", "Secrets (5)"},
	{"contexts", "Contexts (6)"},
	{"nodes", "Nodes (7)"},
}

// uiTabOrder drives Tab cycling; every tab joins it.
var uiTabOrder = []string{"containers", "volumes", "forwards", "networks", "secrets", "contexts", "nodes"}

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
	tabBar, help, status                                 *tview.TextView
	footer, root                                         *tview.Flex
	search                                               *tview.InputField

	// per-tab entry points (closures in run(); called from the infra methods)
	renderContainers func()
	loadContainers   func()
	renderForwards   func()
	loadVolumes      func()
	loadNetworks     func()
	loadSecrets      func()
	loadContexts     func()
	loadNodes        func()

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
	selectedVols map[string]bool
	shownVols    []swarmVolume
	volUsage     map[string][]volumeConsumer
	volSizes     map[string]int64

	// networks / secrets / contexts tab caches
	nets []swarmNetwork
	secs []swarmSecret
	ctxs []dockerctx.Context

	// toggleable client-log overlay
	logViewStop    chan struct{}
	logViewPrev    tview.Primitive
	logViewRestore func()
}
