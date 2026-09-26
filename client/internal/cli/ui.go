// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/secscan"
	"swarmexec/client/internal/session"
	cterm "swarmexec/client/internal/term"
)

type uiFlags struct {
	connectTimeout time.Duration
}

func newUICmd(g *globalFlags) *cobra.Command {
	f := &uiFlags{}
	cmd := &cobra.Command{
		Use:   "ui [service]",
		Short: "Interactive view of containers, volumes, networks, secrets and contexts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUI(cmd, g, f, args)
		},
	}
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to an agent")
	return cmd
}

// runUI runs the UI. It starts on the context the flags/env select and stays up
// across cluster switches: activating a context in the Contexts tab swaps the
// visible cluster rather than rebuilding the screen (see switchCluster).
//
// It used to restart instead, and the two reasons given for that are worth
// recording because only one of them was real:
//   - Data races, on the docker client and resolver being reassigned under
//     in-flight background reads. That never needed locking around every
//     access; it needed each cluster to own an immutable bundle and stale
//     results to be dropped. That is clusterState and (*ui).onCluster.
//   - Cluster-bound state — forwards, open streams, cached candidates. Real,
//     and now held per cluster instead of being destroyed. The exception is
//     forwards, which are deliberately NOT torn down: they are local listeners
//     the operator started, and killing them for looking at another cluster is
//     the behaviour this replaces.
//
// How often the UI re-polls the swarm so the container tree reflects background
// changes (rolling updates, restarts, scaling).
//
// Two rates, chosen by whether the manager's event stream is actually feeding
// us (see ui_events.go). With events flowing the poll is only a safety net and
// a third of the manager traffic is enough; without them it is the sole
// mechanism and has to stay quick.
//
// The distinction is not decoration. A dropped event stream is easy to detect —
// but one whose TCP connection dies silently is not, because the Docker events
// endpoint sends nothing while idle. That is why the watcher re-subscribes on a
// timer rather than trusting a connection that has merely not complained: the
// "events are live" answer is never older than topologyResubscribe.
const (
	treeRefreshFast = 10 * time.Second
	treeRefreshSlow = 30 * time.Second
)

// listEntryAction is an optional per-entry action a staged list editor exposes:
// pressing key on the selected entry closes the editor and runs the action with
// that entry (e.g. the networks editor's "aliases" action).
type listEntryAction struct {
	key   rune
	label string
	run   func(entry string)
}

// editListConfig is the (named) configuration for the editList staged list-editor
// overlay. Most callers set only title/applyVerb/items/validate/onApply/back/
// after; the rest default to their zero value (no autocomplete, single-line, no
// confirm note, no per-entry action, the built-in text prompt).
type editListConfig struct {
	title       string
	applyVerb   string
	items       []string
	validate    func(string) (string, error)
	onApply     func([]string) error
	suggest     func(string) []string
	allowEdit   bool
	multiline   bool
	confirmNote func([]string) string
	entryAction *listEntryAction
	formPrompt  func(initial string, submit func(raw string) error, cancel func()) tview.Primitive
	back        tview.Primitive
	after       func()
}

// Page names for the overlay Pages ("pages"). Each overlay only cleans up if its
// RemovePage matches its AddPage exactly, and a typo leaks a stuck overlay with
// no compile-time signal — so the names live here as constants and every
// AddPage/RemovePage/HasPage references them.
const (
	pageMain             = "main"
	pageInfo             = "info"
	pageConfirm          = "confirm"
	pageHelp             = "help"
	pageSecurity         = "security"
	pageSecurityReport   = "securityreport"
	pageStackFile        = "stackfile"
	pageStackDiff        = "stackdiff"
	pageStackPlan        = "stackplan"
	pageNodeAvail        = "nodeavail"
	pageConfigDetail     = "configdetail"
	pageMenu             = "menu"
	pageTerm             = "term"
	pageLogs             = "logs"
	pageLogView          = "logview"
	pageLogGrep          = "loggrep"
	pageInspect          = "inspect"
	pageInspectDiff      = "inspectdiff"
	pageInspectActions   = "inspectactions"
	pageListEdit         = "listedit"
	pageListEditPrompt   = "listeditprompt"
	pageListEditLeave    = "listeditleave"
	pageScalePrompt      = "scaleprompt"
	pageImageVersion     = "imageversion"
	pagePlacementMenu    = "placementmenu"
	pagePlaceDiag        = "placediag"
	pageResEdit          = "resedit"
	pageOrphanSecrets    = "orphansecrets"
	pageSvcPrompt        = "svcprompt"
	pageFwdPrompt        = "fwdprompt"
	pageFwdDetail        = "fwddetail"
	pageUserPrompt       = "userprompt"
	pageVolForm          = "volform"
	pageVolNodes         = "volnodes"
	pageVolUsers         = "volusers"
	pageVolProgress      = "volprogress"
	pageVolAttach        = "volattach"
	pageVolAttachTgt     = "volattachtgt"
	pageVolAttachConfirm = "volattachconfirm"
	pageNetForm          = "netform"
	pageNetMembers       = "netmembers"
	pageSecForm          = "secform"
	pageSecDetail        = "secdetail"
	pageCtxForm          = "ctxform"
	pageCtxDetail        = "ctxdetail"
	pageNodeLabels       = "nodelabels"
	pageNodeLabelPrompt  = "nodelabelprompt"
	pageNodeDetail       = "nodedetail"
	pageNodeImages       = "nodeimages"
)

func runUI(cmd *cobra.Command, g *globalFlags, f *uiFlags, args []string) error {
	if !cterm.IsTerminal(os.Stdout.Fd()) || !cterm.IsTerminal(os.Stdin.Fd()) {
		return &cliError{code: usageExitCode, err: fmt.Errorf("ui needs an interactive terminal (use plain `ps`/`volume ls` when piping)")}
	}

	// Per-run context. It bounds what belongs to the SESSION — the forwards, the
	// responsiveness watchdog, the log viewer — and outlives a cluster switch.
	// Each cluster's own polling hangs off a child of it that is cancelled when
	// the operator looks elsewhere (see clusterState.ctx).
	runCtx, cancelRun := context.WithCancel(cmdContext(cmd))
	defer cancelRun()

	var service string
	if len(args) == 1 {
		service = args[0]
	}

	// Configurable shortcut keys (keys.yaml). Never fails: invalid/conflicting
	// bindings fall back to defaults with a warning shown once on startup.
	km, keyWarnings := loadKeybinds("")

	u := &ui{
		overlays:  map[string]bool{},
		app:       tview.NewApplication(),
		pages:     tview.NewPages(), // overlays: menus, terminal, logs, volume nodes
		content:   tview.NewPages(), // the per-tab pages
		clusters:  map[string]*clusterState{},
		km:        km,
		f:         f,
		g:         g,
		cmd:       cmd,
		runCtx:    runCtx,
		cancelRun: cancelRun,
		service:   service,
		selStyle:  tcell.StyleDefault.Background(tcell.ColorTeal).Foreground(tcell.ColorWhite),
		// Grouping is on by default; it only takes effect once something in the
		// cluster actually carries a stack label (see anyStacked).
		groupByStack: true,
	}

	// The cluster the flags/env point at. This one must come up: there is no UI
	// to show an error in yet, and a tool that opens onto nothing it can reach is
	// worse than a message on stderr. Every LATER cluster is allowed to fail —
	// by then there is a screen to report it on, and another cluster to go back
	// to. See switchCluster.
	first := u.cluster(effectiveContextName(g.dockerContext), g.dockerContext)
	if err := u.connectCluster(first); err != nil {
		return err
	}
	u.clusterState = first
	u.activeCtx = first.name
	u.filter = service // `swarmexec ui web` opens pre-narrowed

	// Install the global theme before any primitive is constructed: tview reads
	// its palette at construction time and its border glyphs at draw time.
	applyTheme(u.cfg.UI.Dim)

	return u.run(keyWarnings)
}

// effectiveContextName is what the session is pointed at, by name: the explicit
// choice, else $DOCKER_CONTEXT, else docker's stored current, else "default".
// It is the label the footer shows and the key a cluster is remembered under —
// never the thing it is resolved from (see clusterState.ctxOverride).
func effectiveContextName(override string) string {
	name := firstNonEmpty(override, os.Getenv("DOCKER_CONTEXT"))
	if name == "" {
		name = dockerctx.Current()
	}
	if name == "" {
		name = "default"
	}
	return name
}

// connectCluster brings a cluster online: ONE endpoint resolution feeding both
// the manager client and the agent tunnel, the config that carries the tunnel's
// dialer, and the context that bounds this cluster's background work.
//
// Called for the first cluster at startup and for each other one the first time
// the operator switches to it. Everything that differs between clusters is
// decided here and nowhere else, which is what keeps a switch from leaving one
// channel pointed at the cluster we just left (the B4 bug, one level up).
func (u *ui) connectCluster(c *clusterState) error {
	ep := resolveEndpoint(c.ctxOverride)
	cfg, err := u.g.resolveConfig(u.cmd, ep)
	if err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return &cliError{code: usageExitCode, err: err}
	}
	dcli, err := ep.connect(u.runCtx)
	if err != nil {
		return &cliError{code: session.TransportFailure, err: err}
	}
	c.cfg = cfg
	c.dcli = dcli
	c.r = resolve.New(dcli, addrModeOf(cfg))
	c.ctx, c.cancel = context.WithCancel(u.runCtx)
	// The registry cache re-marks service rows when a background version check
	// lands. It belongs to the cluster whose images it resolved, and the guard
	// is pointer identity: if that cluster is no longer the visible one, the
	// rows on screen are not its rows.
	c.regCache = newRegistryCache(func() {
		u.app.QueueUpdateDraw(func() {
			if u.clusterState != c {
				return
			}
			eachServiceNode(u.croot, u.markService)
		})
	})
	c.err = nil
	return nil
}

// run drives one UI session over the infrastructure runUI prepared on u. The
// per-tab closures still live here (they will move to their own files in later
// stages); the shared infra closures are now methods on *ui. The aliases below
// keep those closure bodies referring to app/pages/… unchanged.
func (u *ui) run(keyWarnings []string) error {
	app, pages, content := u.app, u.pages, u.content
	km := u.km
	g := u.g
	service := u.service
	selStyle := u.selStyle
	// The session context, NOT the cluster's: the loops below outlive a cluster
	// switch. The tree ticker refreshes whichever cluster is visible at the time,
	// and the watchdog is measuring this process, not a cluster.
	ctx := u.runCtx

	// forwards is the UI's only persistent background resource: a port forward
	// outlives the overlay that started it, unlike every stream here.
	forwards := newForwardRegistry()
	u.forwards = forwards
	defer forwards.stopAll()
	// Every cluster this session touched, not only the visible one.
	defer u.closeClusters()

	// ---------------------------------------------------------------- containers
	// A tree: services are parent nodes, their containers are children.
	ctree := tview.NewTreeView()
	croot := tview.NewTreeNode("")
	ctree.SetRoot(croot).SetTopLevel(1) // hide the synthetic root; services are top-level
	// filter holds the active "/" search query; empty means show everything. A
	// candidate matches when the query is a substring of its service, container
	// id or node (case-insensitive). It starts from the optional `ui [service]`
	// argument so `swarmexec ui web` opens pre-narrowed to matching services.
	u.filter = service
	// lastCands / lastSvcs cache the most recent fetch so the "/" filter can
	// re-render locally without hitting the docker API on every keystroke (a
	// remote call over the ssh tunnel — doing it per keystroke makes typing
	// crawl). lastSvcs drives the tree so every service shows, even one with no
	// running task; lastCands supplies the container leaves.
	// svcByName / svcCols cache the current services and the column widths so the
	// fold marker (▸/▾) can be rebuilt on a fold — and the docker service ls-style
	// row (mode, replicas, image, ports) stays aligned — without re-rendering the
	// whole tree. A service with no containers gets no marker (nothing to expand),
	// just padding so the rows still line up.
	// regCache resolves the real version behind a service's :latest tag and whether
	// the registry has a newer one; assigned just below. Its result is appended to
	// the service row (e.g. "(1.2.3) ↑").
	// When a background registry check completes, re-mark the service rows so the
	// version / ↑ appears without a full tree reload.
	u.regCache = newRegistryCache(func() {
		app.QueueUpdateDraw(func() {
			eachServiceNode(croot, u.markService)
		})
	})

	// fetchContainers does the two manager round-trips (off the UI goroutine) and
	// returns sorted results. It is instrumented so a slow manager shows up in the
	// log viewer.
	// applyContainers updates the tree from a fetch result. UI-goroutine only.
	// loadContainersSync fetches and applies on the caller's goroutine — used at
	// startup, before app.Run, where QueueUpdateDraw would deadlock.
	// loadContainers refreshes without freezing the event loop: it fetches off the
	// UI goroutine (two manager round-trips that used to run inline and stall the
	// whole TUI) and applies the result via QueueUpdateDraw.
	// autoRefreshContainers re-lists services/containers off the UI goroutine on a
	// timer so a container replaced by a rolling update (or a scaled service) shows
	// up without pressing r. renderContainers restores the cursor and expanded
	// services, so the refresh is unobtrusive. A transient probe error is ignored
	// rather than clobbering the tree with an error node.
	//
	// It skips while an overlay is open — the tree is hidden, and its periodic
	// renderContainers on the UI goroutine would compete with keystrokes in the
	// overlay (laggy input) — and never overlaps itself (a slow probe over ssh can
	// outlast the tick).

	// startForward brings a forward up off the UI goroutine: dialling the agent
	// can take up to the connect timeout, and blocking the UI for that would
	// freeze the whole app. The entry is registered immediately in the starting
	// state so the operator sees that something is happening.

	// portPrompt asks which port to forward. There is deliberately no list of
	// exposed ports to pick from: the manager API cannot inspect a container on
	// another node, and the services worth forwarding are exactly the ones that
	// publish nothing — so a suggestion list would be empty where it matters.

	// userPrompt asks which user/UID to exec as, then calls open with it. Mirrors
	// the CLI's `exec -u`: a name, a UID, or UID:GID.

	ctree.SetSelectedFunc(func(node *tview.TreeNode) {
		if ref, ok := node.GetReference().(resolve.Candidate); ok {
			u.containerMenu(ref)
			return
		}
		// Service node → toggle expand/collapse. Logs are on the L key.
		if isServiceNode(node) {
			node.SetExpanded(!node.IsExpanded())
			u.markService(node)
			return
		}
		// Stack node → toggle the whole group.
		if isStackNode(node) {
			node.SetExpanded(!node.IsExpanded())
			u.markStack(node)
		}
	})

	// ------------------------------------------------------------------- volumes
	vtable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	vtable.SetSelectedStyle(selStyle)
	// selectedVols holds the volumes marked with space for a bulk delete, keyed
	// by name so the selection survives sorting and re-render.
	u.selectedVols = map[string]bool{}
	// shownVols is the filtered + sorted subset currently displayed; it is what
	// selectedVolume() and "select all" index. volFilter is the "/" search query.
	// sortVolumes orders rows by the active field; for size/age an unknown value
	// always sorts last (regardless of direction), with name as the tiebreaker.

	vtable.SetSelectedFunc(func(int, int) {
		if v, ok := u.selectedVolume(); ok && len(v.Nodes) > 0 {
			u.showVolumeNodes(v)
		}
	})
	// showCreateVolume opens a form to create a volume (default driver local) with
	// labels, targeting one node or (blank) all nodes — volumes are node-local, so
	// creation goes to each target node's agent.

	// showVolumeConsumers lists the services/containers that mount a volume.

	// deleteVolumes removes each target volume on every node that holds it, behind
	// a single confirm. Volumes are node-local, so a volume is removed across all
	// its v.Nodes. Shared by the multi-select delete and prune.

	// pruneVolumes deletes every volume that no running container mounts and no
	// service declares (service-declared volumes are spared even with no running
	// task). The service check needs a ServiceList, so it runs off the UI goroutine.

	// ------------------------------------------------------------------ forwards
	ftable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	ftable.SetSelectedStyle(selStyle)
	// selectedForward maps the cursor row back to a forward.

	// ------------------------------------------------------------------ networks
	nettable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	nettable.SetSelectedStyle(selStyle)
	// serviceNamesFromCache returns the known service names (the containers-tab
	// cache), used to seed the attach autocomplete. It is only a suggestion list —
	// the actual attach resolves the name live, so a just-created service that is
	// not cached yet can still be typed in.
	// servicePrompt opens an autocomplete input to choose a service, then a
	// confirmation (the change triggers a rolling update of that service), then
	// runs do(service) off the UI goroutine and calls onDone on success. It backs
	// both attach and detach: actionLabel is the confirm button ("Attach" /
	// "Detach"), confirmVerb the sentence lead-in, suggestions feed the
	// autocomplete only, and back gets focus when the operator cancels.

	// showNetworkMembers lists the services attached to a network with the
	// containers of each service nested under it. Service membership is known
	// synchronously (from the list); the per-service containers need a task
	// lookup, so they fill in lazily after the overlay is up.
	nettable.SetSelectedFunc(func(int, int) {
		if n, ok := u.selectedNetwork(); ok {
			u.showNetworkMembers(n)
		}
	})
	// showCreateNetwork opens a form to create a network (default driver overlay)
	// with the common swarm options — attachable, encrypted, internal, IPv6, MTU
	// and an optional subnet/gateway and labels.

	// ------------------------------------------------------------------- secrets
	sectable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	sectable.SetSelectedStyle(selStyle)
	// showSecretDetail shows a secret's metadata and the services/containers that
	// use it. The value is deliberately absent: the Docker API never returns it,
	// and the overlay says so. Usage (services with their running containers)
	// needs a task lookup, so it fills in lazily after the overlay is up.
	sectable.SetSelectedFunc(func(int, int) {
		if s, ok := u.selectedSecret(); ok {
			u.showSecretDetail(s)
		}
	})
	// openDeleteSecret permanently removes a secret after a confirm. Docker refuses
	// to remove a secret a service still references, so warn up front when in use.
	// showCreateSecret creates a new swarm secret from a name, a (multi-line)
	// value and optional labels. The value is entered in a text area so certs and
	// keys can be pasted as-is.

	// ------------------------------------------------------------------ contexts
	// activeCtx (the name in the footer, and the row marked here) is set with the
	// cluster it belongs to — runUI for the first one, activateCluster for every
	// switch — so the name and the connection can never disagree.
	// No fixed header row: in the sidebar the first row is already a context
	// (see renderContexts), and SetFixed(1,0) would pin it out of reach of the
	// cursor.
	cxtable := tview.NewTable().SetBorders(false).SetSelectable(true, false)
	cxtable.SetSelectedStyle(selStyle)
	// Contexts come from docker's local store (no network), so load synchronously.
	// showCreateContext opens a guided form to add a docker context. The operator
	// explicitly decides whether to connect over SSH and, if so, whether to go
	// through a jump host — the relevant fields appear only when opted in. For SSH
	// the jump host(s) are stored on the context and injected as -J into both the
	// Docker-API and agent-tunnel ssh connections (no ~/.ssh/config needed).
	// "Test" verifies the assembled endpoint (a live daemon ping) before saving.
	// deleteContext removes the selected context behind a confirm. "default" is
	// protected; removing the current one resets the selection to default.
	// activateContext makes c the current docker context and restarts the UI so
	// it reconnects to that cluster. Restarting (rather than swapping the client
	// live) avoids racing the in-flight background loads.

	// -------------------------------------------------------------------- nodes
	notable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	notable.SetSelectedStyle(selStyle)
	// editNodeLabels is a compact staged editor (add/edit/delete/apply) over a
	// node's labels, applied via NodeUpdate. Dedicated (not the service editList)
	// because a node update is immediate — there is no rolling update.
	notable.SetSelectedFunc(func(int, int) {
		if n, ok := u.selectedNode(); ok {
			u.showNodeDetail(n)
		}
	})

	// ------------------------------------------------------------------- configs
	// The Secrets tab's twin, for the other object swarm distributes to
	// containers — with the difference that a config's content is readable, so
	// the detail overlay can show what a service actually receives.
	cfgtable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	cfgtable.SetSelectedStyle(selStyle)
	cfgtable.SetSelectedFunc(func(int, int) {
		if c, ok := u.selectedConfig(); ok {
			u.showConfigDetail(c)
		}
	})

	// ---------------------------------------------------------------- tabs/chrome
	content.AddPage("containers", ctree, true, true)
	content.AddPage("volumes", vtable, true, false)
	content.AddPage("forwards", ftable, true, false)
	content.AddPage("networks", nettable, true, false)
	content.AddPage("secrets", sectable, true, false)
	content.AddPage("nodes", notable, true, false)
	content.AddPage("configs", cfgtable, true, false)

	tabBar := newTabStrip(uiTabList, func(key string) { u.setTab(key) })
	// Two-line footer: the per-tab key hints on top, then one consolidated status
	// line — active context · live cluster summary · forward count · (on the
	// volumes tab) selection count. u.updateStatus() composes the status line; the
	// async cluster/forward refreshers feed it. clusterText holds the last cluster
	// probe result so a forward or selection change can recompose without re-probing.
	help := tview.NewTextView().SetDynamicColors(true)
	status := tview.NewTextView().SetDynamicColors(true)
	u.clusterText = "[gray]cluster: …[white]"
	footer := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(help, 1, 0, false).
		AddItem(status, 1, 0, false)
	// "/" search bar: hidden (height 0) until activated; filters the container
	// tree live by service / container id / node.
	search := tview.NewInputField().SetLabel("/ ").SetFieldWidth(0).
		SetPlaceholder("filter services / containers / nodes")
	// The context list is a column beside the content, not a tab (see
	// ui_sidebar.go). It is framed so it reads as chrome rather than as a second
	// table competing with the one in the middle.
	sidebar := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(cxtable, 0, 1, true)
	sidebar.SetBorder(true).SetTitle(" contexts ").SetBorderColor(palette.border)
	body := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(content, 0, 1, true).
		// One blank column so the content's rightmost characters do not run
		// into the sidebar's border; tview clips rather than wraps, and without
		// it a long image or port list ends flush against the frame.
		AddItem(nil, 0, 0, false).
		AddItem(sidebar, 0, 0, false)
	u.body, u.sidebar = body, sidebar
	// The width is decided at draw time from the space we actually have: tview
	// runs this before the Flex lays its items out, so the new width applies to
	// the very frame that measured it — no resize events to listen for, and no
	// stale width after a terminal resize.
	body.SetDrawFunc(func(_ tcell.Screen, x, y, w, h int) (int, int, int, int) {
		u.resizeSidebar(w)
		return x, y, w, h
	})

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tabBar, 2, 0, false). // labels row + active-tab underline indicator
		AddItem(body, 0, 1, true).
		AddItem(search, 0, 0, false).
		AddItem(footer, 2, 0, false)
	pages.AddPage(pageMain, root, true, true)

	// Promote the shared widgets onto u so the infra methods (updateStatus,
	// setTab, refreshForwardViews, …) can reach them. All exist by now.
	u.ctree, u.croot = ctree, croot
	u.vtable, u.ftable, u.nettable = vtable, ftable, nettable
	u.sectable, u.cxtable, u.notable = sectable, cxtable, notable
	u.cfgtable = cfgtable
	u.tabBar, u.help, u.status = tabBar, help, status
	u.footer, u.root, u.search = footer, root, search

	// u.savedHelp holds the footer help to restore when search closes; u.searchMode
	// selects what the shared "/" bar filters (container tree vs. volumes table).
	search.SetChangedFunc(func(text string) {
		// Filter locally against the cached data — no docker call per keystroke.
		t := strings.TrimSpace(text)
		if u.searchMode == "volumes" {
			u.volFilter = t
			u.renderVolumeTable()
		} else {
			u.filter = t
			u.renderContainers()
		}
	})
	search.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			search.SetText("") // clears the active filter via SetChangedFunc
		}
		isVol := u.searchMode == "volumes"
		empty := u.filter == ""
		if isVol {
			empty = u.volFilter == ""
		}
		if empty {
			root.ResizeItem(search, 0, 0) // nothing active — collapse the bar away
		}
		u.setFooter(u.savedHelp) // restore the tab help
		if isVol {
			app.SetFocus(vtable) // Enter keeps the filter; the bar stays as an indicator
		} else {
			app.SetFocus(ctree)
		}
	})

	u.active = "containers"
	u.mouseEnabled = true
	var screen tcell.Screen // set just before Run; used for clipboard (OSC52)

	// On the tree, "/" opens search; h/l collapse/expand the service under the
	// cursor; j/k stay down/up via the shared keys.
	// showInspect renders an inspect in a scrollable overlay with two views: a
	// tabular, operator-first summary (default) and the raw daemon JSON, toggled
	// with `t`. fetch runs off the UI goroutine so a slow manager cannot freeze
	// the loop. It returns (formatted, rawJSON).
	// editList is a staged list editor: it shows the current entries and lets the
	// operator add/edit/delete locally, then apply them all at once (one
	// ServiceUpdate). validate normalizes/validates a single entry; onApply gets
	// the final list; after runs on success. Used for a service's ports and labels.
	// formPrompt (optional) replaces the default single-line add/edit prompt with
	// a custom form: it receives the current entry (empty when adding), a submit
	// callback that validates+stages+closes on success (returning an error to show
	// otherwise), and a cancel callback; it returns the primitive to display.
	// openPortsEditor / openLabelsEditor fetch the service's current ports/labels
	// off the UI goroutine, then open the staged editor.
	// openAliasEditorForNet edits a service's DNS aliases on one network. Reached
	// from the networks editor (select a network, press A), so aliases only show
	// in that context, not as a top-level inspect key.
	// openNetworksEditor edits the networks a service is attached to, with
	// autocomplete of network names (add/remove), applied in one ServiceUpdate.
	// Selecting a network and pressing A edits that network's DNS aliases.
	// openSecretsEditor edits the secrets a service references, with autocomplete
	// of secret names (add/remove), applied in one ServiceUpdate. Works even when
	// the service has none yet.
	// openMountsEditor edits a service's mounts (volumes + binds, with a
	// read-only flag). For bind mounts it can't verify the host path (no host
	// access), so on apply it warns which nodes the service could run on and that
	// each bind source must already exist on all of them.
	// openEnvEditor edits a service's environment variables (KEY=VALUE), with
	// edit allowed (adjust a value in place), add and remove.
	// placementListEditor opens a staged list editor whose add/edit input
	// autocompletes cluster-derived candidates (excluding ones already staged).
	// Shared by the constraints and the spread-preferences editors.
	// openPlacementConstraintsEditor edits the service's hard placement
	// constraints (node.* / engine.* == / !=) with node-derived autocomplete.
	// openSpreadEditor edits the service's spread placement preferences — bare
	// node attributes (e.g. node.labels.zone) that Swarm spreads tasks over — in
	// priority order, with node-derived autocomplete.
	// openPlacementMenu groups the two placement concerns (hard constraints and
	// soft spread preferences) under one key so the inspect footer stays short.
	// openScalePrompt asks for a new replica count and scales the service.
	// openForceUpdate redeploys a service (docker service update --force) after a
	// confirm — every task is restarted/rescheduled, which unsticks a service in
	// an incomplete state (e.g. 1/2). No spec change beyond bumping ForceUpdate.
	// promptDeleteOrphanSecrets asks whether to also delete secrets that the
	// just-removed service was the only user of (nothing references them now).
	// openRemoveService permanently deletes a service after a confirm. onRemoved is
	// called on success (the caller closes the inspect overlay and refreshes the
	// tree, since the service no longer exists). If the service was the sole user
	// of any secret, it then offers to delete those now-orphaned secrets.
	// openImageVersionPicker updates a service's image after a confirm: for a
	// version-pinned service it prompts for a target version (newer ones suggested,
	// any existing tag typeable, downgrades warned); for a :latest service it
	// confirms the current-digest target. Rolling update.
	// showPlacementDiagnosis explains why a service is not running everywhere it is
	// expected to — per-node exclusion reasons for a global service, and the
	// scheduler's own message on each non-running task for a replicated one.
	// openResourcesEditor sets/edits/clears the service's CPU and memory limits
	// (and reservations) in a form. An empty field clears that limit; applying
	// does one ServiceUpdate (rolling update).

	// inspectCurrent shows service inspect on a service node, task inspect on a
	// container leaf (both from the manager).
	ctree.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
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
				u.removeServiceUnderCursor()
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
						u.markStack(n)
					case isServiceNode(n) && n.IsExpanded():
						n.SetExpanded(false)
						u.markService(n)
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
							u.markStack(n)
						} else {
							u.markService(n)
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
				u.groupByStack = !u.groupByStack
				u.renderContainers()
				if u.groupByStack {
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
	})
	// Enter shows the full detail of a forward. The table truncates the state
	// column, so this is where a failure reason is actually readable.
	ftable.SetSelectedFunc(func(int, int) { u.showForwardDetail() })
	// On the forwards table: Enter/i details, d stops the forward, o copies its URL.
	ftable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case 'i':
				u.showForwardDetail()
				return nil
			case km.FwdStop:
				if e, ok := u.selectedForward(); ok {
					forwards.remove(e.id)
					u.refreshForwardViews()
					u.flash(fmt.Sprintf(" [green]stopped[white] forward to %s:%d", shortID(e.cand.ContainerID), e.remote))
				}
				return nil
			case km.FwdCopyURL:
				// Copy rather than launch a browser: the UI often runs over
				// ssh, where opening a local browser would target the wrong
				// machine — and the forward is bound on the operator's side.
				if e, ok := u.selectedForward(); ok && e.state == forwardActive {
					url := fmt.Sprintf("http://127.0.0.1:%d", e.boundPort())
					if screen != nil {
						screen.SetClipboard([]byte(url))
					}
					u.flash(" [green]copied[white] " + url)
				}
				return nil
			}
		}
		return u.tabKeys(ev)
	})
	// On the volumes table, "i" shows which services/containers use the volume.
	// attachVolumeToService mounts a volume into a service from the Volumes tab:
	// pick a service (autocomplete), enter the container target path, choose
	// read-only or not, then a ServiceUpdate adds the mount.
	vtable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case km.Search:
				u.startSearch("volumes")
				return nil
			case km.VolNew:
				u.showCreateVolume()
				return nil
			case km.VolAttach:
				if v, ok := u.selectedVolume(); ok {
					u.attachVolumeToService(v.Name)
				}
				return nil
			case km.VolSelect:
				// Toggle the current volume's selection for a bulk delete.
				if v, ok := u.selectedVolume(); ok {
					if u.selectedVols[v.Name] {
						delete(u.selectedVols, v.Name)
					} else {
						u.selectedVols[v.Name] = true
					}
					u.renderVolumeTable()
					u.updateStatus()
				}
				return nil
			case km.VolSelectAll:
				// Select or deselect all currently displayed volumes.
				all := len(u.shownVols) > 0
				for _, v := range u.shownVols {
					if !u.selectedVols[v.Name] {
						all = false
						break
					}
				}
				for _, v := range u.shownVols {
					if all {
						delete(u.selectedVols, v.Name)
					} else {
						u.selectedVols[v.Name] = true
					}
				}
				u.renderVolumeTable()
				u.updateStatus()
				return nil
			case km.VolDelete:
				// Delete the selected volumes, or the one under the cursor.
				var targets []swarmVolume
				if len(u.selectedVols) > 0 {
					for _, v := range u.vols {
						if u.selectedVols[v.Name] {
							targets = append(targets, v)
						}
					}
				} else if v, ok := u.selectedVolume(); ok {
					targets = []swarmVolume{v}
				}
				u.deleteVolumes(targets, fmt.Sprintf("Remove %d volume(s) on every node that holds them?", len(targets)))
				return nil
			case km.VolPrune:
				u.pruneVolumes()
				return nil
			case km.VolUsedBy:
				if v, ok := u.selectedVolume(); ok {
					u.showVolumeConsumers(v)
				}
				return nil
			case km.VolSort:
				// Cycle the sort field; pick a sensible default direction for it.
				u.sortField = (u.sortField + 1) % 5
				u.sortDesc = u.sortField != volSortName && u.sortField != volSortAge
				u.renderVolumeTable()
				return nil
			case km.VolSortRev:
				u.sortDesc = !u.sortDesc
				u.renderVolumeTable()
				return nil
			}
		}
		return u.tabKeys(ev)
	})
	// On the networks table, "i" (like the volumes tab) shows the attached
	// services/containers; Enter does the same.
	nettable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune && ev.Rune() == km.NetAttached {
			if n, ok := u.selectedNetwork(); ok {
				u.showNetworkMembers(n)
			}
			return nil
		}
		if ev.Key() == tcell.KeyRune && ev.Rune() == km.NetNew {
			u.showCreateNetwork()
			return nil
		}
		return u.tabKeys(ev)
	})
	sectable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune && ev.Rune() == km.SecNew {
			u.showCreateSecret()
			return nil
		}
		if ev.Key() == tcell.KeyRune && ev.Rune() == km.SecDelete {
			if s, ok := u.selectedSecret(); ok {
				u.openDeleteSecret(s)
			}
			return nil
		}
		// "i" opens the same detail as Enter — inspect means the same on every tab.
		if ev.Key() == tcell.KeyRune && ev.Rune() == 'i' {
			if s, ok := u.selectedSecret(); ok {
				u.showSecretDetail(s)
			}
			return nil
		}
		return u.tabKeys(ev)
	})
	// The context sidebar's keys: Enter/u switch cluster, i details, n creates,
	// d removes, Esc hands the keyboard back to the tab you were on.
	cxtable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape {
			u.blurSidebar()
			return nil
		}
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case 'i':
				if c, ok := u.selectedContext(); ok {
					u.showContextDetail(c)
				}
				return nil
			case km.CtxNew:
				u.showCreateContext()
				return nil
			case km.CtxDelete:
				if c, ok := u.selectedContext(); ok {
					u.deleteContext(c)
				}
				return nil
			case km.CtxUse:
				if c, ok := u.selectedContext(); ok {
					u.activateContext(c)
				}
				return nil
			}
		}
		return u.tabKeys(ev)
	})
	cxtable.SetSelectedFunc(func(int, int) {
		if c, ok := u.selectedContext(); ok {
			u.activateContext(c)
		}
	})
	cfgtable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune && ev.Rune() == 'i' {
			if c, ok := u.selectedConfig(); ok {
				u.showConfigDetail(c)
			}
			return nil
		}
		return u.tabKeys(ev)
	})

	notable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune && ev.Rune() == km.NodeAvail {
			if n, ok := u.selectedNode(); ok {
				u.openNodeAvailability(n, notable, u.loadNodes)
			}
			return nil
		}
		if ev.Key() == tcell.KeyRune && ev.Rune() == km.NodeImages {
			if n, ok := u.selectedNode(); ok {
				u.openNodeImagePrune(n, notable, u.loadNodes)
			}
			return nil
		}
		if ev.Key() == tcell.KeyRune && ev.Rune() == km.NodeLabels {
			if n, ok := u.selectedNode(); ok {
				u.editNodeLabels(n, u.loadNodes)
			}
			return nil
		}
		if ev.Key() == tcell.KeyRune && ev.Rune() == 'i' {
			if n, ok := u.selectedNode(); ok {
				u.showNodeDetail(n)
			}
			return nil
		}
		return u.tabKeys(ev)
	})

	// The sidebar is visible from the first frame, so its content is loaded
	// before it: it comes from docker's local store, no network involved.
	u.loadContexts()
	u.loadContainersSync() // startup: before app.Run, so fetch+apply inline
	// Arm the agents' samplers right away: the first reading carries memory but
	// no CPU (a percentage needs two), so asking at startup means the numbers are
	// complete by the time the operator has looked at anything.
	u.loadUsage()
	u.setTab("containers")
	u.refreshCluster()

	// React to the manager's own account of what changed, so the tree follows a
	// rolling update or a scaled service in well under a second instead of
	// somewhere within the poll interval. Started here for the cluster the UI
	// opened on; every later cluster gets its own in activateCluster.
	go u.watchTopology(u.ctx, u.clusterState)

	// And poll anyway. The client learns topology from the manager; the agents
	// are node-local and cannot push such events. This is the net under the
	// event stream, not a leftover — see ui_events.go for why removing it would
	// turn a dropped connection into a tree that silently stops updating.
	go func() {
		t := time.NewTicker(treeRefreshFast)
		defer t.Stop()
		rate := treeRefreshFast
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// Re-rated on each tick rather than on a change, so a stream
				// that dies takes at most one slow interval to be noticed.
				if want := u.pollInterval(); want != rate {
					rate = want
					t.Reset(rate)
				}
				u.autoRefreshContainers()
				// Usage rides the same tick but on its own goroutine, so a slow or
				// unreachable agent delays only the badges, never the tree.
				u.loadUsage()
			}
		}
	}()

	// Surface any keys.yaml problems once, non-fatally, over the started UI.
	if len(keyWarnings) > 0 {
		u.info("keys.yaml:\n\n" + strings.Join(keyWarnings, "\n"))
	}

	// Responsiveness watchdog: time how long the event loop takes to service a
	// no-op; a stall means something is blocking the loop (the "UI reagiert nicht"
	// symptom). Logged so the viewer/file shows it. QueueUpdate runs in its own
	// goroutine so a stuck loop cannot block the measurement.
	clientlog.L().Info("ui started", "log_level", g.logLevel)
	go func() {
		tk := time.NewTicker(2 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				sent := time.Now()
				done := make(chan struct{})
				go func() { app.QueueUpdate(func() {}); close(done) }()
				select {
				case <-done:
					if d := time.Since(sent); d > uiStallWarn {
						clientlog.L().Warn("ui event loop was busy", "blocked_ms", d.Milliseconds())
					}
				case <-time.After(uiStallWarn):
					clientlog.L().Warn("ui event loop stalled", "over_ms", uiStallWarn.Milliseconds())
					<-done
					clientlog.L().Warn("ui event loop recovered", "after_ms", time.Since(sent).Milliseconds())
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	// Own the screen so we can post to the system clipboard (OSC52) on yank.
	scr, serr := tcell.NewScreen()
	if serr != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: init screen: %w", serr)}
	}
	screen = scr
	u.screen = screen
	app.SetScreen(screen)

	// Intercept tview's one hard-coded global key (Ctrl-C → Stop) so that inside
	// a container shell Ctrl-C reaches the remote process instead of quitting the
	// TUI; elsewhere it is swallowed (quit is on the `q` key). See ctrlCCapture.
	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		return ctrlCCapture(ev, app.GetFocus())
	})

	if err := app.SetRoot(pages, true).EnableMouse(true).Run(); err != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: %w", err)}
	}
	return nil
}

// svcRef marks a service (group) node and carries its name. It is deliberately
// NOT a resolve.Candidate, so isServiceNode (and every other
// GetReference().(resolve.Candidate) check) still treats it as a group node; it
// only lets a re-render restore the cursor and expansion state by service name.
type svcRef struct{ name string }

// stackRef marks a stack group node. The tree nests container leaves under
// service nodes under stack nodes, so node kind is decided by the reference
// type, never by depth.
type stackRef struct{ name string }

// isServiceNode reports whether a tree node is a service (group) node — matched
// on its reference, so a stack node is not mistaken for one.
func isServiceNode(n *tview.TreeNode) bool {
	_, ok := n.GetReference().(svcRef)
	return ok
}

// isStackNode reports whether a tree node is a stack group node.
func isStackNode(n *tview.TreeNode) bool {
	_, ok := n.GetReference().(stackRef)
	return ok
}

// eachServiceNode calls fn for every service node in the tree, at whatever
// depth it sits (directly under the root when ungrouped, under a stack node
// when grouped).
func eachServiceNode(root *tview.TreeNode, fn func(*tview.TreeNode)) {
	root.Walk(func(node, _ *tview.TreeNode) bool {
		if isServiceNode(node) {
			fn(node)
			return false // services hold container leaves, not more services
		}
		return true
	})
}

// parentOf returns a node's parent in the tree, or nil for the root.
func parentOf(root, target *tview.TreeNode) *tview.TreeNode {
	var found *tview.TreeNode
	root.Walk(func(node, parent *tview.TreeNode) bool {
		if node == target {
			found = parent
			return false
		}
		return true
	})
	return found
}

// trimFoldMarker strips the leading ▸/▾ fold marker (or its blank padding) from
// a service node's text, recovering the plain "name  running/desired" label.
func trimFoldMarker(s string) string {
	return strings.TrimLeft(s, "▸▾ ")
}

// networkType summarizes a network's role for the TYPE column. The flags are
// mutually meaningful but rarely combine, so the most operationally salient one
// wins: ingress (routing mesh) over internal (no egress) over attachable.
// uiStallWarn is how long the event loop may take to service a no-op before the
// watchdog logs it as a stall.
const uiStallWarn = 250 * time.Millisecond

// colorLogLine tints a slog text line for the log viewer by its level= field.
func colorLogLine(line string) string {
	esc := tview.Escape(line)
	switch {
	case strings.Contains(line, "level=ERROR"):
		return "[red]" + esc + "[-]"
	case strings.Contains(line, "level=WARN"):
		return "[yellow]" + esc + "[-]"
	case strings.Contains(line, "level=DEBUG"):
		return "[gray]" + esc + "[-]"
	default:
		return esc
	}
}

func networkType(n swarmNetwork) string {
	switch {
	case n.Attachable:
		return "attachable"
	case n.Internal:
		return "internal"
	case n.Ingress:
		return "ingress"
	case n.Driver == "overlay":
		return "overlay"
	default:
		return orDash(n.Driver)
	}
}

// networkTypeColor marks a network by type. Priority (highest first) matches the
// networkType switch: attachable→green, internal→yellow, ingress→gray,
// overlay(swarm)→aqua, everything local (bridge/host/…)→dimmed.
func networkTypeColor(n swarmNetwork) tcell.Color {
	switch {
	case n.Attachable:
		return tcell.ColorGreen
	case n.Internal:
		return tcell.ColorYellow
	case n.Ingress:
		// The routing mesh gets the same dim as the other networks nobody
		// created. It used to have a gray of its own, one shade off the gray
		// everything dim already uses — asking the eye to tell two grays apart
		// for something the TYPE column says in words.
		return tcell.ColorDimGray
	case n.Driver == "overlay" || n.Scope == "swarm":
		return tcell.ColorAqua
	default:
		return tcell.ColorDimGray
	}
}

// renderPlaceReport formats a placement diagnosis as colour-tagged text for the
// overlay: a ✓/✗ per node (global) or per non-running task (replicated) with the
// reason, and a short explanation of what "desired" means.
func renderPlaceReport(rep placeReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  [aqua]%s[-]  —  %s  —  %d/%d running\n\n", tview.Escape(rep.Service), rep.Mode, rep.Running, rep.Desired)
	if len(rep.Rows) == 0 {
		b.WriteString("  [green]✓ all desired tasks are running[-]\n")
	}
	w := 0
	for _, r := range rep.Rows {
		if len(r.Node) > w {
			w = len(r.Node)
		}
	}
	for _, r := range rep.Rows {
		mark, color := "[green]✓[-]", "[white]"
		if !r.OK {
			mark, color = "[red]✗[-]", "[yellow]"
		}
		fmt.Fprintf(&b, "  %s %-*s  %s%s[-]\n", mark, w, tview.Escape(r.Node), color, tview.Escape(r.Detail))
	}
	if len(rep.Unevaluated) > 0 {
		fmt.Fprintf(&b, "\n  [gray]note: constraints not checkable client-side (assumed OK): %s[-]\n", tview.Escape(strings.Join(rep.Unevaluated, ", ")))
	}
	renderTaskPS(&b, rep.Tasks)
	b.WriteString("\n")
	if rep.Global {
		b.WriteString("  [gray]global: \"desired\" = eligible nodes. A node is excluded by drain/pause,\n  a down state, an unmet placement constraint, or a platform mismatch.[-]\n")
	} else {
		b.WriteString("  [gray]replicated: rows are the tasks that are not running, with the\n  scheduler's own reason (constraints, resources, image pull, …).[-]\n")
	}
	return b.String()
}

// renderTaskPS appends the `docker service ps` equivalent: the recent tasks with
// their node, desired/current state and last-change age, and — indented on its
// own line so nothing is truncated — the scheduler/runtime error of any task
// that failed. This is the detailed evidence behind the summary rows above.
func renderTaskPS(b *strings.Builder, tasks []taskPS) {
	if len(tasks) == 0 {
		return
	}
	const maxRows = 15
	nameW, nodeW := 4, 4
	for i, t := range tasks {
		if i >= maxRows {
			break
		}
		if l := len(t.Name); l > nameW {
			nameW = l
		}
		if l := len(t.Node); l > nodeW {
			nodeW = l
		}
	}
	if nameW > 30 {
		nameW = 30
	}
	if nodeW > 20 {
		nodeW = 20
	}
	fmt.Fprintf(b, "\n  [aqua]recent tasks[-] [gray](docker service ps)[-]\n")
	for i, t := range tasks {
		if i >= maxRows {
			fmt.Fprintf(b, "  [gray]… and %d older task(s)[-]\n", len(tasks)-maxRows)
			break
		}
		mark, col := taskStateStyle(t.Current)
		when := volumeAge(t.When)
		fmt.Fprintf(b, "  %s %-*s  [gray]%-*s[-]  %s%s[-]/%s%s[-]  [gray]%s[-]\n",
			mark, nameW, tview.Escape(clip(t.Name, nameW)), nodeW, tview.Escape(clip(t.Node, nodeW)),
			"[gray]", t.Desired, col, t.Current, when)
		if t.Err != "" {
			fmt.Fprintf(b, "      [red]↳ %s[-]\n", tview.Escape(t.Err))
		}
	}
}

// taskStateStyle returns a status glyph and colour tag for a task's current
// state: running/complete are good, failed/rejected/orphaned are errors, the
// rest are in-flight.
func taskStateStyle(state string) (mark, color string) {
	switch state {
	case "running", "complete":
		return "[green]✓[-]", "[green]"
	case "failed", "rejected", "orphaned":
		return "[red]✗[-]", "[red]"
	default:
		return "[yellow]•[-]", "[yellow]"
	}
}

// clip truncates s to n runes with an ellipsis so wide names/nodes don't break
// the task table's column alignment.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// nodeAvailColor colours a node's availability: active→green, drain→yellow,
// pause→gray, anything else default.
func nodeAvailColor(a string) tcell.Color {
	switch a {
	case "active":
		return tcell.ColorGreen
	case "drain":
		return tcell.ColorYellow
	case "pause":
		return tcell.ColorGray
	default:
		return tcell.ColorWhite
	}
}

// nodeStateColor colours a node's state: ready→green, down→red, else yellow.
func nodeStateColor(s string) tcell.Color {
	switch s {
	case "ready":
		return tcell.ColorGreen
	case "down", "disconnected":
		return tcell.ColorRed
	case "unknown":
		// Not red: red is a confirmed failure, and this is the manager saying
		// it does not know. Yellow is the honest colour for missing news.
		return tcell.ColorYellow
	default:
		return tcell.ColorYellow
	}
}

// serviceParent returns the service node that owns leaf, or nil. tview.TreeNode
// exposes no parent pointer, so we scan the (shallow, two-level) tree.
func serviceParent(root, leaf *tview.TreeNode) *tview.TreeNode {
	if p := parentOf(root, leaf); p != nil && isServiceNode(p) {
		return p
	}
	return nil
}

// serviceColor maps a service's running/desired task counts to a health color
// for the containers tree: grey when scaled to zero (0/0), red when down (0/n),
// orange when partial (e.g. 1/3), aqua when healthy (n/n).
func serviceColor(running, desired int) tcell.Color {
	switch {
	case desired == 0:
		return tcell.ColorGray
	case running == 0:
		return tcell.ColorRed
	case running != desired:
		return tcell.ColorOrange
	default:
		return tcell.ColorAqua
	}
}

// jobColor is the same verdict for a job, where "nothing running" is not an
// outage: grey when it asks for nothing, aqua when every wanted completion
// happened, orange while it is still working, and red only when it has neither
// finished nor started anything.
func jobColor(completed, total, running int) tcell.Color {
	switch {
	case total == 0:
		return tcell.ColorGray
	case completed >= total:
		return tcell.ColorAqua
	case completed > 0 || running > 0:
		return tcell.ColorOrange
	default:
		return tcell.ColorRed
	}
}

// progressColor is the colour of a service row: the running/desired verdict for
// something meant to stay up, the completion verdict for a job.
func progressColor(s resolve.Service) tcell.Color {
	have, want := s.Progress()
	if s.IsJob() {
		return jobColor(have, want, s.Running)
	}
	return serviceColor(have, want)
}

// progressCount renders the counter column: how much of the service is up, or —
// for a job — how much of it is done.
//
// Deliberately narrower than `docker service ls`, which prints both pairs for a
// job ("0/1 (3/3 completed)"). The mode column right beside it already says
// this is a job, and the live count of a finished job is noise the column can
// not afford: it is the widest cell in a fixed-width row.
func progressCount(s resolve.Service) string {
	have, want := s.Progress()
	return fmt.Sprintf("%d/%d", have, want)
}

// svcColumns holds the padding widths that align the service rows in the
// containers tree, computed across all services — not just the filtered ones,
// so the alignment stays stable while filtering.
//
// The widths also stay stable over TIME, which is the same argument on the
// other axis. Recomputing them from scratch on every refresh made the layout
// follow whatever happened to exist at that instant: a swarm that runs a
// `replicated-job` every minute has a mode column that is 14 wide for one
// frame and 10 for the next, and everything to the right of it jumps four
// columns, twice a minute, forever. See growTo.
type svcColumns struct {
	name, mode, repl, image int
}

// svcColumnFloors are the minimum widths, so the first frame is not narrower
// than the second and then visibly settles.
var svcColumnFloors = svcColumns{repl: len("0/0")}

// growTo widens each column to fit other and never narrows — the whole point.
// A column shrinks only when something explicit happens (a manual refresh, a
// structural change, a different cluster), never because a transient service
// came and went.
//
// Deliberately not "shrink after N quiet cycles": the service that causes this
// is periodic, and any delay shorter than its period just makes the same two
// jumps per minute slower. The period is set by someone else's cron, so there
// is no safe value to pick.
func (c *svcColumns) growTo(other svcColumns) {
	c.name = max(c.name, max(other.name, svcColumnFloors.name))
	c.mode = max(c.mode, max(other.mode, svcColumnFloors.mode))
	c.repl = max(c.repl, max(other.repl, svcColumnFloors.repl))
	c.image = max(c.image, max(other.image, svcColumnFloors.image))
}

// serviceRow renders a service group node like a docker service ls line —
// "name  mode  running/desired  image  ports", or completed/total for a job —
// padded to the shared column widths. Image and ports are appended only when present, and trailing padding
// is trimmed so a selected row's highlight does not run past the text.
func serviceRow(s resolve.Service, c svcColumns, imageSuffix, usage string) string {
	var b strings.Builder
	// The security marker leads the row, in a fixed-width slot every service
	// occupies, so the shields form a vertical scan column and the name column
	// still lines up. Leading it also keeps it visible when a long image ref or
	// port list pushes the end of the row past the right edge.
	b.WriteString(securityBadge(s.Risks))
	fmt.Fprintf(&b, "%-*s  %-*s  %-*s",
		c.name, orDash(s.Name), c.mode, orDash(s.Mode), c.repl, progressCount(s))
	if c.image > 0 {
		// The resolved-version / ↑ annotation sits inside the image cell so it
		// reads right next to the image URI, not far off in the ports column.
		fmt.Fprintf(&b, "  %-*s", c.image, orDash(s.Image)+imageSuffix)
	}
	if s.Ports != "" {
		fmt.Fprintf(&b, "  %s", s.Ports)
	}
	// The update and resource badges are appended last (after the padded columns
	// are trimmed) so their tags never disturb the column alignment.
	return strings.TrimRight(b.String(), " ") + updateBadge(s.UpdateState) + usage
}

// securityBadgeWidth is the fixed leading slot every service row reserves for
// the security marker, so flagged and clean rows keep the same name column.
const securityBadgeWidth = 2

// securityBadge returns the leading shield marker for a service with an
// actionable security-scan finding, padded to securityBadgeWidth; a clean
// service gets the same width in spaces. Informational (low) findings do not
// badge — they hold for nearly every service and would mark every row. It is a
// single "risk present" indicator (🛡 is an emoji; terminals ignore its
// foreground colour) — the per-finding severity is shown in the overlay (!).
//
// The bare shield (no variation selector) is deliberate: tview measures U+1F6E1
// as one cell, so "🛡 " and "  " are the same width to the layout.
func securityBadge(risks []secscan.Finding) string {
	if !secscan.Actionable(risks) {
		return strings.Repeat(" ", securityBadgeWidth)
	}
	return "🛡 "
}

// updateStatusLabel maps a swarm rolling-update state to a short glyph+label and
// whether an update is actually in flight. Finished ("completed" /
// "rollback_completed") and absent states report active=false. The colour name
// is the accompanying severity tint (yellow = normal, orange/red = attention).
// Shared by the tree badge and the inspect UPDATE line so both stay in sync.
func updateStatusLabel(state string) (label, color string, active bool) {
	switch state {
	case "updating":
		return "⟳ updating", "yellow", true
	case "rollback_started":
		return "↺ rolling back", "orange", true
	default:
		// Only the actively-converging states above are badged. Swarm keeps
		// UpdateStatus.State indefinitely, and the default failure_action is
		// "pause", so a single failed task in some past deploy pins a service at
		// "paused" for weeks — a stale, misleading signal, not an ongoing update.
		// "paused"/"rollback_paused" (and completed/none) therefore get no badge.
		return "", "", false
	}
}

// updateBadge renders a coloured in-progress marker for a service whose rolling
// update (or rollback) is still running, so a mid-update service is visible at a
// glance in the tree. The colour tags render because the tree draws node text
// through tview's tag-aware printer.
func updateBadge(state string) string {
	label, color, active := updateStatusLabel(state)
	if !active {
		return ""
	}
	return "  [" + color + "]" + label + "[-]"
}

// shortVolume abbreviates long anonymous-volume hashes (64-char hex) for display
// while leaving human-named volumes intact. The full name is still used for
// operations.
func shortVolume(name string) string {
	if len(name) >= 32 && isHexString(name) {
		return name[:12] + "…"
	}
	return name
}

func isHexString(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// shellLabel renders a menu shell entry, greying it once a probe confirms the
// container can't start that shell.
func shellLabel(name string, ok, probed bool) string {
	if probed && !ok {
		return "[gray]" + name + " (not available)[-]"
	}
	return name
}

// vimListKeys maps j/k/g/G to ↓/↑/Home/End for a tview.List. TextView brings
// these natively, but List does not: it reads runes as item shortcuts, and
// since our items carry no shortcut the lookup finds nothing and swallows the
// key. Every List that the operator navigates has to run its input through
// this, or vim movement silently dies the moment an overlay takes focus.
func vimListKeys(ev *tcell.EventKey) *tcell.EventKey {
	if ev.Key() != tcell.KeyRune {
		return ev
	}
	switch ev.Rune() {
	case 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	case 'g':
		return tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone)
	case 'G':
		return tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone)
	}
	return ev
}

func headerCell(text string) *tview.TableCell {
	return tview.NewTableCell(text).SetTextColor(tcell.ColorYellow).SetAttributes(tcell.AttrBold).SetSelectable(false)
}

// selectedRow returns the item the operator has highlighted in a single-column
// table whose row 0 is a header, mapping table row → rows[row-1], and false when
// the header (or an empty area) is selected. One helper for every tab's
// "which row is selected" getter, so the header-offset lives in one place.
func selectedRow[T any](t *tview.Table, rows []T) (T, bool) {
	row, _ := t.GetSelection()
	i := row - 1
	if i < 0 || i >= len(rows) {
		var zero T
		return zero, false
	}
	return rows[i], true
}

func joinNodes(nodes []resolve.Node) string {
	return joinComma(nodeNames(nodes))
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}

// overlayFooterReserve is the number of bottom rows a centered overlay must
// leave uncovered so the always-present two-line footer (drawn by the "main"
// page beneath every overlay) stays visible — the key hints live there.
const overlayFooterReserve = 2

// centered floats p (its natural width×height) in the upper-centre of the
// screen, transparent around it so the page beneath shows through — used for
// every menu/editor/inspect overlay. Crucially it never draws over the bottom
// overlayFooterReserve rows: on a terminal too short for p's natural height the
// box shrinks to fit the space above the footer instead of covering it (the old
// fixed-height Flex would overflow and hide the shortcut footer). p shrinks; the
// footer survives.
func centered(p tview.Primitive, width, height int) tview.Primitive {
	return &centeredBox{Box: tview.NewBox(), inner: p, width: width, height: height}
}

// Recurring overlay dimensions. The widths still vary per overlay (a filename
// prompt is narrower than a set-labels prompt), but the two shared intents —
// "single-input prompt row" and "standard property form" — get a name so new
// overlays inherit consistent heights instead of copying a stray literal.
const (
	// promptHeight fits a bordered single-line InputField (border + input + border).
	promptHeight = 3
	// formWidth is the standard width for a multi-field property form.
	formWidth = 74
)

// centeredPrompt floats a single-input prompt of the given width at promptHeight.
func centeredPrompt(p tview.Primitive, width int) tview.Primitive {
	return centered(p, width, promptHeight)
}

// centeredBox is the size-aware layout backing centered(). It embeds Box only
// for the default Primitive plumbing; it draws nothing itself (transparent
// margins) and delegates focus/input/mouse/paste to its inner primitive, which
// callers focus directly.
type centeredBox struct {
	*tview.Box
	inner         tview.Primitive
	width, height int
}

func (c *centeredBox) Draw(screen tcell.Screen) {
	x, y, w, h := c.GetRect()
	if w <= 0 || h <= 0 {
		return
	}
	avail := h - overlayFooterReserve
	if avail < 1 {
		avail = h // terminal shorter than the footer itself: don't vanish
	}
	// Recede the backdrop so the dialog stands out; keep the footer rows crisp,
	// they carry this overlay's own shortcuts.
	dimBehind(screen, x, y, w, avail)
	bw, bh := c.width, c.height
	if bw > w {
		bw = w
	}
	if bh > avail {
		bh = avail
	}
	bx := x + (w-bw)/2
	by := y + (avail-bh)/2
	if by < y {
		by = y
	}
	c.inner.SetRect(bx, by, bw, bh)
	c.inner.Draw(screen)
}

func (c *centeredBox) Focus(delegate func(p tview.Primitive)) { delegate(c.inner) }
func (c *centeredBox) HasFocus() bool                         { return c.inner.HasFocus() }
func (c *centeredBox) InputHandler() func(*tcell.EventKey, func(p tview.Primitive)) {
	return c.inner.InputHandler()
}
func (c *centeredBox) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(p tview.Primitive)) (bool, tview.Primitive) {
	return c.WrapMouseHandler(func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(p tview.Primitive)) (bool, tview.Primitive) {
		return swallowBackgroundMouse(c.Box, c.inner, action, event, setFocus)
	})
}
func (c *centeredBox) PasteHandler() func(string, func(p tview.Primitive)) {
	return c.inner.PasteHandler()
}

// swallowBackgroundMouse makes an overlay modal for the mouse: it forwards the
// event to the inner dialog, and if the dialog did not consume it but it landed
// inside the overlay's (full-screen) rect, it swallows the event here instead of
// letting it fall through. Without this, tview.Pages routes any unconsumed event
// down to the next visible page — the main view — so scrolling or clicking in the
// transparent margin around a dialog would still drive the window beneath it. A
// left-press also (re)focuses the dialog, so a stray click can't strand focus.
func swallowBackgroundMouse(box *tview.Box, inner tview.Primitive, action tview.MouseAction, event *tcell.EventMouse, setFocus func(p tview.Primitive)) (bool, tview.Primitive) {
	consumed, capture := inner.MouseHandler()(action, event, setFocus)
	if !consumed && box.InRect(event.Position()) {
		if action == tview.MouseLeftDown {
			setFocus(inner)
		}
		consumed = true
	}
	return consumed, capture
}

// scrim wraps a self-centering overlay (e.g. tview.Modal, which positions itself
// within the rect it is given) so the mouse can't fall through to the page
// beneath — the same guard centeredBox applies to the primitives it positions.
// Wrap a modal in newScrim before adding it as a page.
type scrim struct {
	*tview.Box
	inner tview.Primitive
}

func newScrim(inner tview.Primitive) *scrim {
	return &scrim{Box: tview.NewBox(), inner: inner}
}

func (s *scrim) Draw(screen tcell.Screen) {
	x, y, w, h := s.GetRect()
	avail := h - overlayFooterReserve
	if avail < 1 {
		avail = h
	}
	dimBehind(screen, x, y, w, avail) // recede the backdrop; keep the footer crisp
	s.inner.SetRect(s.GetRect())
	s.inner.Draw(screen)
}
func (s *scrim) Focus(delegate func(p tview.Primitive)) { delegate(s.inner) }
func (s *scrim) HasFocus() bool                         { return s.inner.HasFocus() }
func (s *scrim) InputHandler() func(*tcell.EventKey, func(p tview.Primitive)) {
	return s.inner.InputHandler()
}
func (s *scrim) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(p tview.Primitive)) (bool, tview.Primitive) {
	return s.WrapMouseHandler(func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(p tview.Primitive)) (bool, tview.Primitive) {
		return swallowBackgroundMouse(s.Box, s.inner, action, event, setFocus)
	})
}
func (s *scrim) PasteHandler() func(string, func(p tview.Primitive)) {
	return s.inner.PasteHandler()
}

// footerKeys builds the markup for the bottom footer from key,description pairs
// (dynamic-colour tags, as the footer is a TextView). Overlays hand it to
// overlayFor so the single bottom footer describes the active view's keys.
func footerKeys(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(&b, " [yellow]%s[white] %s ", pairs[i], pairs[i+1])
	}
	return b.String()
}

// generic info modal. Every message is also logged (with context) so the log
// viewer / file has a record of what the operator was shown.
func (u *ui) info(msg string) {
	clientlog.L().Info("ui notice", "msg", msg)
	m := tview.NewModal().SetText(msg).AddButtons([]string{"OK"})
	// A notice takes no footer and gives focus back to nobody in particular:
	// whatever opens next focuses itself, and there is usually something.
	ov := u.overlayFor(pageInfo, nil, "")
	m.SetDoneFunc(func(int, string) { ov.Close() })
	ov.show(newScrim(m), m)
}

// confirm shows a two-button confirmation modal (confirmLabel + "Cancel") and
// owns the modal, its page, and focus restoration — the two-button sibling of
// info. onConfirm runs only when the operator picks the confirm button; on
// cancel, focus returns to back. On confirm, onConfirm decides what happens
// next (open a progress overlay, start async work, restore focus itself, …),
// so it must handle its own focus. Only one confirm is ever open at a time, so
// a single shared page name is safe.
func (u *ui) confirm(msg, confirmLabel string, back tview.Primitive, onConfirm func()) {
	app := u.app
	m := tview.NewModal().
		SetText(msg).
		AddButtons([]string{confirmLabel, "Cancel"})
	// Close before onConfirm runs, not after: onConfirm routinely opens the next
	// overlay, and that one has to be able to take the page and the focus.
	// Cancel is the path that wants `back`, so Close only restores focus there.
	ov := u.overlayFor(pageConfirm, nil, "")
	m.SetDoneFunc(func(_ int, label string) {
		ov.Close()
		if label != confirmLabel {
			app.SetFocus(back)
			return
		}
		onConfirm()
	})
	ov.show(newScrim(m), m)
}

// flash briefly replaces the footer with a status message, then puts the
// footer back.
//
// "Back" is u.footerBase — what the footer's actual owner (the tab, or an open
// overlay) last asked for — NOT a snapshot of the text taken here. Snapshotting
// was the bug: press a flashing key twice inside the window and the second
// flash captured the FIRST flash's message as what to restore, so the key hints
// vanished until the next tab switch. Same for opening an overlay mid-flash.
//
// The restore is still guarded on the text: if anything changed the footer
// meanwhile, that owner keeps it.
func (u *ui) flash(msg string) {
	help, app := u.help, u.app
	help.SetText(msg)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		app.QueueUpdateDraw(func() {
			if help.GetText(false) == msg {
				help.SetText(u.footerBase)
			}
		})
	}()
}

// setFooter sets the footer AND records it as the baseline a flash returns to.
// Every owner of the footer goes through this; only flash writes the widget
// directly, precisely because a flash is not an owner.
func (u *ui) setFooter(markup string) {
	u.footerBase = markup
	u.help.SetText(markup)
}

// updateStatus composes the footer status line from the active context, the
// last cluster probe, the forward count and (on the volumes tab) the number
// of selected volumes. Assigned here — after `active` exists — and called by
// the refreshers, setTab and the volume-selection toggle.
func (u *ui) updateStatus() {
	activeCtx, clusterText, active := u.activeCtx, u.clusterText, u.active
	forwards := u.forwards
	selectedVols := u.selectedVols
	status := u.status
	parts := []string{fmt.Sprintf("[aqua]ctx[white] %s", activeCtx)}
	if clusterText != "" {
		parts = append(parts, clusterText)
	}
	if total, act := forwards.counts(); total > 0 {
		if act == total {
			parts = append(parts, fmt.Sprintf("[aqua]%d[white] fwd", total))
		} else {
			parts = append(parts, fmt.Sprintf("[aqua]%d[white]/%d fwd", act, total))
		}
	}
	if active == "volumes" && len(selectedVols) > 0 {
		parts = append(parts, fmt.Sprintf("[yellow]▣ %d selected[white]", len(selectedVols)))
	}
	status.SetText(" " + strings.Join(parts, "  ·  "))
}

// toggleMouse flips tview's mouse capture. With it off, the terminal's own
// text selection / copy works again (tview otherwise grabs the mouse).
func (u *ui) toggleMouse() {
	app := u.app
	u.mouseEnabled = !u.mouseEnabled
	app.EnableMouse(u.mouseEnabled)
	if u.mouseEnabled {
		u.flash(" [green]mouse ON[white] — app handles the mouse")
	} else {
		u.flash(" [green]mouse OFF[white] — select & copy with your terminal (m to re-enable)")
	}
}

// The background refreshers must not run while an overlay is open: their
// renderContainers / remarkUsage pass walks the whole tree on the UI goroutine
// — the same goroutine that services keystrokes in the overlay — and it rebuilds
// and reselects the tree underneath whatever the operator is looking at.
//
// Two gates, because one cannot do both jobs:
//
//   - overlayOpen is DERIVED from the page stack, so it is true for EVERY
//     overlay, including the ones that register nothing. It is the invariant,
//     and it guards the expensive half (the apply). tview's page list may only
//     be read from the UI goroutine, so this is main-loop only.
//   - anyOverlayOpen reads the set that every overlay registers in.
//     It is readable from any goroutine, and it guards the cheap half (skipping
//     the fetch, which fans out to every node). Being a best-effort optimisation
//     is exactly why a stale answer here costs nothing.

// overlayOpen reports whether anything floats over the main page. UI goroutine
// only.
func (u *ui) overlayOpen() bool {
	name, _ := u.pages.GetFrontPage()
	return name != pageMain
}

// anyOverlayOpen reports whether a registered overlay is open. Safe from any
// goroutine.
func (u *ui) anyOverlayOpen() bool {
	u.overlayMu.Lock()
	defer u.overlayMu.Unlock()
	return len(u.overlays) > 0
}

// markOverlay records an open overlay under key and returns its release, which
// is idempotent: a modal dismissed twice, or a page replaced by a second one of
// the same name, must not take the entry away from whoever still holds it.
func (u *ui) markOverlay(key string) func() {
	u.overlayMu.Lock()
	u.overlays[key] = true
	u.overlayMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			u.overlayMu.Lock()
			delete(u.overlays, key)
			u.overlayMu.Unlock()
		})
	}
}

// helpFor builds the footer key hints from the live keymap, so remapped keys
// show correctly. j/k, Enter and Tab/1-6 are fixed and stay literal.
// helpFor builds the per-tab footer. The two universal escape hatches — "?"
// (full-key help) and quit — are FRONT-LOADED, so on a terminal too narrow for
// the whole line it is the tab-specific tail that clips, never the way out or
// the pointer to every other key. The complete list (copy/mouse/tabs/refresh
// and each tab's keys) lives in the "?" overlay (showHelp).
func (u *ui) helpFor(name string) string {
	km := u.km
	kl := keyLabel
	head := fmt.Sprintf(" [yellow]?[white] help  [yellow]%s[white] quit   ", kl(km.Quit))
	switch name {
	case "containers":
		// This line is the widest footer and already fills a ~117-column
		// terminal; anything past the edge wraps onto a hidden second row. So
		// labels here are kept terse to make room for X (destructive, and it
		// must not be the thing that silently falls off the end).
		return head + fmt.Sprintf("[yellow]j/k[white] move  [yellow]Enter[white] open  [yellow]%s[white] logs  [yellow]%s/%s[white] fold  [yellow]%s[white] inspect  [yellow]%s[white] search  [yellow]%s[white] forward  [yellow]%s[white] risks  [yellow]%s[white] stacks  [red]X[white] remove",
			kl(km.Logs), kl(km.Fold), kl(km.Unfold), kl(km.ContainerInspect), kl(km.Search), kl(km.Forward), kl(km.SecurityRisks), kl(km.StackGroup))
	case "volumes":
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]%s[white] search  [yellow]%s[white] new  [yellow]%s[white] select  [yellow]%s[white] all  [yellow]%s[white] attach  [yellow]%s[white] delete  [yellow]%s[white] prune  [yellow]Enter[white] nodes  [yellow]%s[white] used by  [yellow]%s[white] sort",
			kl(km.Search), kl(km.VolNew), kl(km.VolSelect), kl(km.VolSelectAll), kl(km.VolAttach), kl(km.VolDelete), kl(km.VolPrune), kl(km.VolUsedBy), kl(km.VolSort))
	case "networks":
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/%s[white] attached  [yellow]%s[white] new", kl(km.NetAttached), kl(km.NetNew))
	case "secrets":
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] new  [yellow]%s[white] delete", kl(km.SecNew), kl(km.SecDelete))
	case "contexts":
		// The sidebar's footer, shown while it holds the keyboard. Esc is first
		// after the movement keys because it is the way back out — the sidebar
		// is the one focusable thing here that is not a tab, so "how do I get
		// back" is the question it has to answer.
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Esc[white] back  [yellow]Enter/%s[white] switch  [yellow]i[white] details  [yellow]%s[white] new  [yellow]%s[white] delete",
			kl(km.CtxUse), kl(km.CtxNew), kl(km.CtxDelete))
	case "nodes":
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] edit labels  [yellow]%s[white] availability  [yellow]%s[white] reclaim images",
			kl(km.NodeLabels), kl(km.NodeAvail), kl(km.NodeImages))
	case "configs":
		return head + "[yellow]j/k[white] up/down  [yellow]Enter/i[white] details (with content)"

	default:
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] stop  [yellow]%s[white] copy url",
			kl(km.FwdStop), kl(km.FwdCopyURL))
	}
}

// showHelp opens a scrollable overlay listing every keybinding — the complete
// reference the single-row footer cannot hold. It is generated from the live
// keymap, so remapped keys show correctly. Bound to "?" on every tab.
func (u *ui) showHelp() {
	km, app := u.km, u.app
	kl := keyLabel
	var b strings.Builder
	sec := func(title string) { fmt.Fprintf(&b, "\n[aqua]%s[-]\n", title) }
	line := func(keys, desc string) { fmt.Fprintf(&b, "  [yellow]%-9s[white] %s\n", keys, desc) }

	b.WriteString("[aqua]Global[-]\n")
	line("?", "this help")
	line(kl(km.Quit), "quit")
	line(kl(km.Refresh), "refresh the current tab + cluster")
	line(kl(km.Copy), "copy the current list to the clipboard")
	line(kl(km.ToggleMouse), "toggle mouse on/off")
	line("Tab", "next tab")
	line("1–7", "jump to a tab by number")
	line(kl(km.CtxFocus), "focus the contexts sidebar · Esc leaves")
	line("`", "toggle the client log view")
	line("Esc", "close the current overlay / dialog")

	sec("Stacks/Services")
	line("Enter", "expand a service · open a container's menu")
	line(kl(km.Logs), "logs (service or container)")
	line(kl(km.ContainerInspect), "inspect: service/task detail + editors")
	line(kl(km.Fold)+"/"+kl(km.Unfold), "fold / unfold")
	line(kl(km.Forward), "port-forward the task under the cursor")
	line(kl(km.SecurityRisks), "security-risks overlay (spec hardening checks)")
	line(kl(km.StackGroup), "group services by stack / flat list")
	line(kl(km.StackFile), "stack file: export the stack, or compare one against it")
	line("X", "remove the service under the cursor (confirmed)")
	line(kl(km.Search), "search services / containers / nodes")

	sec("Service inspect (" + kl(km.ContainerInspect) + ")")
	line("a", "actions menu (all edits below, no Shift needed)")
	line("Enter", "NETWORKS: expand a network → its containers")
	line("1 2 3", "table · stats (cpu/memory) · raw JSON")
	line("t", "cycle those three views")
	line("d / D", "diff spec · why (placement)")
	line("R", "roll back to the previous version")
	line("s / f", "scale · force-update")
	line("u", "set the image version (update / pin / revert)")
	line("p l e", "edit ports · labels · env")
	line("n S v", "edit networks · secrets · mounts")
	line("r P", "edit resources · placement")
	line("X", "remove the service")

	sec("Volumes")
	line(kl(km.VolNew), "new volume")
	line(kl(km.VolSelect)+"/"+kl(km.VolSelectAll), "select / select all")
	line(kl(km.VolAttach), "attach to a service")
	line(kl(km.VolDelete)+"/"+kl(km.VolPrune), "delete / prune unused")
	line("Enter", "which nodes hold it")
	line(kl(km.VolUsedBy), "used-by (containers / services)")
	line(kl(km.VolSort)+"/"+kl(km.VolSortRev), "sort / reverse")
	line(kl(km.Search), "search")

	sec("Networks")
	line("Enter/"+kl(km.NetAttached), "attached services")
	line(kl(km.NetNew), "new network")

	sec("Secrets")
	line("Enter / i", "details")
	line(kl(km.SecNew)+"/"+kl(km.SecDelete), "new / delete")

	sec("Contexts (sidebar, not a tab)")
	line(kl(km.CtxFocus), "focus the sidebar from any tab; again or Esc to leave")
	line("Enter / "+kl(km.CtxUse), "switch to that cluster — keeps your place, filters and forwards")
	line("i", "context details (endpoint, jump hosts)")
	line(kl(km.CtxNew)+"/"+kl(km.CtxDelete), "new / delete")
	line("▶ · ✗", "active · connected (instant) · last attempt refused")

	sec("Nodes")
	line("Enter / i", "details")
	line(kl(km.NodeLabels), "edit labels")
	line(kl(km.NodeAvail), "availability: active / pause / drain")
	line(kl(km.NodeImages), "reclaim image disk space on the node")

	sec("Configs")
	line("Enter / i", "details, including the config's content")

	sec("Forwards")
	line("Enter / i", "details")
	line(kl(km.FwdStop), "stop")
	line(kl(km.FwdCopyURL), "copy URL")

	sec("Log view (`)")
	line("f", "follow on/off")
	line("F", "cycle format")
	line("a", "auto-detect format")
	line("l", "cycle min level")
	line("/", "filter message")
	line("↑/↓", "scroll")

	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	tv.SetText(strings.TrimLeft(b.String(), "\n"))
	tv.SetBorder(true).SetTitle(" keybindings ")
	prev := app.GetFocus()
	ov := u.overlayFor(pageHelp, prev, footerKeys("j/k", "scroll", "Esc", "close"))
	closeHelp := ov.Close
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == '?')):
			closeHelp()
			return nil
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
		return ev
	})
	ov.show(centered(tv, 60, 24), tv)
}

func (u *ui) renderTabBar(active string) {
	u.tabBar.setActive(active)
}

func (u *ui) setTab(name string) {
	content, app := u.content, u.app
	ctree, vtable, ftable := u.ctree, u.vtable, u.ftable
	nettable, sectable := u.nettable, u.sectable
	notable := u.notable
	u.active = name
	content.SwitchToPage(name)
	u.curHelp = u.helpFor(name)
	// Switching tab takes the keyboard back from the sidebar, so the footer it
	// borrowed goes back too rather than being restored over the new tab's.
	u.sidebarReturn = ""
	u.sidebarHasFocus = false
	u.sidebar.SetBorderColor(palette.border)
	u.setFooter(u.curHelp)
	u.renderTabBar(name)
	u.updateStatus() // the selection count shows only on the volumes tab
	switch name {
	case "containers":
		app.SetFocus(ctree)
	case "volumes":
		app.SetFocus(vtable)
		u.loadVolumes()
	case "forwards":
		app.SetFocus(ftable)
		u.renderForwards()
	case "networks":
		app.SetFocus(nettable)
		u.loadNetworks()
	case "secrets":
		app.SetFocus(sectable)
		u.loadSecrets()
	case "nodes":
		app.SetFocus(notable)
		u.loadNodes()
	case "configs":
		app.SetFocus(u.cfgtable)
		u.loadConfigs()
	}
}

func (u *ui) startSearch(mode string) {
	root, search, app := u.root, u.search, u.app
	u.searchMode = mode
	root.ResizeItem(search, 1, 0)
	u.savedHelp = u.footerBase
	u.setFooter(" [yellow]type[white] to filter   [yellow]Enter[white] keep filter & exit   [yellow]Esc[white] clear & exit")
	if mode == "volumes" {
		search.SetPlaceholder("filter volumes / driver / node")
		search.SetText(u.volFilter)
	} else {
		search.SetPlaceholder("filter services / containers / nodes")
		search.SetText(u.filter)
	}
	app.SetFocus(search)
}

// refreshCluster probes the swarm in the background and updates the footer
// summary. The agent probe (a Version RPC per node) can be slow, so it runs
// off the UI goroutine and pushes the result back via QueueUpdateDraw.
func (u *ui) refreshCluster() {
	r, ctx := u.r, u.ctx
	cfg, f := u.cfg, u.f
	gen := u.generation()
	go func() {
		nodes, err := r.Nodes(ctx)
		if err != nil {
			u.onCluster(gen, func() {
				u.clusterText = "[red]cluster: unreachable[white]"
				u.updateStatus()
			})
			return
		}
		agents := 0
		for _, h := range checkNodes(ctx, cfg, nodes, f.connectTimeout) {
			if h.err == nil {
				agents++
			}
		}
		u.onCluster(gen, func() {
			u.clusterText = fmt.Sprintf("[aqua]%d[white] nodes · [aqua]%d[white]/%d agents", len(nodes), agents, len(nodes))
			u.updateStatus()
		})
	}()
}

// refreshForwardViews repaints everything a forward's state feeds: the
// footer counter, the forwards table, and the tree annotations. Must run on
// the UI goroutine.
func (u *ui) refreshForwardViews() {
	u.updateStatus()
	u.renderForwards()
	u.renderContainers()
}

// yankCurrent copies the active tab's list to the system clipboard via the
// terminal (OSC52), so it also works over ssh when the terminal supports it.
func (u *ui) yankCurrent() {
	screen := u.screen
	active, activeCtx := u.active, u.activeCtx
	// The context list is no longer a tab, so "what is on screen" is no longer
	// the same question as "which tab is this": when the sidebar holds the
	// keyboard, that is what the operator means by copy.
	if u.sidebarFocused() {
		active = "contexts"
	}
	croot := u.croot
	forwards := u.forwards
	nets, secs, ctxs := u.nets, u.secs, u.ctxs
	shownVols := u.shownVols
	volUsage, volSizes := u.volUsage, u.volSizes
	if screen == nil {
		return
	}
	var b strings.Builder
	switch active {
	case "containers":
		// Copy the tree as displayed, indenting by depth so a stack-grouped tree
		// copies as a grouped tree (and an ungrouped one is unchanged).
		for _, stackOrSvc := range croot.GetChildren() {
			fmt.Fprintln(&b, trimFoldMarker(stackOrSvc.GetText()))
			for _, svc := range stackOrSvc.GetChildren() {
				fmt.Fprintf(&b, "  %s\n", trimFoldMarker(svc.GetText()))
				for _, c := range svc.GetChildren() {
					fmt.Fprintf(&b, "    %s\n", c.GetText())
				}
			}
		}
	case "forwards":
		fmt.Fprintln(&b, "LOCAL\tREMOTE\tCONTAINER\tSERVICE\tNODE\tSTATE")
		for _, e := range forwards.list() {
			local := "-"
			if p := e.boundPort(); p > 0 {
				local = fmt.Sprintf("127.0.0.1:%d", p)
			}
			fmt.Fprintf(&b, "%s\t%d\t%s\t%s\t%s\t%s\n",
				local, e.remote, shortID(e.cand.ContainerID),
				orDash(e.cand.Service), orDash(e.cand.NodeName), e.state)
		}
	case "networks":
		fmt.Fprintln(&b, "NETWORK\tDRIVER\tSCOPE\tTYPE\tSERVICES\tAGE")
		for _, n := range nets {
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%d\t%s\n",
				n.Name, orDash(n.Driver), orDash(n.Scope), networkType(n), len(n.Services), volumeAge(n.Created))
		}
	case "secrets":
		fmt.Fprintln(&b, "SECRET\tUSED BY\tAGE\tUPDATED\tLABELS")
		for _, s := range secs {
			fmt.Fprintf(&b, "%s\t%d\t%s\t%s\t%d\n", s.Name, len(s.Services), volumeAge(s.Created), volumeAge(s.Updated), len(s.Labels))
		}
	case "contexts":
		fmt.Fprintln(&b, "CONTEXT\tDOCKER HOST\tACTIVE")
		for _, c := range ctxs {
			mark := ""
			if c.Name == activeCtx {
				mark = "*"
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\n", c.Name, orDash(c.Host), mark)
		}
	default:
		// Copy the displayed (filtered + sorted) volumes.
		fmt.Fprintln(&b, "NAME\tDRIVER\tNODES\tUSED BY\tAGE\tSIZE")
		for _, v := range shownVols {
			used := "-"
			if n := len(volUsage[v.Name]); n > 0 {
				used = fmt.Sprintf("%d", n)
			}
			size := int64(-1)
			if s, ok := volSizes[v.Name]; ok {
				size = s
			}
			fmt.Fprintf(&b, "%s\t%s\t%d: %s\t%s\t%s\t%s\n", v.Name, orDash(v.Driver), len(v.Nodes), joinNodes(v.Nodes), used, volumeAge(v.Created), humanBytes(size))
		}
	}
	screen.SetClipboard([]byte(b.String()))
	u.flash(" [green]✓ copied to clipboard[white]")
}

func (u *ui) tabKeys(ev *tcell.EventKey) *tcell.EventKey {
	app, km := u.app, u.km
	active := u.active
	if ev.Key() == tcell.KeyRune && ev.Rune() == '?' {
		u.showHelp()
		return nil
	}
	if ev.Key() == tcell.KeyRune && ev.Rune() == '`' {
		u.toggleLogView()
		return nil
	}
	if ev.Key() == tcell.KeyTab {
		for i, name := range uiTabOrder {
			if name == active {
				u.setTab(uiTabOrder[(i+1)%len(uiTabOrder)])
				break
			}
		}
		return nil
	}
	// Tab-number keys, derived from the tab list rather than hardcoded — the tab
	// bar labels its tabs from the same positions, so the two cannot drift.
	if ev.Key() == tcell.KeyRune && ev.Rune() >= '1' && ev.Rune() <= '9' {
		if i := int(ev.Rune() - '1'); i < len(uiTabList) {
			u.setTab(uiTabList[i].key)
			return nil
		}
	}
	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case km.Quit:
			app.Stop()
			return nil
		case km.Refresh:
			switch active {
			case "containers":
				u.loadContainers()
				u.loadUsage()
			case "volumes":
				u.loadVolumes()
			case "networks":
				u.loadNetworks()
			case "secrets":
				u.loadSecrets()
			case "nodes":
				u.loadNodes()
				u.loadUsage() // the node detail shows measured usage next to reservations
			default:
				u.renderForwards()
			}
			// The context list comes from docker's local store, so it costs
			// nothing and is refreshed whatever tab you are on — it is always
			// on screen now.
			u.loadContexts()
			u.refreshCluster()
			return nil
		case km.Copy:
			u.yankCurrent()
			return nil
		case km.ToggleMouse:
			u.toggleMouse()
			return nil
		case km.CtxFocus:
			u.focusSidebar()
			return nil
		case 'j':
			return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
		case 'k':
			return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
		}
	}
	return ev
}

// The toggleable log viewer (closeLogView/openLogView/toggleLogView) is an
// overlay showing the in-memory log ring, refreshed live while open; it is
// opened/closed with the backtick key from any tab. Its state (logViewStop /
// logViewPrev / logViewOverlay) lives on u.
func (u *ui) closeLogView() {
	if u.logViewStop != nil {
		close(u.logViewStop)
		u.logViewStop = nil
	}
	// Close gives the footer back and returns focus to logViewPrev, which it
	// was opened with.
	if u.logViewOverlay != nil {
		u.logViewOverlay.Close()
		u.logViewOverlay = nil
	}
}

func (u *ui) openLogView() {
	// runCtx: this overlay shows the CLIENT's log ring, which has nothing to do
	// with any cluster — its refresher must not die because the operator
	// switched cluster while it was open.
	app, ctx := u.app, u.runCtx
	u.logViewPrev = app.GetFocus()
	u.logViewOverlay = u.overlayFor(pageLogView, u.logViewPrev,
		footerKeys("`", "toggle/close", "Esc/q", "close", "↑/↓", "scroll"))
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
	tv.SetBorder(true).SetTitle(" logs — newest at bottom ")
	refresh := func() {
		r := clientlog.RingBuffer()
		var b strings.Builder
		if r == nil {
			b.WriteString("[gray]logging is disabled (--log-level off)[-]")
		} else if lines := r.Lines(); len(lines) == 0 {
			b.WriteString("[gray](no log records yet)[-]")
		} else {
			for _, line := range lines {
				b.WriteString(colorLogLine(line))
				b.WriteByte('\n')
			}
		}
		tv.SetText(b.String())
		tv.ScrollToEnd()
	}
	refresh()
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == '`' || ev.Rune() == 'q')) {
			u.closeLogView()
			return nil
		}
		return ev
	})
	stop := make(chan struct{})
	u.logViewStop = stop
	u.logViewOverlay.show(tv, tv)
	go func() {
		tk := time.NewTicker(700 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-tk.C:
				app.QueueUpdateDraw(refresh)
			}
		}
	}()
}

func (u *ui) toggleLogView() {
	pages := u.pages
	if pages.HasPage(pageLogView) {
		u.closeLogView()
	} else {
		u.openLogView()
	}
}
