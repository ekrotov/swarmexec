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
			// Loop so activating a context in the Contexts tab restarts the UI
			// cleanly against the chosen cluster; "" means a normal quit.
			ctxOverride := g.dockerContext
			for {
				next, err := runUI(cmd, g, f, args, ctxOverride)
				if err != nil {
					return err
				}
				if next == "" {
					return nil
				}
				ctxOverride = next
			}
		},
	}
	cmd.Flags().DurationVar(&f.connectTimeout, "connect-timeout", 10*time.Second, "timeout for connecting to an agent")
	return cmd
}

// runUI runs one session of the UI against ctxOverride's docker context. It
// returns the name of a context to switch to (the operator activated one in the
// Contexts tab) so the caller can restart cleanly against it, or "" on a normal
// quit.
//
// A context switch restarts the UI rather than re-pointing it live, for two
// reasons:
//   - Data races: the docker client and resolver are read by many background
//     goroutines (per-node volume/agent probes, the cluster-summary probe,
//     container loads). Reassigning them under those in-flight reads would be a
//     data race, so a live swap would need locking around every access.
//   - Cluster-bound state: active port-forwards, open exec/logs streams, the
//     async cluster summary and cached candidates all belong to the old cluster.
//     A fresh run tears them down (the deferred cleanups fire, old goroutines
//     drain) and rebuilds everything for the new cluster, so there is no
//     half-switched state — e.g. a forward left pointing at an old-cluster node.
//
// treeRefreshInterval is how often the UI re-polls the swarm so the container
// tree reflects background changes (rolling updates, restarts, scaling).
const treeRefreshInterval = 10 * time.Second

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
)

func runUI(cmd *cobra.Command, g *globalFlags, f *uiFlags, args []string, ctxOverride string) (string, error) {
	cfg, err := g.resolveConfig(cmd)
	if err != nil {
		return "", &cliError{code: usageExitCode, err: err}
	}
	if err := cfg.Validate(); err != nil {
		return "", &cliError{code: usageExitCode, err: err}
	}
	if !cterm.IsTerminal(os.Stdout.Fd()) || !cterm.IsTerminal(os.Stdin.Fd()) {
		return "", &cliError{code: usageExitCode, err: fmt.Errorf("ui needs an interactive terminal (use plain `ps`/`volume ls` when piping)")}
	}

	// Install the global theme before any primitive is constructed: tview reads
	// its palette at construction time and its border glyphs at draw time.
	applyTheme(cfg.UI.Dim)

	ctx := cmdContext(cmd)
	// Per-run context so background goroutines (the auto-refresh ticker, the
	// responsiveness watchdog, the log-viewer refresher, open streams, forwards)
	// stop when this run returns — e.g. on a Contexts-tab cluster switch, where
	// the caller restarts runUI with a fresh context.
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var service string
	if len(args) == 1 {
		service = args[0]
	}

	// switchTo is set when the operator activates a context in the Contexts tab;
	// the UI then stops and the caller restarts against it.
	var switchTo string

	dcli, err := newDockerClient(ctxOverride)
	if err != nil {
		return "", &cliError{code: session.TransportFailure, err: err}
	}
	r := resolve.New(dcli, addrModeOf(cfg))

	// Configurable shortcut keys (keys.yaml). Never fails: invalid/conflicting
	// bindings fall back to defaults with a warning shown once on startup.
	km, keyWarnings := loadKeybinds("")

	app := tview.NewApplication()
	pages := tview.NewPages()   // overlays: menus, terminal, logs, volume nodes
	content := tview.NewPages() // the two tabs

	selStyle := tcell.StyleDefault.Background(tcell.ColorTeal).Foreground(tcell.ColorWhite)

	u := &ui{
		app: app, pages: pages, content: content,
		dcli: dcli, r: r, cfg: cfg, km: km,
		f: f, g: g, ctx: ctx, cancelRun: cancelRun,
		ctxOverride: ctxOverride, service: service, switchTo: switchTo,
		selStyle: selStyle,
	}
	return u.run(keyWarnings)
}

// run drives one UI session over the infrastructure runUI prepared on u. The
// per-tab closures still live here (they will move to their own files in later
// stages); the shared infra closures are now methods on *ui. The aliases below
// keep those closure bodies referring to app/pages/… unchanged.
func (u *ui) run(keyWarnings []string) (string, error) {
	app, pages, content := u.app, u.pages, u.content
	km := u.km
	g, ctx := u.g, u.ctx
	ctxOverride, service := u.ctxOverride, u.service
	selStyle := u.selStyle

	// forwards is the UI's only persistent background resource: a port forward
	// outlives the overlay that started it, unlike every stream here.
	forwards := newForwardRegistry()
	u.forwards = forwards
	defer forwards.stopAll()

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
			for _, sn := range croot.GetChildren() {
				u.markService(sn)
			}
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
	// activeCtx is this session's effective docker context (what the UI is
	// connected to): the context the session was started with, else
	// $DOCKER_CONTEXT, else the stored current, else "default". Shown in the
	// footer and marked here.
	u.activeCtx = firstNonEmpty(ctxOverride, os.Getenv("DOCKER_CONTEXT"))
	if u.activeCtx == "" {
		u.activeCtx = dockerctx.Current()
	}
	if u.activeCtx == "" {
		u.activeCtx = "default"
	}
	cxtable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
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

	// ---------------------------------------------------------------- tabs/chrome
	content.AddPage("containers", ctree, true, true)
	content.AddPage("volumes", vtable, true, false)
	content.AddPage("forwards", ftable, true, false)
	content.AddPage("networks", nettable, true, false)
	content.AddPage("secrets", sectable, true, false)
	content.AddPage("contexts", cxtable, true, false)
	content.AddPage("nodes", notable, true, false)

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
	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tabBar, 2, 0, false). // labels row + active-tab underline indicator
		AddItem(content, 0, 1, true).
		AddItem(search, 0, 0, false).
		AddItem(footer, 2, 0, false)
	pages.AddPage(pageMain, root, true, true)

	// Promote the shared widgets onto u so the infra methods (updateStatus,
	// setTab, refreshForwardViews, …) can reach them. All exist by now.
	u.ctree, u.croot = ctree, croot
	u.vtable, u.ftable, u.nettable = vtable, ftable, nettable
	u.sectable, u.cxtable, u.notable = sectable, cxtable, notable
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
		help.SetText(u.savedHelp) // restore the tab help
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
			case km.Fold:
				// Collapse. tview's TreeView has no fold key — Left/Right only
				// move the cursor — so fold explicitly. On a container leaf,
				// step out to its service (press h again to fold it).
				if n := ctree.GetCurrentNode(); n != nil {
					if isServiceNode(n) {
						n.SetExpanded(false)
						u.markService(n)
					} else if p := serviceParent(croot, n); p != nil {
						ctree.SetCurrentNode(p)
					}
				}
				return nil
			case km.Unfold:
				// Expand the service under the cursor; if it is already open,
				// descend to its first container.
				if n := ctree.GetCurrentNode(); n != nil && isServiceNode(n) {
					if n.IsExpanded() && len(n.GetChildren()) > 0 {
						ctree.SetCurrentNode(n.GetChildren()[0])
					} else {
						n.SetExpanded(true)
						u.markService(n)
					}
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
	// showContextDetail is the read-only "i" view for a context, so inspect works
	// on the contexts tab like every other tab. Enter/u still activate (switch).
	// On the contexts table: Enter/u activate (switch), i details, n creates, d removes.
	cxtable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
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
	// On the nodes table: edit the selected node's labels; Enter/i opens details.
	notable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
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

	u.loadContainersSync() // startup: before app.Run, so fetch+apply inline
	u.setTab("containers")
	u.refreshCluster()

	// Poll the swarm so the container tree notices background changes (rolling
	// updates, restarts, scaling) on its own. The client learns topology from the
	// manager; the agents are node-local and cannot push such events.
	go func() {
		t := time.NewTicker(treeRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				u.autoRefreshContainers()
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
		return "", &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: init screen: %w", serr)}
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
		return "", &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: %w", err)}
	}
	return u.switchTo, nil
}

// svcRef marks a service (group) node and carries its name. It is deliberately
// NOT a resolve.Candidate, so isServiceNode (and every other
// GetReference().(resolve.Candidate) check) still treats it as a group node; it
// only lets a re-render restore the cursor and expansion state by service name.
type svcRef struct{ name string }

// isServiceNode reports whether a tree node is a service (group) node rather
// than a container leaf (containers carry a resolve.Candidate reference).
func isServiceNode(n *tview.TreeNode) bool {
	_, ok := n.GetReference().(resolve.Candidate)
	return !ok
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
		return tcell.ColorGray
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
	case "down", "disconnected", "unknown":
		return tcell.ColorRed
	default:
		return tcell.ColorYellow
	}
}

// serviceParent returns the service node that owns leaf, or nil. tview.TreeNode
// exposes no parent pointer, so we scan the (shallow, two-level) tree.
func serviceParent(root, leaf *tview.TreeNode) *tview.TreeNode {
	for _, svc := range root.GetChildren() {
		for _, c := range svc.GetChildren() {
			if c == leaf {
				return svc
			}
		}
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

// svcColumns holds the padding widths that align the service rows in the
// containers tree, computed once per render across all services.
type svcColumns struct {
	name, mode, repl, image int
}

// serviceRow renders a service group node like a docker service ls line —
// "name  mode  running/desired  image  ports" — padded to the shared column
// widths. Image and ports are appended only when present, and trailing padding
// is trimmed so a selected row's highlight does not run past the text.
func serviceRow(s resolve.Service, c svcColumns, imageSuffix string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-*s  %-*s",
		c.name, orDash(s.Name), c.mode, orDash(s.Mode), c.repl, fmt.Sprintf("%d/%d", s.Running, s.Desired))
	if c.image > 0 {
		// The resolved-version / ↑ annotation sits inside the image cell so it
		// reads right next to the image URI, not far off in the ports column.
		fmt.Fprintf(&b, "  %-*s", c.image, orDash(s.Image)+imageSuffix)
	}
	if s.Ports != "" {
		fmt.Fprintf(&b, "  %s", s.Ports)
	}
	// The update badge is appended last (after the padded columns are trimmed) so
	// its colour tags never disturb the column alignment.
	return strings.TrimRight(b.String(), " ") + updateBadge(s.UpdateState)
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
	case "paused":
		return "⏸ update paused", "orange", true
	case "rollback_started":
		return "↺ rolling back", "orange", true
	case "rollback_paused":
		return "⏸ rollback paused", "red", true
	default: // completed, rollback_completed, or none
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
// (dynamic-colour tags, as the footer is a TextView). Overlays feed it to
// pushOverlayHelp so the single bottom footer describes the active view's keys.
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
	app, pages := u.app, u.pages
	clientlog.L().Info("ui notice", "msg", msg)
	m := tview.NewModal().SetText(msg).AddButtons([]string{"OK"}).
		SetDoneFunc(func(int, string) { pages.RemovePage(pageInfo) })
	pages.AddPage(pageInfo, newScrim(m), true, true)
	app.SetFocus(m)
}

// confirm shows a two-button confirmation modal (confirmLabel + "Cancel") and
// owns the modal, its page, and focus restoration — the two-button sibling of
// info. onConfirm runs only when the operator picks the confirm button; on
// cancel, focus returns to back. On confirm, onConfirm decides what happens
// next (open a progress overlay, start async work, restore focus itself, …),
// so it must handle its own focus. Only one confirm is ever open at a time, so
// a single shared page name is safe.
func (u *ui) confirm(msg, confirmLabel string, back tview.Primitive, onConfirm func()) {
	app, pages := u.app, u.pages
	m := tview.NewModal().
		SetText(msg).
		AddButtons([]string{confirmLabel, "Cancel"}).
		SetDoneFunc(func(_ int, label string) {
			pages.RemovePage(pageConfirm)
			if label != confirmLabel {
				app.SetFocus(back)
				return
			}
			onConfirm()
		})
	pages.AddPage(pageConfirm, newScrim(m), true, true)
	app.SetFocus(m)
}

// flash briefly replaces the footer with a status message, then restores the
// footer that was there BEFORE — which may be a tab footer or an overlay's own
// footer (pushOverlayHelp), so it must snapshot, not assume curHelp. The
// restore is guarded: if anything else changed the footer meanwhile (a tab
// switch, an opened overlay, a newer flash), that owner keeps it.
func (u *ui) flash(msg string) {
	help, app := u.help, u.app
	prev := help.GetText(false)
	help.SetText(msg)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		app.QueueUpdateDraw(func() {
			if help.GetText(false) == msg {
				help.SetText(prev)
			}
		})
	}()
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

// pushOverlayHelp points the single bottom footer at an overlay's keys. It
// saves the current footer text and returns a setter (to update while open)
// and a restore (to call on close). Because each call captures the then-
// current text, nested overlays restore correctly. It also tracks how many
// overlays are open (overlayDepth) so the background tree refresh can pause —
// otherwise its periodic renderContainers on the UI goroutine competes with
// keystrokes in an overlay and makes them feel laggy.
func (u *ui) pushOverlayHelp(markup string) (func(string), func()) {
	help := u.help
	prev := help.GetText(false)
	help.SetText(markup)
	u.overlayDepth.Add(1)
	var once sync.Once
	restore := func() {
		once.Do(func() {
			u.overlayDepth.Add(-1)
			help.SetText(prev)
		})
	}
	return func(m string) { help.SetText(m) }, restore
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
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter[white] expand/menu  [yellow]%s[white] logs  [yellow]%s/%s[white] fold  [yellow]%s[white] inspect  [yellow]%s[white] search  [yellow]%s[white] forward",
			kl(km.Logs), kl(km.Fold), kl(km.Unfold), kl(km.ContainerInspect), kl(km.Search), kl(km.Forward))
	case "volumes":
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]%s[white] search  [yellow]%s[white] new  [yellow]%s[white] select  [yellow]%s[white] all  [yellow]%s[white] attach  [yellow]%s[white] delete  [yellow]%s[white] prune  [yellow]Enter[white] nodes  [yellow]%s[white] used by  [yellow]%s[white] sort",
			kl(km.Search), kl(km.VolNew), kl(km.VolSelect), kl(km.VolSelectAll), kl(km.VolAttach), kl(km.VolDelete), kl(km.VolPrune), kl(km.VolUsedBy), kl(km.VolSort))
	case "networks":
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/%s[white] attached  [yellow]%s[white] new", kl(km.NetAttached), kl(km.NetNew))
	case "secrets":
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] new  [yellow]%s[white] delete", kl(km.SecNew), kl(km.SecDelete))
	case "contexts":
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/%s[white] use  [yellow]i[white] details  [yellow]%s[white] new  [yellow]%s[white] delete",
			kl(km.CtxUse), kl(km.CtxNew), kl(km.CtxDelete))
	case "nodes":
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] edit labels", kl(km.NodeLabels))
	default:
		return head + fmt.Sprintf("[yellow]j/k[white] up/down  [yellow]Enter/i[white] details  [yellow]%s[white] stop  [yellow]%s[white] copy url",
			kl(km.FwdStop), kl(km.FwdCopyURL))
	}
}

// showHelp opens a scrollable overlay listing every keybinding — the complete
// reference the single-row footer cannot hold. It is generated from the live
// keymap, so remapped keys show correctly. Bound to "?" on every tab.
func (u *ui) showHelp() {
	km, app, pages := u.km, u.app, u.pages
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
	line("`", "toggle the client log view")
	line("Esc", "close the current overlay / dialog")

	sec("Containers")
	line("Enter", "expand a service · open a container's menu")
	line(kl(km.Logs), "logs (service or container)")
	line(kl(km.ContainerInspect), "inspect: service/task detail + editors")
	line(kl(km.Fold)+"/"+kl(km.Unfold), "fold / unfold")
	line(kl(km.Forward), "port-forward the task under the cursor")
	line(kl(km.Search), "search services / containers / nodes")

	sec("Service inspect (" + kl(km.ContainerInspect) + ")")
	line("a", "actions menu (all edits below, no Shift needed)")
	line("t", "toggle raw JSON / table")
	line("d / D", "diff spec · why (placement)")
	line("s / f", "scale · force-update")
	line("u", "update to a newer image")
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

	sec("Contexts")
	line("Enter / "+kl(km.CtxUse), "use (switch cluster)")
	line("i", "context details")
	line(kl(km.CtxNew)+"/"+kl(km.CtxDelete), "new / delete")

	sec("Nodes")
	line("Enter / i", "details")
	line(kl(km.NodeLabels), "edit labels")

	sec("Forwards")
	line("Enter / i", "details")
	line(kl(km.FwdStop), "stop")
	line(kl(km.FwdCopyURL), "copy URL")

	sec("Log view (`)")
	line("f", "follow on/off")
	line("F", "cycle format")
	line("l", "cycle min level")
	line("/", "filter message")
	line("↑/↓", "scroll")

	tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true)
	tv.SetText(strings.TrimLeft(b.String(), "\n"))
	tv.SetBorder(true).SetTitle(" keybindings ")
	prev := app.GetFocus()
	_, restore := u.pushOverlayHelp(footerKeys("j/k", "scroll", "Esc", "close"))
	closeHelp := func() { restore(); pages.RemovePage(pageHelp); app.SetFocus(prev) }
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
	pages.AddPage(pageHelp, centered(tv, 60, 24), true, true)
	app.SetFocus(tv)
}

func (u *ui) renderTabBar(active string) {
	u.tabBar.setActive(active)
}

func (u *ui) setTab(name string) {
	content, help, app := u.content, u.help, u.app
	ctree, vtable, ftable := u.ctree, u.vtable, u.ftable
	nettable, sectable := u.nettable, u.sectable
	cxtable, notable := u.cxtable, u.notable
	u.active = name
	content.SwitchToPage(name)
	u.curHelp = u.helpFor(name)
	help.SetText(u.curHelp)
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
	case "contexts":
		app.SetFocus(cxtable)
		u.loadContexts()
	case "nodes":
		app.SetFocus(notable)
		u.loadNodes()
	}
}

func (u *ui) startSearch(mode string) {
	root, search, help, app := u.root, u.search, u.help, u.app
	u.searchMode = mode
	root.ResizeItem(search, 1, 0)
	u.savedHelp = help.GetText(false)
	help.SetText(" [yellow]type[white] to filter   [yellow]Enter[white] keep filter & exit   [yellow]Esc[white] clear & exit")
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
	r, ctx, app := u.r, u.ctx, u.app
	cfg, f := u.cfg, u.f
	go func() {
		nodes, err := r.Nodes(ctx)
		if err != nil {
			app.QueueUpdateDraw(func() {
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
		app.QueueUpdateDraw(func() {
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
		for _, svc := range croot.GetChildren() {
			fmt.Fprintln(&b, trimFoldMarker(svc.GetText()))
			for _, c := range svc.GetChildren() {
				fmt.Fprintf(&b, "  %s\n", c.GetText())
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
	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case '1':
			u.setTab("containers")
			return nil
		case '2':
			u.setTab("volumes")
			return nil
		case '3':
			u.setTab("forwards")
			return nil
		case '4':
			u.setTab("networks")
			return nil
		case '5':
			u.setTab("secrets")
			return nil
		case '6':
			u.setTab("contexts")
			return nil
		case '7':
			u.setTab("nodes")
			return nil
		case km.Quit:
			app.Stop()
			return nil
		case km.Refresh:
			switch active {
			case "containers":
				u.loadContainers()
			case "volumes":
				u.loadVolumes()
			case "networks":
				u.loadNetworks()
			case "secrets":
				u.loadSecrets()
			case "contexts":
				u.loadContexts()
			case "nodes":
				u.loadNodes()
			default:
				u.renderForwards()
			}
			u.refreshCluster()
			return nil
		case km.Copy:
			u.yankCurrent()
			return nil
		case km.ToggleMouse:
			u.toggleMouse()
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
// logViewPrev / logViewRestore) lives on u.
func (u *ui) closeLogView() {
	pages, app := u.pages, u.app
	if u.logViewStop != nil {
		close(u.logViewStop)
		u.logViewStop = nil
	}
	if u.logViewRestore != nil {
		u.logViewRestore()
		u.logViewRestore = nil
	}
	pages.RemovePage(pageLogView)
	if u.logViewPrev != nil {
		app.SetFocus(u.logViewPrev)
	}
}

func (u *ui) openLogView() {
	app, pages, ctx := u.app, u.pages, u.ctx
	u.logViewPrev = app.GetFocus()
	_, u.logViewRestore = u.pushOverlayHelp(footerKeys("`", "toggle/close", "Esc/q", "close", "↑/↓", "scroll"))
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
	pages.AddPage(pageLogView, tv, true, true)
	app.SetFocus(tv)
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
