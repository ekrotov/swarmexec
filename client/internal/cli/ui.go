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

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/secscan"
	"swarmexec/client/internal/session"
	"swarmexec/client/internal/swarmops"
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
	onApply     func(context.Context, []string) error // runs off the UI goroutine (async), with the cluster context
	suggest     func(string) []string
	allowEdit   bool
	multiline   bool
	confirmNote func([]string) string
	entryAction *listEntryAction
	formPrompt  func(initial string, submit func(raw string) error, cancel func()) tview.Primitive
	formHeight  int      // formPrompt's box height (0: 19, the mount form's)
	staged      []string // when set, the editor opens with these entries staged over items
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
	pageEnvEdit          = "envedit"
	pageVimHelp          = "vimhelp"
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
	pageSecRotate        = "secrotate"
	pageSecRotateRun     = "secrotaterun"
	pageCtxForm          = "ctxform"
	pageCtxDetail        = "ctxdetail"
	pageNodeLabels       = "nodelabels"
	pageNodeLabelPrompt  = "nodelabelprompt"
	pageNodeDetail       = "nodedetail"
	pageNodeImages       = "nodeimages"
	pageNodePrune        = "nodeprune"
	pageNodePrunePrompt  = "nodepruneprompt"
	pageNodeSchedule     = "nodeschedule"
	pageNodeSchedPrompt  = "nodeschedprompt"
	pageAskpass          = "askpass"
	pageSSHAdd           = "sshadd"
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

	// Install the global theme before any primitive is constructed: tview reads
	// its palette at construction time and its border glyphs at draw time.
	applyTheme(u.cfg.UI.Dim)

	return u.run(keyWarnings)
}

// effectiveContextName is what the session is pointed at, by name: the explicit
// choice, else $DOCKER_CONTEXT, else "default" when $DOCKER_HOST is set, else
// docker's stored current, else "default" — dockerctx.ResolveName, the same
// order ResolveHost dials by. It is the label the footer shows, the sidebar's
// ▶ marker and the key a cluster is remembered under — never the thing it is
// resolved from (see clusterState.ctxOverride).
//
// It used to skip $DOCKER_HOST: the session dialled DOCKER_HOST's daemon while
// the footer said "ctx <stored current>" — production, typically — which is
// the one label that must never lie.
func effectiveContextName(override string) string {
	return dockerctx.ResolveName(override)
}

// ctxStatusLabel is how the footer names a context. While $DOCKER_HOST is set
// the built-in "default" context IS that daemon (for ResolveHost as for the
// docker CLI), and a bare "default" reads as the local socket — so it says
// where the requests go. Every other name is a stored context and dials its own
// endpoint whatever DOCKER_HOST says.
func ctxStatusLabel(name string) string {
	if name == "default" && os.Getenv("DOCKER_HOST") != "" {
		return "default [gray]($DOCKER_HOST)[white]"
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
	c.agents = agentcall.New(cfg)
	c.dcli = dcli
	c.r = resolve.New(dcli, addrModeOf(cfg))
	c.ctx, c.cancel = context.WithCancel(u.runCtx)
	// The registry cache re-marks service rows when a background version check
	// lands. It belongs to the cluster whose images it resolved, and the guard
	// is pointer identity: if that cluster is no longer the visible one, the
	// rows on screen are not its rows.
	c.regCache = swarmops.NewRegistryCache(func() {
		u.app.QueueUpdateDraw(func() {
			if u.clusterState != c {
				return
			}
			u.treeTab().remarkServices()
		})
	})
	c.err = nil
	return nil
}

// run drives one UI session over the infrastructure runUI prepared on u, in
// steps (review Q-01 split the former 190-line body): build the window
// (buildChrome), wire the search bar and the context list, load the first
// cluster and start the session-wide loops (startSession), then hand the
// terminal to tview (runScreen).
func (u *ui) run(keyWarnings []string) error {
	// forwards is the UI's only persistent background resource: a port forward
	// outlives the overlay that started it, unlike every stream here.
	forwards := newForwardRegistry()
	u.forwards = forwards
	defer forwards.stopAll()
	// Every cluster this session touched, not only the visible one.
	defer u.closeClusters()

	// Every tab builds its own widget: the tree in newTreeTab (ui_containers.go),
	// the tables in newVolumesTab, newNetworksTab and the rest.
	u.buildChrome()
	u.wireSearch()
	u.active = "containers"
	u.mouseEnabled = true
	u.wireContexts()
	u.startSession(keyWarnings)
	return u.runScreen()
}

// buildChrome builds the window around the tabs — tab bar, content beside the
// context sidebar, the hidden search bar, the two-line footer — as page
// "main", and promotes the shared widgets onto u so the infra methods
// (updateStatus, setTab, refreshForwardViews, …) can reach them.
func (u *ui) buildChrome() {
	// activeCtx (the name in the footer, and the row marked here) is set with the
	// cluster it belongs to — runUI for the first one, activateCluster for every
	// switch — so the name and the connection can never disagree.
	// No fixed header row: in the sidebar the first row is already a context
	// (see renderContexts), and SetFixed(1,0) would pin it out of reach of the
	// cursor.
	u.cxtable = tview.NewTable().SetBorders(false).SetSelectable(true, false)
	u.cxtable.SetSelectedStyle(u.selStyle)

	for i, t := range uiTabList {
		u.content.AddPage(t.key, u.tab(t.key).widget(), true, i == 0)
	}
	// `swarmexec ui web` opens pre-narrowed. The first cluster only: the
	// argument named a service there, not on whatever cluster comes next.
	u.treeTab().st().filter = u.service

	u.tabBar = newTabStrip(uiTabList, func(key string) { u.setTab(key) })
	u.buildFooter()
	// "/" search bar: hidden (height 0) until activated; filters the container
	// tree live by service / container id / node.
	u.search = tview.NewInputField().SetLabel("/ ").SetFieldWidth(0).
		SetPlaceholder("filter services / containers / nodes")
	u.buildBody()
	u.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.tabBar, 2, 0, false). // labels row + active-tab underline indicator
		AddItem(u.body, 0, 1, true).
		AddItem(u.search, 0, 0, false).
		AddItem(u.footer, 2, 0, false)
	// The sidebar width is decided at draw time from the space we actually have
	// — no resize events to listen for, no stale width after a resize. It is
	// decided in ROOT's draw func, which tview runs before root draws any child:
	// a Flex draws its focused item LAST, and the body nearly always has focus,
	// so a decision made in the body's own draw would reach the footer (the
	// context-list hint) only one frame later. Body and root share the width.
	u.root.SetDrawFunc(func(_ tcell.Screen, x, y, w, h int) (int, int, int, int) {
		u.resizeSidebar(w)
		return x, y, w, h
	})
	u.pages.AddPage(pageMain, u.root, true, true)
	u.updateStatusRight()
}

// buildFooter builds the two-line footer: the per-tab key hints on top, then
// one consolidated status line — active context · live cluster summary ·
// forward count · (on the volumes tab) selection count. u.updateStatus()
// composes the status line; the async cluster/forward refreshers feed it.
// clusterText holds the last cluster probe result so a forward or selection
// change can recompose without re-probing.
func (u *ui) buildFooter() {
	u.help = tview.NewTextView().SetDynamicColors(true)
	u.status = tview.NewTextView().SetDynamicColors(true)
	u.clusterText = "[gray]cluster: …[white]"
	// The status line shares its row with a right-aligned part: the version of
	// this client, and — while the context list is hidden for lack of width —
	// the key that brings it up. Its width follows its text (statusRightText).
	u.statusRight = tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight)
	u.statusRow = tview.NewFlex().
		AddItem(u.status, 0, 1, false).
		AddItem(u.statusRight, 0, 0, false)
	u.footer = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(u.help, 1, 0, false).
		AddItem(u.statusRow, 1, 0, false)
}

// buildBody puts the tab content and the context sidebar side by side. The
// context list is a column beside the content, not a tab (see
// ui_sidebar.go). It is framed so it reads as chrome rather than as a second
// table competing with the one in the middle.
func (u *ui) buildBody() {
	u.sidebar = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(u.cxtable, 0, 1, true)
	u.sidebar.SetBorder(true).SetTitle(" contexts ").SetBorderColor(palette.border)
	u.body = tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(u.content, 0, 1, true).
		// One blank column so the content's rightmost characters do not run
		// into the sidebar's border; tview clips rather than wraps, and without
		// it a long image or port list ends flush against the frame.
		AddItem(nil, 0, 0, false).
		AddItem(u.sidebar, 0, 0, false)
}

// wireSearch makes the "/" bar filter the tab it was opened on. u.savedHelp
// holds the footer help to restore when search closes; u.searchMode names the
// tab the shared bar filters (see startSearch).
func (u *ui) wireSearch() {
	search := u.search
	search.SetChangedFunc(func(text string) {
		// Filter locally against the cached data — no docker call per keystroke.
		if s := u.searchTab(); s != nil {
			s.setSearch(strings.TrimSpace(text))
		}
	})
	search.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			search.SetText("") // clears the active filter via SetChangedFunc
		}
		s := u.searchTab()
		if s == nil || s.searchText() == "" {
			u.root.ResizeItem(search, 0, 0) // nothing active — collapse the bar away
		}
		u.setFooter(u.savedHelp) // restore the tab help
		// Enter keeps the filter; the bar stays as an indicator.
		u.app.SetFocus(u.tab(u.searchMode).widget())
	})
}

// wireContexts gives the context list its keys: Enter activates the context
// under the cursor.
func (u *ui) wireContexts() {
	u.cxtable.SetInputCapture(u.contextKeys)
	u.cxtable.SetSelectedFunc(func(int, int) {
		if c, ok := u.selectedContext(); ok {
			u.activateContext(c)
		}
	})
}

// startSession loads what the first frame shows and starts the session-wide
// loops: the topology event stream and poll, and the watchdog.
func (u *ui) startSession(keyWarnings []string) {
	// The session context, NOT the cluster's: the loops below outlive a cluster
	// switch. The tree ticker refreshes whichever cluster is visible at the time,
	// and the watchdog is measuring this process, not a cluster.
	ctx := u.runCtx

	// The sidebar is visible from the first frame, so its content is loaded
	// before it: it comes from docker's local store, no network involved.
	u.loadContexts()
	u.treeTab().loadSync() // startup: before app.Run, so fetch+apply inline
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
	go u.pollTopology(ctx)

	// Surface any keys.yaml problems once, non-fatally, over the started UI.
	if len(keyWarnings) > 0 {
		u.info("keys.yaml:\n\n" + strings.Join(keyWarnings, "\n"))
	}

	// Responsiveness watchdog: a stall means something is blocking the loop.
	clientlog.L().Info("ui started", "log_level", u.g.logLevel)
	go u.watchdog(ctx)
}

// runScreen takes the terminal and runs the event loop until the UI quits.
func (u *ui) runScreen() error {
	app := u.app
	// Own the screen so we can post to the system clipboard (OSC52) on yank.
	screen, serr := tcell.NewScreen()
	if serr != nil {
		return &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: init screen: %w", serr)}
	}
	u.screen = screen
	app.SetScreen(screen)

	// Intercept tview's one hard-coded global key (Ctrl-C → Stop) so that inside
	// a container shell Ctrl-C reaches the remote process instead of quitting the
	// TUI; elsewhere it is swallowed (quit is on the `q` key). See ctrlCCapture.
	app.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		return ctrlCCapture(ev, app.GetFocus())
	})

	// From here ssh asks through a dialog instead of the terminal, which tview
	// is about to own. Before this line — the first cluster's connection and
	// load — the terminal is still the operator's, and ssh asks there.
	setAskpassPrompter(u.askpassPrompt)
	defer setAskpassPrompter(nil)

	if err := app.SetRoot(u.pages, true).EnableMouse(true).Run(); err != nil {
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

func networkType(n swarmops.Network) string {
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
func networkTypeColor(n swarmops.Network) tcell.Color {
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
func renderPlaceReport(rep swarmops.PlacementReport) string {
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
func renderTaskPS(b *strings.Builder, tasks []swarmops.TaskPS) {
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

// updateBadge renders a coloured in-progress marker for a service whose rolling
// update (or rollback) is still running, so a mid-update service is visible at a
// glance in the tree. The colour tags render because the tree draws node text
// through tview's tag-aware printer.
func updateBadge(state string) string {
	label, color, active := swarmops.UpdateStatusLabel(state)
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

// centeredGrow is centered for an editor: at least width×height, and on a
// bigger terminal frac of the screen (the space above the footer), so a large
// text gets the room there is. The footer stays free exactly as with centered.
func centeredGrow(p tview.Primitive, width, height int, frac float64) tview.Primitive {
	return &centeredBox{Box: tview.NewBox(), inner: p, width: width, height: height, fill: frac}
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
	fill          float64 // centeredGrow: the share of the screen to take, if more
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
	if c.fill > 0 {
		bw = max(bw, int(float64(w)*c.fill))
		bh = max(bh, int(float64(avail)*c.fill))
	}
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
// A key passed through dangerKey renders red instead of yellow.
//
// The Esc pair always comes first, wherever the caller listed it. A footer
// that overflows a narrow terminal is clipped from the right, and the way out
// is the one key that must never be the part that goes — the tabs put "? help
// q quit" in front for the same reason. Doing it here rather than at each call
// site keeps a new overlay from getting it wrong. The verb is the caller's:
// "back" for a detail view, "cancel" for a form or editor, "close" for a
// stream.
func footerKeys(pairs ...string) string {
	var b strings.Builder
	write := func(key, desc string) { fmt.Fprintf(&b, " %s %s ", keyTag(key), desc) }
	for i := 0; i+1 < len(pairs); i += 2 {
		if isEscLabel(pairs[i]) {
			write(pairs[i], pairs[i+1])
		}
	}
	for i := 0; i+1 < len(pairs); i += 2 {
		if !isEscLabel(pairs[i]) {
			write(pairs[i], pairs[i+1])
		}
	}
	return b.String()
}

// isEscLabel reports whether a footer key label is the Esc key, alone or
// shared with the quit key ("Esc/q").
func isEscLabel(label string) bool {
	return label == "Esc" || strings.HasPrefix(label, "Esc/")
}

// dangerMark prefixes a key label that names a destructive action (delete,
// remove, stop). It is a NUL byte, which no key label or tview tag contains.
const dangerMark = "\x00"

// dangerKey marks a key label as destructive, so keyTag (and every footer and
// help line built on it) draws it red — the same red the inspect actions menu
// gives "remove service". It wraps the LIVE label, so a remapped delete key is
// the one that turns red.
func dangerKey(label string) string { return dangerMark + label }

// keyTag renders one key label for a footer: yellow, or red when the label
// came through dangerKey.
func keyTag(label string) string {
	if l, ok := strings.CutPrefix(label, dangerMark); ok {
		return "[red]" + l + "[white]"
	}
	return "[yellow]" + label + "[white]"
}

// info shows a generic info modal. Every message is also logged (with context)
// so the log viewer / file has a record of what the operator was shown.
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
	activeCtx, clusterText := u.activeCtx, u.clusterText
	forwards := u.forwards
	status := u.status
	parts := []string{fmt.Sprintf("[aqua]ctx[white] %s", ctxStatusLabel(activeCtx))}
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
	if n, ok := u.activeTab().(tabStatusNoter); ok {
		if note := n.statusNote(); note != "" {
			parts = append(parts, note)
		}
	}
	status.SetText(" " + strings.Join(parts, "  ·  "))
}

// statusRightText is the right end of the status line: the key that shows the
// context list while it is hidden, then the client version.
func statusRightText(version string, ctxKey rune, sidebarHidden bool, contexts int) string {
	var b strings.Builder
	if sidebarHidden && contexts > 0 {
		fmt.Fprintf(&b, "[yellow]%s[white] contexts  ·  ", keyLabel(ctxKey))
	}
	b.WriteString("[gray]" + tview.Escape(version) + "[white] ")
	return b.String()
}

// updateStatusRight redraws the right end of the status line and sizes it to
// its text, so the status on the left keeps every column it does not need.
func (u *ui) updateStatusRight() {
	if u.statusRight == nil || u.statusRow == nil {
		return
	}
	txt := statusRightText(u.binaryVersion(), u.km.CtxFocus, u.sidebarHidden, len(u.ctxs))
	u.statusRight.SetText(txt)
	u.statusRow.ResizeItem(u.statusRight, tview.TaggedStringWidth(txt), 0)
}

// binaryVersion is this client's release as `swarmexec --version` reports it.
func (u *ui) binaryVersion() string {
	if u.g == nil || u.g.version.Binary == "" {
		return "dev"
	}
	return u.g.version.Binary
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
// tree render / remarkUsage pass walks the whole tree on the UI goroutine
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
	if name == "contexts" {
		// The sidebar's footer, shown while it holds the keyboard. Esc is first
		// after the movement keys because it is the way back out — the sidebar
		// is the one focusable thing here that is not a tab, so "how do I get
		// back" is the question it has to answer.
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Esc[white] back  [yellow]Enter/%s[white] switch  [yellow]i[white] details  [yellow]%s[white] new  %s delete",
			kl(km.CtxUse), kl(km.CtxNew), keyTag(dangerKey(kl(km.CtxDelete))))
	}
	if t := u.tab(name); t != nil {
		return head + t.footer()
	}
	return head
}

// showHelp opens a scrollable overlay listing every keybinding — the complete
// reference the single-row footer cannot hold. It is generated from the live
// keymap, so remapped keys show correctly. Bound to "?" on every tab.
func (u *ui) showHelp() {
	km, app := u.km, u.app
	kl := keyLabel
	h := &helpWriter{}

	h.b.WriteString("[aqua]Global[-]\n")
	h.line("?", "this help")
	h.line(kl(km.Quit), "quit")
	h.line(kl(km.Refresh), "refresh the current tab + cluster")
	h.line(kl(km.Copy), "copy the current list to the clipboard")
	h.line(kl(km.ToggleMouse), "toggle mouse on/off")
	h.line("Tab", "next tab")
	h.line(fmt.Sprintf("1–%d", len(uiTabList)), "jump to a tab by number")
	h.line(kl(km.CtxFocus), "focus the contexts sidebar · Esc leaves")
	h.line("`", "toggle the client log view (w wraps long lines)")
	h.line("Esc", "leave the current overlay / dialog (back, cancel or close — always the first key in its footer)")

	// Every tab's own section, in tab order, from the registry.
	for _, t := range uiTabList {
		u.tab(t.key).helpSection(h)
	}

	h.sec("Contexts (sidebar, not a tab)")
	h.line(kl(km.CtxFocus), "focus the sidebar from any tab; again or Esc to leave")
	h.line("Enter / "+kl(km.CtxUse), "switch to that cluster — keeps your place, filters and forwards")
	h.line("i", "context details (endpoint, jump hosts)")
	h.line(kl(km.CtxNew), "new")
	h.danger(kl(km.CtxDelete), "delete")
	h.line("▶ · ✗", "active · connected (instant) · last attempt refused")

	h.sec("Log view (`)")
	h.line("f", "follow on/off")
	h.line("F", "cycle format")
	h.line("a", "auto-detect format")
	h.line("l", "cycle min level")
	h.line("/", "filter message")
	h.line("↑/↓", "scroll")
	b := &h.b

	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	tv.SetText(strings.TrimLeft(b.String(), "\n"))
	tv.SetBorder(true).SetTitle(" keybindings ")
	prev := app.GetFocus()
	ov := u.overlayFor(pageHelp, prev, footerKeys("j/k", "scroll", "Esc", "back"))
	closeHelp := ov.Close
	tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch {
		case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == u.km.Quit || ev.Rune() == '?')):
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
	t := u.tab(name)
	if t == nil {
		return
	}
	u.active = name
	u.content.SwitchToPage(name)
	u.curHelp = u.helpFor(name)
	// Switching tab takes the keyboard back from the sidebar, so the footer it
	// borrowed goes back too rather than being restored over the new tab's.
	u.sidebarReturn = ""
	u.sidebarHasFocus = false
	u.sidebar.SetBorderColor(palette.border)
	u.setFooter(u.curHelp)
	u.renderTabBar(name)
	u.updateStatus() // the selection count shows only on the volumes tab
	u.app.SetFocus(t.widget())
	t.enter()
}

// startSearch opens the shared "/" bar over the tab registered as mode, which
// must be a tabSearcher.
func (u *ui) startSearch(mode string) {
	root, search, app := u.root, u.search, u.app
	s, ok := u.tab(mode).(tabSearcher)
	if !ok {
		return
	}
	u.searchMode = mode
	root.ResizeItem(search, 1, 0)
	u.savedHelp = u.footerBase
	u.setFooter(" [yellow]type[white] to filter   [yellow]Enter[white] keep filter & exit   [yellow]Esc[white] clear & exit")
	search.SetPlaceholder(s.searchPlaceholder())
	search.SetText(s.searchText())
	app.SetFocus(search)
}

// searchTab is the tab the "/" bar currently filters, nil when none.
func (u *ui) searchTab() tabSearcher {
	s, _ := u.tab(u.searchMode).(tabSearcher)
	return s
}

// refreshCluster probes the swarm in the background and updates the footer
// summary. The agent probe (a Version RPC per node) can be slow, so it runs
// off the UI goroutine and pushes the result back via QueueUpdateDraw.
func (u *ui) refreshCluster() {
	r, ac, f := u.r, u.agents, u.f
	type census struct{ nodes, agents int }
	async(u, "ui.refreshCluster", func(ctx context.Context) (census, error) {
		nodes, err := r.Nodes(ctx)
		if err != nil {
			return census{}, err
		}
		agents := 0
		for _, h := range ac.Versions(ctx, nodes, f.connectTimeout) {
			if h.Err == nil {
				agents++
			}
		}
		return census{len(nodes), agents}, nil
	}, func(c census, err error) {
		if err != nil {
			u.clusterText = "[red]cluster: unreachable[white]"
		} else {
			u.clusterText = clusterSummary(c.nodes, c.agents)
		}
		u.updateStatus()
	})
}

// clusterSummary is the status line's cluster part: "3 nodes · 3/3 agents".
func clusterSummary(nodes, agents int) string {
	noun := "nodes"
	if nodes == 1 {
		noun = "node"
	}
	return fmt.Sprintf("[aqua]%d[white] %s · [aqua]%d[white]/%d agents", nodes, noun, agents, nodes)
}

// refreshForwardViews repaints everything a forward's state feeds: the
// footer counter, the forwards table, and the tree annotations. Must run on
// the UI goroutine.
func (u *ui) refreshForwardViews() {
	u.updateStatus()
	u.fwdTab().render()
	u.treeTab().render()
}

// yankCurrent copies the active tab's list to the system clipboard via the
// terminal (OSC52), so it also works over ssh when the terminal supports it.
func (u *ui) yankCurrent() {
	if u.screen == nil {
		return
	}
	var b strings.Builder
	// The context list is no longer a tab, so "what is on screen" is no longer
	// the same question as "which tab is this": when the sidebar holds the
	// keyboard, that is what the operator means by copy.
	if u.sidebarFocused() {
		u.yankContexts(&b)
	} else if t := u.activeTab(); t != nil {
		t.yank(&b)
	}
	u.screen.SetClipboard([]byte(b.String()))
	u.flash(" [green]✓ copied to clipboard[white]")
}

// yankContexts writes the sidebar's context list as the clipboard receives it.
func (u *ui) yankContexts(b *strings.Builder) {
	fmt.Fprintln(b, "CONTEXT\tDOCKER HOST\tACTIVE")
	for _, c := range u.ctxs {
		mark := ""
		if c.Name == u.activeCtx {
			mark = "*"
		}
		fmt.Fprintf(b, "%s\t%s\t%s\n", c.Name, orDash(c.Host), mark)
	}
}

// tabKeys is the keys every tab shares, the last stop of each tab's own
// input capture. The order is the one the keys were always matched in (help
// and log viewer, Tab, tab numbers, the keymap's keys, j/k): a keys.yaml
// binding over a key earlier in it never fires.
func (u *ui) tabKeys(ev *tcell.EventKey) *tcell.EventKey {
	if ev.Key() == tcell.KeyTab {
		u.nextTab()
		return nil
	}
	if ev.Key() != tcell.KeyRune {
		return ev
	}
	r := ev.Rune()
	switch r {
	case '?':
		u.showHelp()
		return nil
	case '`':
		u.toggleLogView()
		return nil
	}
	// Tab-number keys, derived from the tab list rather than hardcoded — the tab
	// bar labels its tabs from the same positions, so the two cannot drift.
	if r >= '1' && r <= '9' && int(r-'1') < len(uiTabList) {
		u.setTab(uiTabList[r-'1'].key)
		return nil
	}
	if u.globalKey(r) {
		return nil
	}
	switch r {
	case 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	}
	return ev
}

// nextTab is Tab: the next tab in the tab bar, wrapping to the first.
func (u *ui) nextTab() {
	for i, t := range uiTabList {
		if t.key == u.active {
			u.setTab(uiTabList[(i+1)%len(uiTabList)].key)
			return
		}
	}
}

// globalKey runs the keymap action bound to r, if any, and says whether
// there was one.
func (u *ui) globalKey(r rune) bool {
	km := u.km
	switch r {
	case km.Quit:
		u.app.Stop()
	case km.Refresh:
		if t := u.activeTab(); t != nil {
			t.refresh()
		}
		// The context list comes from docker's local store, so it costs
		// nothing and is refreshed whatever tab you are on — it is always
		// on screen now.
		u.loadContexts()
		u.refreshCluster()
	case km.Copy:
		u.yankCurrent()
	case km.ToggleMouse:
		u.toggleMouse()
	case km.CtxFocus:
		u.focusSidebar()
	default:
		return false
	}
	return true
}

// closeLogView closes the log viewer. The viewer (closeLogView / openLogView /
// toggleLogView) is an overlay showing the in-memory log ring, refreshed live
// while open; it is opened/closed with the backtick key from any tab. Its state
// (logViewStop / logViewPrev / logViewOverlay) lives on u.
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
		footerKeys("`", "toggle/close", "Esc/"+keyLabel(u.km.Quit), "close", "↑/↓", "scroll", "w", "wrap"))
	// Wrapped by default: the end of an error line — the gRPC cause, the
	// reason the viewer was opened — is exactly the part a cut line loses.
	// w turns it off for the rare wide line worth reading unbroken.
	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
	tv.SetBorder(true).SetTitle(" logs — newest at bottom ")
	wrap := true
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
		if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == '`' || ev.Rune() == u.km.Quit)) {
			u.closeLogView()
			return nil
		}
		if ev.Key() == tcell.KeyRune && ev.Rune() == 'w' {
			wrap = !wrap
			tv.SetWrap(wrap)
			return nil
		}
		return ev
	})
	stop := make(chan struct{})
	u.logViewStop = stop
	// centeredGrow, not tv on its own: a full-screen page covered the footer,
	// so the keys it names were never on screen.
	u.logViewOverlay.show(centeredGrow(tv, 100, 20, 0.92), tv)
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
