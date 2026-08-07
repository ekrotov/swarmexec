// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/spf13/cobra"

	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/logfmt"
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

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
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

	// generic info modal. Every message is also logged (with context) so the log
	// viewer / file has a record of what the operator was shown.
	info := func(msg string) {
		clientlog.L().Info("ui notice", "msg", msg)
		m := tview.NewModal().SetText(msg).AddButtons([]string{"OK"}).
			SetDoneFunc(func(int, string) { pages.RemovePage("info") })
		pages.AddPage("info", m, true, true)
		app.SetFocus(m)
	}

	// forwards is the UI's only persistent background resource: a port forward
	// outlives the overlay that started it, unlike every stream here.
	forwards := newForwardRegistry()
	defer forwards.stopAll()

	// Set below, once the widgets they touch exist. They are declared up here
	// because starting a forward has to refresh the tree, the forwards table and
	// the footer, and those are all built further down.
	var (
		refreshForwardViews func()
		flash               func(string)
		updateStatus        func()       // recomposes the footer status line
		toggleMouse         func()       // flips tview's mouse capture (defined below)
		mouseEnabled        bool         // mirrors app.EnableMouse; toggled by 'm'
		overlayDepth        atomic.Int32 // number of open overlays (pauses the tree auto-refresh)
		autoRefreshBusy     atomic.Bool  // guards against overlapping tree refreshes
		// pushOverlayHelp overrides the single bottom footer with an overlay's own
		// keys, returning a setter (to update it while the overlay is open) and a
		// restore func (call on close). Nesting-safe: each call saves the current
		// footer text. Assigned once the footer widget exists.
		pushOverlayHelp func(string) (set func(string), restore func())
	)

	// ---------------------------------------------------------------- containers
	// A tree: services are parent nodes, their containers are children.
	ctree := tview.NewTreeView()
	croot := tview.NewTreeNode("")
	ctree.SetRoot(croot).SetTopLevel(1) // hide the synthetic root; services are top-level
	// filter holds the active "/" search query; empty means show everything. A
	// candidate matches when the query is a substring of its service, container
	// id or node (case-insensitive). It starts from the optional `ui [service]`
	// argument so `swarmexec ui web` opens pre-narrowed to matching services.
	filter := service
	matchesFilter := func(c resolve.Candidate) bool {
		if filter == "" {
			return true
		}
		q := strings.ToLower(filter)
		return strings.Contains(strings.ToLower(c.Service), q) ||
			strings.Contains(strings.ToLower(c.ContainerID), q) ||
			strings.Contains(strings.ToLower(c.NodeName), q)
	}
	// lastCands / lastSvcs cache the most recent fetch so the "/" filter can
	// re-render locally without hitting the docker API on every keystroke (a
	// remote call over the ssh tunnel — doing it per keystroke makes typing
	// crawl). lastSvcs drives the tree so every service shows, even one with no
	// running task; lastCands supplies the container leaves.
	var (
		lastCands []resolve.Candidate
		lastSvcs  []resolve.Service
	)
	// svcByName / svcCols cache the current services and the column widths so the
	// fold marker (▸/▾) can be rebuilt on a fold — and the docker service ls-style
	// row (mode, replicas, image, ports) stays aligned — without re-rendering the
	// whole tree. A service with no containers gets no marker (nothing to expand),
	// just padding so the rows still line up.
	svcByName := map[string]resolve.Service{}
	svcCols := svcColumns{}
	markService := func(n *tview.TreeNode) {
		ref, ok := n.GetReference().(svcRef)
		if !ok {
			return
		}
		row := serviceRow(svcByName[ref.name], svcCols)
		switch {
		case len(n.GetChildren()) == 0:
			n.SetText("  " + row)
		case n.IsExpanded():
			n.SetText("▾ " + row)
		default:
			n.SetText("▸ " + row)
		}
	}

	renderContainers := func() {
		// Remember the cursor (a leaf by container id, else a service by name)
		// and which services were expanded, so a refresh keeps both.
		prevID, prevSvc := "", ""
		if n := ctree.GetCurrentNode(); n != nil {
			if ref, ok := n.GetReference().(resolve.Candidate); ok {
				prevID = ref.ContainerID
			} else if ref, ok := n.GetReference().(svcRef); ok {
				prevSvc = ref.name
			}
		}
		wasExpanded := map[string]bool{}
		for _, sn := range croot.GetChildren() {
			if ref, ok := sn.GetReference().(svcRef); ok {
				wasExpanded[ref.name] = sn.IsExpanded()
			}
		}
		croot.ClearChildren()

		// Group running containers by service for the leaves.
		byService := map[string][]resolve.Candidate{}
		for _, c := range lastCands {
			byService[c.Service] = append(byService[c.Service], c)
		}
		// Pre-compute column widths so every container row lines up, regardless
		// of node-name length or whether a task carries a slot.
		slotStr := func(c resolve.Candidate) string {
			if c.Slot > 0 {
				return fmt.Sprintf("slot %d", c.Slot)
			}
			return ""
		}
		nodeW, slotW := 0, 0
		for _, c := range lastCands {
			if w := len(orDash(c.NodeName)); w > nodeW {
				nodeW = w
			}
			if w := len(slotStr(c)); w > slotW {
				slotW = w
			}
		}

		// Column widths for the docker service ls-style service rows, computed
		// over every service so the alignment stays stable while filtering.
		svcCols = svcColumns{}
		svcByName = make(map[string]resolve.Service, len(lastSvcs))
		for _, s := range lastSvcs {
			svcByName[s.Name] = s
			if w := len(orDash(s.Name)); w > svcCols.name {
				svcCols.name = w
			}
			if w := len(orDash(s.Mode)); w > svcCols.mode {
				svcCols.mode = w
			}
			if w := len(fmt.Sprintf("%d/%d", s.Running, s.Desired)); w > svcCols.repl {
				svcCols.repl = w
			}
			if w := len(s.Image); w > svcCols.image {
				svcCols.image = w
			}
		}

		q := strings.ToLower(strings.TrimSpace(filter))
		var firstSvc, targetSvc, targetLeaf *tview.TreeNode
		for _, s := range lastSvcs {
			nameMatch := q == "" || strings.Contains(strings.ToLower(s.Name), q)
			// Which running containers to list: all when the service name matches,
			// otherwise only the containers that match the filter themselves.
			var shown []resolve.Candidate
			for _, c := range byService[s.Name] {
				if nameMatch || matchesFilter(c) {
					shown = append(shown, c)
				}
			}
			// Hide a service only if it neither matches by name nor has any
			// matching container.
			if !nameMatch && len(shown) == 0 {
				continue
			}
			// Collapsed by default (spec); keep a service the operator expanded.
			svcNode := tview.NewTreeNode(serviceRow(s, svcCols)).
				SetColor(serviceColor(s.Running, s.Desired)).
				SetReference(svcRef{name: s.Name}).
				SetExpanded(wasExpanded[s.Name])
			croot.AddChild(svcNode)
			if firstSvc == nil {
				firstSvc = svcNode
			}
			if s.Name == prevSvc {
				targetSvc = svcNode
			}
			for _, c := range shown {
				var label string
				if slotW > 0 {
					label = fmt.Sprintf("%-12s  %-*s  %-*s  up %s", shortID(c.ContainerID), nodeW, orDash(c.NodeName), slotW, slotStr(c), uptime(c.Uptime))
				} else {
					label = fmt.Sprintf("%-12s  %-*s  up %s", shortID(c.ContainerID), nodeW, orDash(c.NodeName), uptime(c.Uptime))
				}
				leaf := tview.NewTreeNode(annotateForwards(label, forwards.forContainer(c.ContainerID))).SetReference(c)
				svcNode.AddChild(leaf)
				if c.ContainerID == prevID {
					targetLeaf = leaf
					svcNode.SetExpanded(true) // reveal the previously-selected leaf
				}
			}
			// Marker depends on the final child count / expanded state, so set it
			// once the leaves are attached.
			markService(svcNode)
		}
		if len(croot.GetChildren()) == 0 {
			empty := "(no services)"
			if q != "" {
				empty = fmt.Sprintf("(no matches for %q)", filter)
			}
			croot.AddChild(tview.NewTreeNode(empty).SetColor(tcell.ColorGray).SetSelectable(false))
		}
		switch {
		case targetLeaf != nil:
			ctree.SetCurrentNode(targetLeaf)
		case targetSvc != nil:
			ctree.SetCurrentNode(targetSvc)
		case firstSvc != nil:
			ctree.SetCurrentNode(firstSvc)
		}
	}
	sortCands := func(cands []resolve.Candidate) {
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].Service != cands[j].Service {
				return cands[i].Service < cands[j].Service
			}
			if cands[i].Slot != cands[j].Slot {
				return cands[i].Slot < cands[j].Slot
			}
			return cands[i].NodeName < cands[j].NodeName
		})
	}
	// fetchContainers does the two manager round-trips (off the UI goroutine) and
	// returns sorted results. It is instrumented so a slow manager shows up in the
	// log viewer.
	fetchContainers := func() ([]resolve.Service, []resolve.Candidate, error) {
		start := time.Now()
		svcs, err := r.Services(ctx)
		clientlog.Timed("ui.containers.Services", start, err)
		if err != nil {
			return nil, nil, err
		}
		// Fetch every running container (not just the CLI-arg service); the tree
		// filters client-side so services with 0 containers still appear.
		start = time.Now()
		cands, err := r.Candidates(ctx, "")
		clientlog.Timed("ui.containers.Candidates", start, err)
		if err != nil {
			return nil, nil, err
		}
		sortCands(cands)
		return svcs, cands, nil
	}
	// applyContainers updates the tree from a fetch result. UI-goroutine only.
	applyContainers := func(svcs []resolve.Service, cands []resolve.Candidate, err error) {
		if err != nil {
			croot.ClearChildren()
			croot.AddChild(tview.NewTreeNode("error: " + err.Error()).SetColor(tcell.ColorRed).SetSelectable(false))
			return
		}
		lastSvcs = svcs
		lastCands = cands
		renderContainers()
	}
	// loadContainersSync fetches and applies on the caller's goroutine — used at
	// startup, before app.Run, where QueueUpdateDraw would deadlock.
	loadContainersSync := func() {
		svcs, cands, err := fetchContainers()
		applyContainers(svcs, cands, err)
	}
	// loadContainers refreshes without freezing the event loop: it fetches off the
	// UI goroutine (two manager round-trips that used to run inline and stall the
	// whole TUI) and applies the result via QueueUpdateDraw.
	loadContainers := func() {
		go func() {
			svcs, cands, err := fetchContainers()
			app.QueueUpdateDraw(func() { applyContainers(svcs, cands, err) })
		}()
	}
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
	autoRefreshContainers := func() {
		if overlayDepth.Load() > 0 || !autoRefreshBusy.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer autoRefreshBusy.Store(false)
			svcs, cands, err := fetchContainers()
			if err != nil || ctx.Err() != nil || overlayDepth.Load() > 0 {
				return // transient error, run ending, or an overlay opened meanwhile
			}
			app.QueueUpdateDraw(func() { applyContainers(svcs, cands, nil) })
		}()
	}
	openTerminal := func(c resolve.Candidate, command []string, tty bool) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		tctx, tcancel := context.WithCancel(ctx)
		tv := newTerminalView(app)
		tv.SetTitle(fmt.Sprintf(" %v · %s · %s on %s — Ctrl-] detach ", command, orDash(c.Service), shortID(c.ContainerID), orDash(c.NodeName)))
		var once sync.Once
		closeTerm := func() {
			once.Do(func() {
				tcancel()
				pages.RemovePage("term")
				app.SetFocus(ctree)
				loadContainers()
			})
		}
		tv.detach = closeTerm
		tv.run(tctx, cfg, ep, command, tty, f.connectTimeout, func(code int, rerr error) {
			app.QueueUpdateDraw(func() {
				switch {
				case tctx.Err() != nil:
					closeTerm() // user detached (Ctrl-]) — just close
				case rerr != nil:
					closeTerm()
					info(enrichAgentError(ctx, dcli, rerr).Error())
				case code != 0:
					// The command failed (e.g. `bash` not in the image). Keep the
					// pane up with its output so the error stays readable.
					tv.showEnded(fmt.Sprintf("[swarmexec] %v exited with code %d — press any key to close", command, code))
				default:
					closeTerm() // clean exit
				}
			})
		})
		pages.AddPage("term", tv, true, true)
		app.SetFocus(tv)
	}

	// logDefaults resolves the log view's initial format+filter from config,
	// falling back to sane defaults when the config values are invalid.
	logDefaults := func() (logfmt.Format, logfmt.Filter) {
		format, filter, err := buildLogFilter(cfg.Logs.Format, cfg.Logs.MinLevel, "")
		if err != nil {
			return logfmt.DefaultFormat(), logfmt.Filter{}
		}
		return format, filter
	}
	// logGrepPrompt asks for a message regexp and applies it to a log view.
	logGrepPrompt := func(lv *logViewer, back tview.Primitive, after func()) {
		in := tview.NewInputField().SetLabel("grep: ").SetFieldWidth(44).
			SetPlaceholder("regexp on the message — empty clears")
		in.SetDoneFunc(func(key tcell.Key) {
			pages.RemovePage("loggrep")
			app.SetFocus(back)
			if key == tcell.KeyEscape {
				return
			}
			txt := strings.TrimSpace(in.GetText())
			if txt == "" {
				lv.setGrep(nil)
				after()
				return
			}
			re, err := regexp.Compile(txt)
			if err != nil {
				info("invalid grep regexp: " + err.Error())
				return
			}
			lv.setGrep(re)
			after()
		})
		in.SetBorder(true).SetTitle(" filter logs ")
		pages.AddPage("loggrep", centered(in, 64, 3), true, true)
		app.SetFocus(in)
	}
	// logFooterText is the log view's footer hint. It appends the mouse state
	// because that is what decides whether terminal text-selection works: while
	// the app captures the mouse (the default), tview grabs drags for scrolling
	// and the terminal cannot select/copy — press m to hand the mouse back.
	logFooterText := func() string {
		m := " [yellow]m[white] mouse: app — press to select/copy in terminal"
		if !mouseEnabled {
			m = " [yellow]m[white] mouse: off — select & copy with your terminal"
		}
		return logViewHelp + "  " + m
	}
	// logViewKeys is the shared input capture for a log view: close, follow,
	// cycle format (F) / min-level (l), grep (/) and mouse capture (m).
	logViewKeys := func(lv *logViewer, follow *atomic.Bool, tv *tview.TextView, closeLogs, setTitle, refreshHint func()) func(*tcell.EventKey) *tcell.EventKey {
		return func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && ev.Rune() == 'q'):
				closeLogs()
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'f':
				follow.Store(!follow.Load())
				if follow.Load() {
					tv.ScrollToEnd() // re-enabling: jump to the newest line
				}
				setTitle()
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'F':
				lv.cycleFormat()
				setTitle()
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'l':
				lv.cycleLevel()
				setTitle()
			case ev.Key() == tcell.KeyRune && ev.Rune() == '/':
				logGrepPrompt(lv, tv, setTitle)
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'm':
				// Same mouse toggle as the tabs, but the tabs' flash lands on the
				// footer hidden behind this overlay, so reflect the state in the
				// log footer instead.
				if toggleMouse != nil {
					toggleMouse()
				}
				refreshHint()
			default:
				return ev
			}
			return nil
		}
	}

	// logPage wraps a log TextView with a footer key-hint line — the same place
	// every tab shows its shortcuts — so the log view's keys are consistent and
	// spelled out, instead of being crammed into the border title. It returns the
	// page and a closure that repaints the hint (used when the mouse state flips).
	logPage := func(tv *tview.TextView) (tview.Primitive, func()) {
		hint := tview.NewTextView().SetDynamicColors(true).SetText(logFooterText())
		page := tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(tv, 0, 1, true).
			AddItem(hint, 1, 0, false)
		return page, func() { hint.SetText(logFooterText()) }
	}

	showLogs := func(c resolve.Candidate) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
		follow := &atomic.Bool{}
		follow.Store(true)
		format, filter := logDefaults()
		lv := newLogViewer(app, tv, follow, format, filter)
		setTitle := func() {
			state := "on"
			if !follow.Load() {
				state = "off"
			}
			tv.SetTitle(fmt.Sprintf(" logs %s on %s — follow:%s · %s ",
				shortID(c.ContainerID), orDash(c.NodeName), state, lv.status()))
		}
		tv.SetBorder(true)
		setTitle()
		page, refreshHint := logPage(tv)
		lctx, lcancel := context.WithCancel(ctx)
		closeLogs := func() { lcancel(); pages.RemovePage("logs"); app.SetFocus(ctree) }
		tv.SetInputCapture(logViewKeys(lv, follow, tv, closeLogs, setTitle, refreshHint))
		target := resolve.FollowTarget{Service: c.Service, Slot: c.Slot, NodeID: c.NodeID}
		go func() {
			lerr := streamServiceLogs(lctx, cfg, r, target, ep,
				logsParams{follow: true, tail: 1000, connectTimeout: f.connectTimeout},
				&logIngest{v: lv}, &logIngest{v: lv, stderr: true}, lv.addNote)
			if lerr != nil && lctx.Err() == nil {
				app.QueueUpdateDraw(func() { fmt.Fprintf(tv, "\n[red]error: %s[-]\n", tview.Escape(lerr.Error())) })
			}
		}()
		pages.AddPage("logs", page, true, true)
		app.SetFocus(tv)
	}

	// showServiceLogs streams the logs of every container of a service into one
	// viewer, each line prefixed with [container@node].
	showServiceLogs := func(serviceName string, members []resolve.Candidate) {
		tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(true)
		follow := &atomic.Bool{}
		follow.Store(true)
		format, filter := logDefaults()
		lv := newLogViewer(app, tv, follow, format, filter)
		setTitle := func() {
			state := "on"
			if !follow.Load() {
				state = "off"
			}
			tv.SetTitle(fmt.Sprintf(" service logs %s (%d containers) — follow:%s · %s ",
				serviceName, len(members), state, lv.status()))
		}
		tv.SetBorder(true)
		setTitle()
		page, refreshHint := logPage(tv)
		lctx, lcancel := context.WithCancel(ctx)
		closeLogs := func() { lcancel(); pages.RemovePage("logs"); app.SetFocus(ctree) }
		tv.SetInputCapture(logViewKeys(lv, follow, tv, closeLogs, setTitle, refreshHint))
		for _, c := range members {
			ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
			target := resolve.FollowTarget{Service: c.Service, Slot: c.Slot, NodeID: c.NodeID}
			// A slot (or node, for a global service) tag stays valid across
			// replacements, unlike the container id — the reconnect notice reports
			// the new container/node.
			prefix := fmt.Sprintf("[slot %d] ", c.Slot)
			if c.Slot == 0 {
				prefix = fmt.Sprintf("[%s] ", orDash(c.NodeName))
			}
			go func(ep resolve.Endpoint, target resolve.FollowTarget, prefix string) {
				lerr := streamServiceLogs(lctx, cfg, r, target, ep,
					logsParams{follow: true, tail: 200, connectTimeout: f.connectTimeout},
					&logIngest{v: lv, prefix: prefix},
					&logIngest{v: lv, prefix: prefix, stderr: true},
					func(msg string) { lv.addNote(prefix + msg) })
				if lerr != nil && lctx.Err() == nil {
					app.QueueUpdateDraw(func() {
						fmt.Fprintf(tv, "[red]%serror: %s[-]\n", prefix, tview.Escape(lerr.Error()))
					})
				}
			}(ep, target, prefix)
		}
		pages.AddPage("logs", page, true, true)
		app.SetFocus(tv)
	}

	// startForward brings a forward up off the UI goroutine: dialling the agent
	// can take up to the connect timeout, and blocking the UI for that would
	// freeze the whole app. The entry is registered immediately in the starting
	// state so the operator sees that something is happening.
	startForward := func(c resolve.Candidate, local, remote uint32) {
		fctx, fcancel := context.WithCancel(ctx)
		var once sync.Once
		entry := forwards.add(c, local, remote, func() { once.Do(fcancel) })
		refreshForwardViews()

		go func() {
			ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
			fw, ferr := startForwarder(fctx, cfg, ep, forwardParams{
				address:        "127.0.0.1", // loopback: do not re-expose an internal port to the local network
				localPort:      local,
				remotePort:     remote,
				connectTimeout: f.connectTimeout,
			})
			if ferr != nil {
				forwards.markFailed(entry.id, ferr)
				app.QueueUpdateDraw(func() { refreshForwardViews() })
				return
			}
			addr := fw.LocalAddr().String()
			// Kept as a local: reading entry.localAddr later would race with
			// the registry's own writers.
			boundPort := local
			if _, ps, perr := net.SplitHostPort(addr); perr == nil {
				if n, cerr := strconv.ParseUint(ps, 10, 32); cerr == nil {
					boundPort = uint32(n)
				}
			}
			forwards.markActive(entry.id, addr)
			app.QueueUpdateDraw(func() { refreshForwardViews() })

			serr := fw.Serve(fctx, func(cerr error) {
				// Announce the first failure only: against an outdated agent
				// every connection fails, and flashing each one would hide the
				// footer behind a stutter of identical messages.
				// Only the first failure changes anything visible (the ⚠
				// marker). Redrawing on every one would rebuild the whole
				// container tree per rejected connection — a browser hammering
				// a broken forward would turn that into a redraw storm.
				if !forwards.noteConnError(entry.id, cerr) {
					return
				}
				app.QueueUpdateDraw(func() {
					refreshForwardViews()
					flash(fmt.Sprintf(" [red]forward %d[white]: %v", boundPort, cerr))
				})
			})
			fw.Close()
			// A cancelled forward was stopped on purpose; anything else is a
			// real failure the operator needs to see in the table.
			if serr != nil && fctx.Err() == nil {
				forwards.markFailed(entry.id, serr)
				app.QueueUpdateDraw(func() { refreshForwardViews() })
			}
		}()
	}

	// portPrompt asks which port to forward. There is deliberately no list of
	// exposed ports to pick from: the manager API cannot inspect a container on
	// another node, and the services worth forwarding are exactly the ones that
	// publish nothing — so a suggestion list would be empty where it matters.
	portPrompt := func(c resolve.Candidate) {
		input := tview.NewInputField().SetLabel(" port: ").SetFieldWidth(20)
		input.SetBorder(true).SetTitle(fmt.Sprintf(" forward %s on %s ", orDash(c.Service), orDash(c.NodeName)))
		hint := "  8080  or  9090:8080 (local:remote)"
		input.SetPlaceholder(hint)

		closePrompt := func() { pages.RemovePage("fwdprompt"); app.SetFocus(ctree) }
		input.SetDoneFunc(func(key tcell.Key) {
			if key != tcell.KeyEnter {
				closePrompt()
				return
			}
			local, remote, perr := parsePortSpec(strings.TrimSpace(input.GetText()))
			if perr != nil {
				// Keep the prompt open so the operator can correct the typo
				// instead of retyping the whole thing.
				input.SetTitle(fmt.Sprintf(" %v ", perr))
				return
			}
			closePrompt()
			startForward(c, local, remote)
			flash(fmt.Sprintf(" [green]forwarding[white] localhost:%d → %s:%d", local, shortID(c.ContainerID), remote))
		})
		pages.AddPage("fwdprompt", centered(input, 54, 3), true, true)
		app.SetFocus(input)
	}

	containerMenu := func(c resolve.Candidate) {
		ep := resolve.Endpoint{DialHost: c.DialHost, ContainerID: c.ContainerID, NodeID: c.NodeID, NodeName: c.NodeName}
		list := tview.NewList().ShowSecondaryText(false)
		list.SetBorder(true).SetTitle(fmt.Sprintf(" %s on %s — checking shells… ", orDash(c.Service), orDash(c.NodeName)))
		_, restoreHelp := pushOverlayHelp(footerKeys("j/k", "move", "Enter", "select", "Esc", "cancel"))
		closeMenu := func() { restoreHelp(); pages.RemovePage("menu"); app.SetFocus(ctree) }

		// Optimistic until the shell probe returns; then unavailable shells grey.
		bashOK, shOK, probed := true, true, false
		render := func() {
			cur := list.GetCurrentItem()
			list.Clear()
			list.AddItem("Logs", "", 0, func() { closeMenu(); showLogs(c) })
			list.AddItem(shellLabel("Bash", bashOK, probed), "", 0, func() {
				if bashOK {
					closeMenu()
					openTerminal(c, []string{"bash"}, true)
				}
			})
			list.AddItem(shellLabel("Sh", shOK, probed), "", 0, func() {
				if shOK {
					closeMenu()
					openTerminal(c, []string{"sh"}, false)
				}
			})
			list.AddItem("Port forward", "", 0, func() { closeMenu(); portPrompt(c) })
			list.AddItem("Cancel", "", 0, closeMenu)
			if cur >= 0 && cur < list.GetItemCount() {
				list.SetCurrentItem(cur)
			}
		}
		render()
		list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape {
				closeMenu()
				return nil
			}
			return vimListKeys(ev)
		})

		go func() {
			b, s := probeShells(ctx, cfg, ep, f.connectTimeout)
			app.QueueUpdateDraw(func() {
				bashOK, shOK, probed = b, s, true
				list.SetTitle(fmt.Sprintf(" %s on %s — actions ", orDash(c.Service), orDash(c.NodeName)))
				render()
			})
		}()

		// Height tracks the item count: 5 items plus the border.
		pages.AddPage("menu", centered(list, 48, 7), true, true)
		app.SetFocus(list)
	}
	ctree.SetSelectedFunc(func(node *tview.TreeNode) {
		if ref, ok := node.GetReference().(resolve.Candidate); ok {
			containerMenu(ref)
			return
		}
		// Service node → aggregated logs of all its containers.
		var members []resolve.Candidate
		for _, ch := range node.GetChildren() {
			if c, ok := ch.GetReference().(resolve.Candidate); ok {
				members = append(members, c)
			}
		}
		if len(members) > 0 {
			title := trimFoldMarker(node.GetText())
			if ref, ok := node.GetReference().(svcRef); ok {
				title = ref.name // the row now carries mode/image/ports — log by name
			}
			showServiceLogs(title, members)
		}
	})

	// ------------------------------------------------------------------- volumes
	const (
		volSortName = iota
		volSortNodes
		volSortUsed
		volSortAge
		volSortSize
	)
	// volSortCol maps a sort field to the header column it annotates with ▲/▼.
	volSortCol := map[int]int{volSortName: 0, volSortNodes: 2, volSortUsed: 3, volSortAge: 4, volSortSize: 5}
	vtable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	vtable.SetSelectedStyle(selStyle)
	vHeaders := []string{"VOLUME", "DRIVER", "NODES", "USED BY", "AGE", "SIZE"}
	var vols []swarmVolume
	var volUsage map[string][]volumeConsumer
	var volSizes map[string]int64
	var volErrs map[string]error
	// selectedVols holds the volumes marked with space for a bulk delete, keyed
	// by name so the selection survives sorting and re-render.
	selectedVols := map[string]bool{}
	// shownVols is the filtered + sorted subset currently displayed; it is what
	// selectedVolume() and "select all" index. volFilter is the "/" search query.
	var shownVols []swarmVolume
	volFilter := ""
	volMatches := func(v swarmVolume) bool {
		q := strings.ToLower(strings.TrimSpace(volFilter))
		if q == "" {
			return true
		}
		if strings.Contains(strings.ToLower(v.Name), q) || strings.Contains(strings.ToLower(v.Driver), q) {
			return true
		}
		for _, n := range v.Nodes {
			if strings.Contains(strings.ToLower(n.Name), q) {
				return true
			}
		}
		return false
	}
	sortField := volSortName
	sortDesc := false
	volSizesLoading := false

	// sortVolumes orders rows by the active field; for size/age an unknown value
	// always sorts last (regardless of direction), with name as the tiebreaker.
	sortVolumes := func(rows []swarmVolume) {
		known := func(v swarmVolume) bool {
			switch sortField {
			case volSortSize:
				_, ok := volSizes[v.Name]
				return ok
			case volSortAge:
				return !v.Created.IsZero()
			default:
				return true
			}
		}
		less := func(a, b swarmVolume) bool {
			switch sortField {
			case volSortNodes:
				if len(a.Nodes) != len(b.Nodes) {
					return len(a.Nodes) < len(b.Nodes)
				}
			case volSortUsed:
				if ua, ub := len(volUsage[a.Name]), len(volUsage[b.Name]); ua != ub {
					return ua < ub
				}
			case volSortAge:
				if !a.Created.Equal(b.Created) {
					return a.Created.Before(b.Created) // earlier = older = "more age"
				}
			case volSortSize:
				if sa, sb := volSizes[a.Name], volSizes[b.Name]; sa != sb {
					return sa < sb
				}
			}
			return a.Name < b.Name
		}
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if ka, kb := known(a), known(b); ka != kb {
				return ka // known before unknown, both directions
			}
			if sortDesc {
				return less(b, a)
			}
			return less(a, b)
		})
	}

	renderVolumeTable := func() {
		// Keep the cursor on the same volume across re-render (sort/size refresh).
		selName := ""
		if row, _ := vtable.GetSelection(); row >= 1 {
			if c := vtable.GetCell(row, 0); c != nil {
				selName = strings.TrimLeft(c.Text, "▣ ") // drop the selection marker
			}
		}
		vtable.Clear()
		for c, h := range vHeaders {
			if volSortCol[sortField] == c {
				if sortDesc {
					h += " ▼"
				} else {
					h += " ▲"
				}
			}
			// While the (slow) size scan runs, show a loading marker on the SIZE
			// header; the spinner goroutine animates this cell.
			if c == volSortCol[volSortSize] && volSizesLoading {
				h += " loading…"
			}
			vtable.SetCell(0, c, headerCell(h))
		}
		// Build the displayed subset from the "/" filter, then sort it. The
		// displayed order must match the slice selectedVolume() indexes, so both
		// use shownVols — otherwise a filter/sort makes delete act on the wrong row.
		shownVols = shownVols[:0]
		for _, v := range vols {
			if volMatches(v) {
				shownVols = append(shownVols, v)
			}
		}
		sortVolumes(shownVols)
		selRow := 1
		for i, v := range shownVols {
			used := len(volUsage[v.Name])
			usedCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if used > 0 {
				usedCell = tview.NewTableCell(fmt.Sprintf("%d", used)).SetTextColor(tcell.ColorGreen).SetExpansion(1)
			}
			sizeCell := tview.NewTableCell("…").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if volSizes != nil {
				size, color := int64(-1), tcell.ColorGray
				if s, ok := volSizes[v.Name]; ok {
					size, color = s, tcell.ColorWhite
				}
				sizeCell = tview.NewTableCell(humanBytes(size)).SetTextColor(color).SetExpansion(1)
			}
			mark, nameColor := "  ", tcell.ColorWhite
			if selectedVols[v.Name] {
				mark, nameColor = "▣ ", tcell.ColorAqua
			}
			vtable.SetCell(i+1, 0, tview.NewTableCell(mark+v.Name).SetTextColor(nameColor).SetExpansion(1))
			vtable.SetCell(i+1, 1, tview.NewTableCell(orDash(v.Driver)).SetExpansion(1))
			vtable.SetCell(i+1, 2, tview.NewTableCell(fmt.Sprintf("%d: %s", len(v.Nodes), joinNodes(v.Nodes))).SetExpansion(1))
			vtable.SetCell(i+1, 3, usedCell)
			vtable.SetCell(i+1, 4, tview.NewTableCell(volumeAge(v.Created)).SetExpansion(1))
			vtable.SetCell(i+1, 5, sizeCell)
			if v.Name == selName {
				selRow = i + 1
			}
		}
		if len(shownVols) > 0 {
			vtable.Select(selRow, 0)
		}
		if len(volErrs) > 0 {
			vtable.SetCell(len(shownVols)+1, 0, tview.NewTableCell(fmt.Sprintf("(%d node(s) unreachable)", len(volErrs))).SetTextColor(tcell.ColorYellow).SetSelectable(false))
		}
	}

	loadVolumes := func() {
		vtable.Clear()
		for c, h := range vHeaders {
			vtable.SetCell(0, c, headerCell(h))
		}
		vtable.SetCell(1, 0, tview.NewTableCell("loading…").SetTextColor(tcell.ColorGray))
		go func() {
			start := time.Now()
			nodes, nerr := r.Nodes(ctx)
			var vs []swarmVolume
			var errs map[string]error
			var usage map[string][]volumeConsumer
			noAgent := false
			if nerr == nil {
				vs, errs = indexVolumes(ctx, cfg, nodes, f.connectTimeout)
				usage = indexVolumeUsage(ctx, cfg, nodes, f.connectTimeout)
				if len(nodes) > 0 && len(errs) == len(nodes) && !agentDeployed(ctx, dcli) {
					noAgent = true
				}
			}
			clientlog.Timed("ui.loadVolumes", start, nerr, "nodes", len(nodes), "vols", len(vs))
			app.QueueUpdateDraw(func() {
				vols, volUsage, volErrs, volSizes = vs, usage, errs, nil
				if nerr != nil {
					vtable.Clear()
					for c, h := range vHeaders {
						vtable.SetCell(0, c, headerCell(h))
					}
					vtable.SetCell(1, 0, tview.NewTableCell("error: "+nerr.Error()).SetTextColor(tcell.ColorRed))
					return
				}
				if noAgent {
					vtable.Clear()
					for c, h := range vHeaders {
						vtable.SetCell(0, c, headerCell(h))
					}
					vtable.SetCell(1, 0, tview.NewTableCell(errNoAgent.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
					return
				}
				renderVolumeTable()
			})

			// Sizes are computed via a du-style disk-usage scan, which is slow, so
			// fill the SIZE column in a second pass once the list is already shown.
			// A spinner on the SIZE header makes clear the data is still loading.
			if nerr == nil && !noAgent {
				app.QueueUpdateDraw(func() {
					volSizesLoading = true
					renderVolumeTable()
				})
				stop := make(chan struct{})
				go func() {
					frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
					tk := time.NewTicker(150 * time.Millisecond)
					defer tk.Stop()
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						case <-tk.C:
							frame := frames[i%len(frames)]
							app.QueueUpdateDraw(func() {
								if volSizesLoading {
									vtable.SetCell(0, volSortCol[volSortSize], headerCell("SIZE "+frame))
								}
							})
						}
					}
				}()
				sz := indexVolumeSizes(ctx, cfg, nodes, f.connectTimeout)
				close(stop)
				app.QueueUpdateDraw(func() {
					volSizesLoading = false
					volSizes = sz
					renderVolumeTable()
				})
			}
		}()
	}
	selectedVolume := func() (swarmVolume, bool) {
		row, _ := vtable.GetSelection()
		i := row - 1
		if i < 0 || i >= len(shownVols) {
			return swarmVolume{}, false
		}
		return shownVols[i], true
	}

	showVolumeNodes := func(v swarmVolume) {
		list := tview.NewList().ShowSecondaryText(false)
		title := fmt.Sprintf(" volume %s — %d node(s) ", shortVolume(v.Name), len(v.Nodes))
		if !v.Created.IsZero() {
			title = fmt.Sprintf(" volume %s — %d node(s) · created %s ", shortVolume(v.Name), len(v.Nodes), volumeCreated(v.Created))
		}
		list.SetBorder(true).SetTitle(title)
		sel := make([]bool, len(v.Nodes))
		render := func() {
			cur := list.GetCurrentItem()
			list.Clear()
			for i, n := range v.Nodes {
				mark := "[ ]"
				if sel[i] {
					mark = "[x]"
				}
				list.AddItem(fmt.Sprintf("%s %s", mark, n.Name), "", 0, nil)
			}
			if cur < list.GetItemCount() {
				list.SetCurrentItem(cur)
			}
		}
		render()
		closeNodes := func() { pages.RemovePage("volnodes"); app.SetFocus(vtable) }

		runDelete := func(targets []resolve.Node) {
			go func() {
				results := removeOnNodes(ctx, cfg, targets, v.Name, false, f.connectTimeout)
				app.QueueUpdateDraw(func() {
					closeNodes()
					loadVolumes()
					var b []string
					for _, res := range results {
						if res.err != nil {
							b = append(b, fmt.Sprintf("%s: error: %v", res.node.Name, res.err))
						} else {
							b = append(b, fmt.Sprintf("%s: removed", res.node.Name))
						}
					}
					info(fmt.Sprintf("volume %q:\n%s", v.Name, joinLines(b)))
				})
			}()
		}
		confirmDelete := func(targets []resolve.Node) {
			m := tview.NewModal().
				SetText(fmt.Sprintf("Remove volume %q on %d node(s)?\n%s", v.Name, len(targets), joinNodes(targets))).
				AddButtons([]string{"Delete", "Cancel"}).
				SetDoneFunc(func(_ int, label string) {
					pages.RemovePage("confirm")
					if label == "Delete" {
						runDelete(targets)
					} else {
						app.SetFocus(list)
					}
				})
			pages.AddPage("confirm", m, true, true)
			app.SetFocus(m)
		}

		list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape:
				closeNodes()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == ' ':
				if i := list.GetCurrentItem(); i < len(sel) {
					sel[i] = !sel[i]
					render()
				}
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
				confirmDelete(v.Nodes)
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
				var chosen []resolve.Node
				for i, s := range sel {
					if s {
						chosen = append(chosen, v.Nodes[i])
					}
				}
				if len(chosen) == 0 {
					if i := list.GetCurrentItem(); i < len(v.Nodes) {
						chosen = []resolve.Node{v.Nodes[i]}
					}
				}
				if len(chosen) > 0 {
					confirmDelete(chosen)
				}
				return nil
			}
			return vimListKeys(ev)
		})
		help := tview.NewTextView().SetDynamicColors(true).SetText(
			" [yellow]space[white] select  [yellow]d[white] delete selected  [yellow]a[white] delete all  [yellow]ESC[white] back")
		box := tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(list, 0, 1, true).
			AddItem(help, 1, 0, false)
		pages.AddPage("volnodes", centered(box, 64, len(v.Nodes)+5), true, true)
		app.SetFocus(list)
	}
	vtable.SetSelectedFunc(func(int, int) {
		if v, ok := selectedVolume(); ok && len(v.Nodes) > 0 {
			showVolumeNodes(v)
		}
	})

	// showVolumeConsumers lists the services/containers that mount a volume.
	showVolumeConsumers := func(v swarmVolume) {
		consumers := volUsage[v.Name]
		list := tview.NewList().ShowSecondaryText(false)
		list.SetBorder(true).SetTitle(fmt.Sprintf(" %s — used by %d ", v.Name, len(consumers)))
		if len(consumers) == 0 {
			list.AddItem("(not in use by any container)", "", 0, nil)
		} else {
			svcW, contW := 0, 0
			for _, c := range consumers {
				if w := len(orDash(c.Service)); w > svcW {
					svcW = w
				}
				if w := len(orDash(c.Container)); w > contW {
					contW = w
				}
			}
			for _, c := range consumers {
				list.AddItem(fmt.Sprintf("%-*s  %-*s  on %s", svcW, orDash(c.Service), contW, orDash(c.Container), orDash(c.Node)), "", 0, nil)
			}
		}
		_, restoreHelp := pushOverlayHelp(footerKeys("j/k", "move", "Esc", "back"))
		closeUsers := func() { restoreHelp(); pages.RemovePage("volusers"); app.SetFocus(vtable) }
		list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')) {
				closeUsers()
				return nil
			}
			return vimListKeys(ev)
		})
		rows := len(consumers)
		if rows == 0 {
			rows = 1
		}
		pages.AddPage("volusers", centered(list, 72, rows+4), true, true)
		app.SetFocus(list)
	}

	// deleteVolumes removes each target volume on every node that holds it, behind
	// a single confirm. Volumes are node-local, so a volume is removed across all
	// its v.Nodes. Shared by the multi-select delete and prune.
	deleteVolumes := func(targets []swarmVolume, prompt string) {
		if len(targets) == 0 {
			return
		}
		shown := make([]string, 0, len(targets))
		for _, v := range targets {
			shown = append(shown, shortVolume(v.Name))
		}
		extra := 0
		if len(shown) > 12 {
			extra, shown = len(shown)-12, shown[:12]
		}
		body := prompt + "\n\n" + strings.Join(shown, "\n")
		if extra > 0 {
			body += fmt.Sprintf("\n(+%d more)", extra)
		}
		m := tview.NewModal().SetText(body).AddButtons([]string{"Delete", "Cancel"}).
			SetDoneFunc(func(_ int, label string) {
				pages.RemovePage("confirm")
				if label != "Delete" {
					app.SetFocus(vtable)
					return
				}
				// Deleting runs per volume across every node it holds, which can
				// take a while, so show a progress overlay instead of freezing.
				prog := tview.NewTextView().SetTextAlign(tview.AlignCenter).SetDynamicColors(true)
				prog.SetBorder(true).SetTitle(" deleting volumes ")
				prog.SetText(fmt.Sprintf("\ndeleted 0/%d…", len(targets)))
				pages.AddPage("volprogress", centered(prog, 60, 5), true, true)
				app.SetFocus(prog)
				go func() {
					// Delete volumes with bounded parallelism: each volume already
					// fans out across its nodes, so a small volume-level pool keeps
					// the total load on the agents in check. A mutex guards the
					// shared counters and the progress overlay shows completions.
					var (
						mu      sync.Mutex
						fails   []string
						done    int
						removed int
					)
					sem := make(chan struct{}, volumeDeleteFanout)
					var wg sync.WaitGroup
					for _, v := range targets {
						wg.Add(1)
						sem <- struct{}{}
						go func(v swarmVolume) {
							defer wg.Done()
							defer func() { <-sem }()
							ok := true
							var vf []string
							for _, res := range removeOnNodes(ctx, cfg, v.Nodes, v.Name, false, f.connectTimeout) {
								if res.err != nil {
									ok = false
									vf = append(vf, fmt.Sprintf("%s on %s: %v", shortVolume(v.Name), res.node.Name, res.err))
								}
							}
							mu.Lock()
							done++
							if ok {
								removed++
							}
							fails = append(fails, vf...)
							d := done
							mu.Unlock()
							app.QueueUpdateDraw(func() {
								prog.SetText(fmt.Sprintf("\ndeleted %d/%d…", d, len(targets)))
							})
						}(v)
					}
					wg.Wait()
					app.QueueUpdateDraw(func() {
						pages.RemovePage("volprogress")
						selectedVols = map[string]bool{}
						loadVolumes()
						updateStatus()
						summary := fmt.Sprintf("removed %d of %d volume(s)", removed, len(targets))
						if len(fails) > 0 {
							summary += ":\n" + joinLines(fails)
						}
						info(summary)
					})
				}()
			})
		pages.AddPage("confirm", m, true, true)
		app.SetFocus(m)
	}

	// pruneVolumes deletes every volume that no running container mounts and no
	// service declares (service-declared volumes are spared even with no running
	// task). The service check needs a ServiceList, so it runs off the UI goroutine.
	pruneVolumes := func() {
		go func() {
			declared := serviceVolumeNames(ctx, dcli)
			app.QueueUpdateDraw(func() {
				var targets []swarmVolume
				for _, v := range vols {
					if len(volUsage[v.Name]) == 0 && !declared[v.Name] {
						targets = append(targets, v)
					}
				}
				if len(targets) == 0 {
					info("no unused volumes to prune (all are in use or declared by a service)")
					return
				}
				deleteVolumes(targets, fmt.Sprintf("Prune %d unused volume(s)? This cannot be undone.", len(targets)))
			})
		}()
	}

	// ------------------------------------------------------------------ forwards
	ftable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	ftable.SetSelectedStyle(selStyle)
	fHeaders := []string{"LOCAL", "REMOTE", "CONTAINER", "SERVICE", "NODE", "AGE", "STATE"}
	// fRows mirrors the rendered table so a row index maps back to a forward.
	var fRows []forwardEntry
	renderForwards := func() {
		prev, _ := ftable.GetSelection()
		ftable.Clear()
		for i, h := range fHeaders {
			ftable.SetCell(0, i, headerCell(h))
		}
		fRows = forwards.list()
		for i, e := range fRows {
			row := i + 1
			local := "-"
			if p := e.boundPort(); p > 0 {
				local = fmt.Sprintf("127.0.0.1:%d", p)
			}
			state := e.state.String()
			color := tcell.ColorWhite
			switch e.state {
			case forwardActive:
				color = tcell.ColorGreen
				if e.connErr != nil {
					// Listening, but connections are failing — the operator
					// needs to see that, not a reassuring green "active". Kept
					// to a marker because the column truncates; Enter shows the
					// reason in full, and it is flashed once when it happens.
					color = tcell.ColorYellow
					state = "active ⚠"
				}
			case forwardStarting:
				color = tcell.ColorYellow
			case forwardFailed:
				color = tcell.ColorRed
				if e.err != nil {
					// The reason matters more than the word "failed": it is the
					// only place the operator can learn what went wrong.
					state = "failed: " + e.err.Error()
				}
			}
			ftable.SetCell(row, 0, tview.NewTableCell(local))
			ftable.SetCell(row, 1, tview.NewTableCell(fmt.Sprintf("%d", e.remote)))
			ftable.SetCell(row, 2, tview.NewTableCell(shortID(e.cand.ContainerID)))
			ftable.SetCell(row, 3, tview.NewTableCell(orDash(e.cand.Service)))
			ftable.SetCell(row, 4, tview.NewTableCell(orDash(e.cand.NodeName)))
			ftable.SetCell(row, 5, tview.NewTableCell(uptime(time.Since(e.started))))
			ftable.SetCell(row, 6, tview.NewTableCell(state).SetTextColor(color))
		}
		if len(fRows) == 0 {
			ftable.SetCell(1, 0, tview.NewTableCell("(no forwards — press p on a container)").
				SetTextColor(tcell.ColorGray).SetSelectable(false))
			return
		}
		if prev > 0 && prev <= len(fRows) {
			ftable.Select(prev, 0)
		} else {
			ftable.Select(1, 0)
		}
	}
	// selectedForward maps the cursor row back to a forward.
	selectedForward := func() (forwardEntry, bool) {
		row, _ := ftable.GetSelection()
		if row < 1 || row > len(fRows) {
			return forwardEntry{}, false
		}
		return fRows[row-1], true
	}

	// ------------------------------------------------------------------ networks
	nettable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	nettable.SetSelectedStyle(selStyle)
	nHeaders := []string{"NETWORK", "DRIVER", "SCOPE", "TYPE", "SERVICES", "AGE"}
	var nets []swarmNetwork
	renderNetworks := func() {
		selName := ""
		if row, _ := nettable.GetSelection(); row >= 1 {
			if c := nettable.GetCell(row, 0); c != nil {
				selName = c.Text
			}
		}
		nettable.Clear()
		for c, h := range nHeaders {
			nettable.SetCell(0, c, headerCell(h))
		}
		selRow := 1
		for i, n := range nets {
			svcCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if len(n.Services) > 0 {
				svcCell = tview.NewTableCell(fmt.Sprintf("%d", len(n.Services))).SetTextColor(tcell.ColorGreen).SetExpansion(1)
			}
			typeColor := networkTypeColor(n)
			nettable.SetCell(i+1, 0, tview.NewTableCell(n.Name).SetTextColor(typeColor).SetExpansion(1))
			nettable.SetCell(i+1, 1, tview.NewTableCell(orDash(n.Driver)).SetExpansion(1))
			nettable.SetCell(i+1, 2, tview.NewTableCell(orDash(n.Scope)).SetExpansion(1))
			nettable.SetCell(i+1, 3, tview.NewTableCell(networkType(n)).SetTextColor(typeColor).SetExpansion(1))
			nettable.SetCell(i+1, 4, svcCell)
			nettable.SetCell(i+1, 5, tview.NewTableCell(volumeAge(n.Created)).SetExpansion(1))
			if n.Name == selName {
				selRow = i + 1
			}
		}
		if len(nets) > 0 {
			nettable.Select(selRow, 0)
		}
	}
	loadNetworks := func() {
		nettable.Clear()
		for c, h := range nHeaders {
			nettable.SetCell(0, c, headerCell(h))
		}
		nettable.SetCell(1, 0, tview.NewTableCell("loading…").SetTextColor(tcell.ColorGray))
		go func() {
			start := time.Now()
			list, err := listNetworks(ctx, dcli)
			clientlog.Timed("ui.loadNetworks", start, err)
			app.QueueUpdateDraw(func() {
				if err != nil {
					nettable.Clear()
					for c, h := range nHeaders {
						nettable.SetCell(0, c, headerCell(h))
					}
					nettable.SetCell(1, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
					return
				}
				nets = list
				renderNetworks()
			})
		}()
	}
	selectedNetwork := func() (swarmNetwork, bool) {
		row, _ := nettable.GetSelection()
		i := row - 1
		if i < 0 || i >= len(nets) {
			return swarmNetwork{}, false
		}
		return nets[i], true
	}
	// serviceNamesFromCache returns the known service names (the containers-tab
	// cache), used to seed the attach autocomplete. It is only a suggestion list —
	// the actual attach resolves the name live, so a just-created service that is
	// not cached yet can still be typed in.
	serviceNamesFromCache := func() []string {
		names := make([]string, 0, len(lastSvcs))
		for _, s := range lastSvcs {
			names = append(names, s.Name)
		}
		return names
	}
	// servicePrompt opens an autocomplete input to choose a service, then a
	// confirmation (the change triggers a rolling update of that service), then
	// runs do(service) off the UI goroutine and calls onDone on success. It backs
	// both attach and detach: actionLabel is the confirm button ("Attach" /
	// "Detach"), confirmVerb the sentence lead-in, suggestions feed the
	// autocomplete only, and back gets focus when the operator cancels.
	servicePrompt := func(title, confirmVerb, actionLabel string, suggestions []string, back tview.Primitive, do func(string) error, onDone func()) {
		in := tview.NewInputField().SetLabel("service: ").SetFieldWidth(46)
		in.SetPlaceholder("type or ↓ to pick; Enter confirms, Esc cancels")
		in.SetAutocompleteFunc(func(text string) []string {
			text = strings.ToLower(strings.TrimSpace(text))
			var out []string
			for _, s := range suggestions {
				if text == "" || strings.Contains(strings.ToLower(s), text) {
					out = append(out, s)
				}
			}
			return out
		})
		in.SetDoneFunc(func(key tcell.Key) {
			name := strings.TrimSpace(in.GetText())
			if key != tcell.KeyEnter || name == "" {
				pages.RemovePage("svcprompt")
				app.SetFocus(back)
				return
			}
			pages.RemovePage("svcprompt")
			confirm := tview.NewModal().
				SetText(fmt.Sprintf("%s %q?\n\nThis triggers a rolling update of the service.", confirmVerb, name)).
				AddButtons([]string{actionLabel, "Cancel"}).
				SetDoneFunc(func(_ int, label string) {
					pages.RemovePage("svcconfirm")
					if label != actionLabel {
						app.SetFocus(back)
						return
					}
					go func() {
						err := do(name)
						app.QueueUpdateDraw(func() {
							if err != nil {
								info(strings.ToLower(actionLabel) + " failed: " + err.Error())
								return
							}
							onDone()
							info(fmt.Sprintf("%q updated — rolling update started", name))
						})
					}()
				})
			pages.AddPage("svcconfirm", confirm, true, true)
			app.SetFocus(confirm)
		})
		in.SetBorder(true).SetTitle(" " + title + " ")
		pages.AddPage("svcprompt", centered(in, 66, 3), true, true)
		app.SetFocus(in)
	}

	// showNetworkMembers lists the services attached to a network with the
	// containers of each service nested under it. Service membership is known
	// synchronously (from the list); the per-service containers need a task
	// lookup, so they fill in lazily after the overlay is up.
	showNetworkMembers := func(n swarmNetwork) {
		list := tview.NewList().ShowSecondaryText(false)
		list.SetBorder(true).SetTitle(fmt.Sprintf(" %s — attached services ", n.Name))
		render := func(svcs []netService, loading bool) {
			cur := list.GetCurrentItem()
			list.Clear()
			if len(svcs) == 0 {
				if loading {
					list.AddItem("loading…", "", 0, nil)
				} else {
					list.AddItem("(no services attached)", "", 0, nil)
				}
			}
			// Pad the id/node columns to the widest across every service so the
			// node and IP columns line up down the whole list, not just per row.
			idW, nodeW := 0, 0
			for _, s := range svcs {
				for _, c := range s.Containers {
					if w := len(c.ID); w > idW {
						idW = w
					}
					if w := len(orDash(c.Node)); w > nodeW {
						nodeW = w
					}
				}
			}
			for _, s := range svcs {
				head := s.Name + " …"
				if !loading {
					head = fmt.Sprintf("%s (%d)", s.Name, len(s.Containers))
				}
				list.AddItem(head, "", 0, nil)
				for _, c := range s.Containers {
					list.AddItem(fmt.Sprintf("    %-*s  %-*s  %s", idW, c.ID, nodeW, orDash(c.Node), orDash(c.IPv4)), "", 0, nil)
				}
			}
			if cur < list.GetItemCount() {
				list.SetCurrentItem(cur)
			}
		}
		// Seed with the services already known from the list; containers pending.
		init := make([]netService, 0, len(n.Services))
		for _, s := range n.Services {
			init = append(init, netService{Name: s})
		}
		render(init, true)
		_, restoreHelp := pushOverlayHelp(footerKeys("a", "attach", "d", "detach", "j/k", "move", "Esc", "back"))
		closeMembers := func() { restoreHelp(); pages.RemovePage("netmembers"); app.SetFocus(nettable) }
		list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')):
				closeMembers()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
				suggestions := servicesExcluding(serviceNamesFromCache(), n.Services)
				servicePrompt(
					fmt.Sprintf("attach a service to network %q", n.Name),
					fmt.Sprintf("Attach network %q to service", n.Name), "Attach",
					suggestions, list,
					func(svc string) error { return attachServiceToNetwork(ctx, dcli, svc, n.ID, n.Name) },
					func() { closeMembers(); loadNetworks() },
				)
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
				if len(n.Services) == 0 {
					info("no services are attached to this network")
					return nil
				}
				servicePrompt(
					fmt.Sprintf("detach a service from network %q", n.Name),
					fmt.Sprintf("Detach network %q from service", n.Name), "Detach",
					n.Services, list,
					func(svc string) error { return detachServiceFromNetwork(ctx, dcli, svc, n.ID, n.Name) },
					func() { closeMembers(); loadNetworks() },
				)
				return nil
			}
			return vimListKeys(ev)
		})
		height := len(n.Services) + 6
		if height > 22 {
			height = 22
		}
		pages.AddPage("netmembers", centered(list, 78, height), true, true)
		app.SetFocus(list)
		go func() {
			members := networkMembers(ctx, dcli, n)
			app.QueueUpdateDraw(func() {
				// Only repaint if this overlay is still the one on screen.
				if pages.HasPage("netmembers") {
					render(members, false)
				}
			})
		}()
	}
	nettable.SetSelectedFunc(func(int, int) {
		if n, ok := selectedNetwork(); ok {
			showNetworkMembers(n)
		}
	})

	// ------------------------------------------------------------------- secrets
	sectable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	sectable.SetSelectedStyle(selStyle)
	sHeaders := []string{"SECRET", "USED BY", "AGE", "UPDATED", "LABELS"}
	var secs []swarmSecret
	renderSecrets := func() {
		selName := ""
		if row, _ := sectable.GetSelection(); row >= 1 {
			if c := sectable.GetCell(row, 0); c != nil {
				selName = c.Text
			}
		}
		sectable.Clear()
		for c, h := range sHeaders {
			sectable.SetCell(0, c, headerCell(h))
		}
		selRow := 1
		for i, s := range secs {
			usedCell := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if len(s.Services) > 0 {
				usedCell = tview.NewTableCell(fmt.Sprintf("%d", len(s.Services))).SetTextColor(tcell.ColorGreen).SetExpansion(1)
			}
			labels := tview.NewTableCell("-").SetTextColor(tcell.ColorGray).SetExpansion(1)
			if len(s.Labels) > 0 {
				labels = tview.NewTableCell(fmt.Sprintf("%d", len(s.Labels))).SetExpansion(1)
			}
			sectable.SetCell(i+1, 0, tview.NewTableCell(s.Name).SetExpansion(1))
			sectable.SetCell(i+1, 1, usedCell)
			sectable.SetCell(i+1, 2, tview.NewTableCell(volumeAge(s.Created)).SetExpansion(1))
			sectable.SetCell(i+1, 3, tview.NewTableCell(volumeAge(s.Updated)).SetExpansion(1))
			sectable.SetCell(i+1, 4, labels)
			if s.Name == selName {
				selRow = i + 1
			}
		}
		if len(secs) > 0 {
			sectable.Select(selRow, 0)
		}
	}
	loadSecrets := func() {
		sectable.Clear()
		for c, h := range sHeaders {
			sectable.SetCell(0, c, headerCell(h))
		}
		sectable.SetCell(1, 0, tview.NewTableCell("loading…").SetTextColor(tcell.ColorGray))
		go func() {
			start := time.Now()
			list, err := listSecrets(ctx, dcli)
			clientlog.Timed("ui.loadSecrets", start, err)
			app.QueueUpdateDraw(func() {
				if err != nil {
					sectable.Clear()
					for c, h := range sHeaders {
						sectable.SetCell(0, c, headerCell(h))
					}
					sectable.SetCell(1, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
					return
				}
				secs = list
				renderSecrets()
			})
		}()
	}
	selectedSecret := func() (swarmSecret, bool) {
		row, _ := sectable.GetSelection()
		i := row - 1
		if i < 0 || i >= len(secs) {
			return swarmSecret{}, false
		}
		return secs[i], true
	}
	// showSecretDetail shows a secret's metadata and the services/containers that
	// use it. The value is deliberately absent: the Docker API never returns it,
	// and the overlay says so. Usage (services with their running containers)
	// needs a task lookup, so it fills in lazily after the overlay is up.
	showSecretDetail := func(s swarmSecret) {
		ts := func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		}
		tv := tview.NewTextView().SetDynamicColors(true)
		tv.SetBorder(true).SetTitle(fmt.Sprintf(" secret %s ", s.Name))
		render := func(members []netService, loading bool) {
			var b strings.Builder
			fmt.Fprintf(&b, "Name:     %s\n", s.Name)
			fmt.Fprintf(&b, "ID:       %s\n", s.ID)
			fmt.Fprintf(&b, "Created:  %s\n", ts(s.Created))
			fmt.Fprintf(&b, "Updated:  %s\n", ts(s.Updated))
			if len(s.Labels) == 0 {
				fmt.Fprintf(&b, "Labels:   -\n")
			} else {
				fmt.Fprintf(&b, "Labels:\n")
				keys := make([]string, 0, len(s.Labels))
				for k := range s.Labels {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					fmt.Fprintf(&b, "  %s=%s\n", k, s.Labels[k])
				}
			}
			fmt.Fprintf(&b, "\nused by:\n")
			if len(members) == 0 {
				if loading {
					fmt.Fprintf(&b, "  …\n")
				} else {
					fmt.Fprintf(&b, "  (no services)\n")
				}
			}
			for _, m := range members {
				head := m.Name + " …"
				if !loading {
					head = fmt.Sprintf("%s (%d)", m.Name, len(m.Containers))
				}
				fmt.Fprintf(&b, "  %s\n", head)
				for _, c := range m.Containers {
					fmt.Fprintf(&b, "      %s  %s\n", c.ID, orDash(c.Node))
				}
			}
			fmt.Fprintf(&b, "\n[gray]the secret value is not retrievable via the Docker API[white]")
			tv.SetText(b.String())
		}
		// Seed with the service names already known from the list; containers pending.
		init := make([]netService, 0, len(s.Services))
		for _, name := range s.Services {
			init = append(init, netService{Name: name})
		}
		render(init, true)
		_, restoreHelp := pushOverlayHelp(footerKeys("a", "attach", "d", "detach", "j/k", "scroll", "Esc", "back"))
		closeSecret := func() { restoreHelp(); pages.RemovePage("secdetail"); app.SetFocus(sectable) }
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')):
				closeSecret()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
				suggestions := servicesExcluding(serviceNamesFromCache(), s.Services)
				servicePrompt(
					fmt.Sprintf("attach secret %q to a service", s.Name),
					fmt.Sprintf("Attach secret %q to service", s.Name), "Attach",
					suggestions, tv,
					func(svc string) error { return attachSecretToService(ctx, dcli, svc, s.ID, s.Name) },
					func() { closeSecret(); loadSecrets() },
				)
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
				if len(s.Services) == 0 {
					info("no services use this secret")
					return nil
				}
				servicePrompt(
					fmt.Sprintf("detach secret %q from a service", s.Name),
					fmt.Sprintf("Detach secret %q from service", s.Name), "Detach",
					s.Services, tv,
					func(svc string) error { return detachSecretFromService(ctx, dcli, svc, s.ID, s.Name) },
					func() { closeSecret(); loadSecrets() },
				)
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
				return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
				return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
			}
			return ev
		})
		height := 11 + len(s.Labels) + len(s.Services)
		if height > 24 {
			height = 24
		}
		pages.AddPage("secdetail", centered(tv, 72, height), true, true)
		app.SetFocus(tv)
		go func() {
			members := secretMembers(ctx, dcli, s)
			app.QueueUpdateDraw(func() {
				if pages.HasPage("secdetail") {
					render(members, false)
				}
			})
		}()
	}
	sectable.SetSelectedFunc(func(int, int) {
		if s, ok := selectedSecret(); ok {
			showSecretDetail(s)
		}
	})

	// ------------------------------------------------------------------ contexts
	// activeCtx is this session's effective docker context (what the UI is
	// connected to): the context the session was started with, else
	// $DOCKER_CONTEXT, else the stored current, else "default". Shown in the
	// footer and marked here.
	activeCtx := firstNonEmpty(ctxOverride, os.Getenv("DOCKER_CONTEXT"))
	if activeCtx == "" {
		activeCtx = dockerctx.Current()
	}
	if activeCtx == "" {
		activeCtx = "default"
	}
	cxtable := tview.NewTable().SetBorders(false).SetSelectable(true, false).SetFixed(1, 0)
	cxtable.SetSelectedStyle(selStyle)
	cxHeaders := []string{"CONTEXT", "DOCKER HOST"}
	var ctxs []dockerctx.Context
	renderContexts := func() {
		selName := ""
		if row, _ := cxtable.GetSelection(); row >= 1 {
			if c := cxtable.GetCell(row, 0); c != nil {
				selName = strings.TrimLeft(c.Text, "▶ ")
			}
		}
		cxtable.Clear()
		for c, h := range cxHeaders {
			cxtable.SetCell(0, c, headerCell(h))
		}
		selRow := 1
		for i, c := range ctxs {
			label, color := "  "+c.Name, tcell.ColorWhite
			if c.Name == activeCtx {
				label, color = "▶ "+c.Name, tcell.ColorAqua // the active session context
			}
			cxtable.SetCell(i+1, 0, tview.NewTableCell(label).SetTextColor(color).SetExpansion(1))
			cxtable.SetCell(i+1, 1, tview.NewTableCell(orDash(c.Host)).SetTextColor(tcell.ColorGray).SetExpansion(2))
			if c.Name == selName {
				selRow = i + 1
			}
		}
		if len(ctxs) > 0 {
			cxtable.Select(selRow, 0)
		}
	}
	// Contexts come from docker's local store (no network), so load synchronously.
	loadContexts := func() {
		list, err := dockerctx.List()
		if err != nil {
			cxtable.Clear()
			for c, h := range cxHeaders {
				cxtable.SetCell(0, c, headerCell(h))
			}
			cxtable.SetCell(1, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
			return
		}
		ctxs = list
		renderContexts()
	}
	selectedContext := func() (dockerctx.Context, bool) {
		row, _ := cxtable.GetSelection()
		i := row - 1
		if i < 0 || i >= len(ctxs) {
			return dockerctx.Context{}, false
		}
		return ctxs[i], true
	}
	// showCreateContext opens a form to add a docker context (name + endpoint).
	showCreateContext := func() {
		var name, host, desc string
		form := tview.NewForm()
		form.SetBorder(true).SetTitle(" new context ")
		form.AddInputField("Name", "", 32, nil, func(t string) { name = t })
		form.AddInputField("Docker host", "", 44, nil, func(t string) { host = t })
		form.AddInputField("Description", "", 44, nil, func(t string) { desc = t })
		if hf, ok := form.GetFormItem(1).(*tview.InputField); ok {
			hf.SetPlaceholder("ssh://ops@manager  |  tcp://host:2376")
		}
		_, restoreHelp := pushOverlayHelp(footerKeys("Tab", "next field", "Enter", "confirm", "Esc", "cancel"))
		closeForm := func() { restoreHelp(); pages.RemovePage("ctxform"); app.SetFocus(cxtable) }
		form.AddButton("Create", func() {
			if err := dockerctx.Create(strings.TrimSpace(name), strings.TrimSpace(host), strings.TrimSpace(desc)); err != nil {
				info("create failed: " + err.Error())
				return
			}
			closeForm()
			loadContexts()
			flash(" [green]created[white] context " + strings.TrimSpace(name))
		})
		form.AddButton("Cancel", closeForm)
		form.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			if ev.Key() == tcell.KeyEscape {
				closeForm()
				return nil
			}
			return ev
		})
		pages.AddPage("ctxform", centered(form, 66, 13), true, true)
		app.SetFocus(form)
	}
	// deleteContext removes the selected context behind a confirm. "default" is
	// protected; removing the current one resets the selection to default.
	deleteContext := func(c dockerctx.Context) {
		if c.Name == "default" {
			info("the built-in \"default\" context cannot be removed")
			return
		}
		force := c.Name == dockerctx.Current()
		msg := fmt.Sprintf("Remove context %q?\n%s", c.Name, orDash(c.Host))
		if force {
			msg += "\n\nIt is the current context — its selection resets to \"default\"."
		}
		m := tview.NewModal().SetText(msg).AddButtons([]string{"Delete", "Cancel"}).
			SetDoneFunc(func(_ int, label string) {
				pages.RemovePage("confirm")
				if label != "Delete" {
					app.SetFocus(cxtable)
					return
				}
				if err := dockerctx.Remove(c.Name, force); err != nil {
					info("remove failed: " + err.Error())
					return
				}
				loadContexts()
				flash(" [green]removed[white] context " + c.Name)
			})
		pages.AddPage("confirm", m, true, true)
		app.SetFocus(m)
	}
	// activateContext makes c the current docker context and restarts the UI so
	// it reconnects to that cluster. Restarting (rather than swapping the client
	// live) avoids racing the in-flight background loads.
	activateContext := func(c dockerctx.Context) {
		if c.Name == activeCtx {
			flash(" [gray]already on[white] context " + c.Name)
			return
		}
		if err := dockerctx.Use(c.Name); err != nil {
			info("switch failed: " + err.Error())
			return
		}
		switchTo = c.Name
		app.Stop() // the caller restarts against switchTo
	}

	// ---------------------------------------------------------------- tabs/chrome
	content.AddPage("containers", ctree, true, true)
	content.AddPage("volumes", vtable, true, false)
	content.AddPage("forwards", ftable, true, false)
	content.AddPage("networks", nettable, true, false)
	content.AddPage("secrets", sectable, true, false)
	content.AddPage("contexts", cxtable, true, false)

	tabBar := tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignCenter)
	// Two-line footer: the per-tab key hints on top, then one consolidated status
	// line — active context · live cluster summary · forward count · (on the
	// volumes tab) selection count. updateStatus() composes the status line; the
	// async cluster/forward refreshers feed it. clusterText holds the last cluster
	// probe result so a forward or selection change can recompose without re-probing.
	help := tview.NewTextView().SetDynamicColors(true)
	status := tview.NewTextView().SetDynamicColors(true)
	clusterText := "[gray]cluster: …[white]"
	footer := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(help, 1, 0, false).
		AddItem(status, 1, 0, false)
	// "/" search bar: hidden (height 0) until activated; filters the container
	// tree live by service / container id / node.
	search := tview.NewInputField().SetLabel("/ ").SetFieldWidth(0).
		SetPlaceholder("filter services / containers / nodes")
	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tabBar, 1, 0, false).
		AddItem(content, 0, 1, true).
		AddItem(search, 0, 0, false).
		AddItem(footer, 2, 0, false)
	pages.AddPage("main", root, true, true)

	// savedHelp holds the footer help to restore when search closes. While the
	// search field has focus the footer shows how to leave it — there is no
	// other on-screen hint, which is what made "how do I exit search?" a real
	// snag. (Not curHelp: that is declared further down, out of scope here.)
	var savedHelp string
	// searchMode selects what the shared "/" search bar filters: the container
	// tree ("containers") or the volumes table ("volumes").
	var searchMode string
	startSearch := func(mode string) {
		searchMode = mode
		root.ResizeItem(search, 1, 0)
		savedHelp = help.GetText(false)
		help.SetText(" [yellow]type[white] to filter   [yellow]Enter[white] keep filter & exit   [yellow]Esc[white] clear & exit")
		if mode == "volumes" {
			search.SetPlaceholder("filter volumes / driver / node")
			search.SetText(volFilter)
		} else {
			search.SetPlaceholder("filter services / containers / nodes")
			search.SetText(filter)
		}
		app.SetFocus(search)
	}
	search.SetChangedFunc(func(text string) {
		// Filter locally against the cached data — no docker call per keystroke.
		t := strings.TrimSpace(text)
		if searchMode == "volumes" {
			volFilter = t
			renderVolumeTable()
		} else {
			filter = t
			renderContainers()
		}
	})
	search.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEscape {
			search.SetText("") // clears the active filter via SetChangedFunc
		}
		isVol := searchMode == "volumes"
		empty := filter == ""
		if isVol {
			empty = volFilter == ""
		}
		if empty {
			root.ResizeItem(search, 0, 0) // nothing active — collapse the bar away
		}
		help.SetText(savedHelp) // restore the tab help
		if isVol {
			app.SetFocus(vtable) // Enter keeps the filter; the bar stays as an indicator
		} else {
			app.SetFocus(ctree)
		}
	})

	// refreshCluster probes the swarm in the background and updates the footer
	// summary. The agent probe (a Version RPC per node) can be slow, so it runs
	// off the UI goroutine and pushes the result back via QueueUpdateDraw.
	refreshCluster := func() {
		go func() {
			nodes, err := r.Nodes(ctx)
			if err != nil {
				app.QueueUpdateDraw(func() {
					clusterText = "[red]cluster: unreachable[white]"
					if updateStatus != nil {
						updateStatus()
					}
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
				clusterText = fmt.Sprintf("[aqua]%d[white] nodes · [aqua]%d[white]/%d agents", len(nodes), agents, len(nodes))
				if updateStatus != nil {
					updateStatus()
				}
			})
		}()
	}

	// refreshForwardViews repaints everything a forward's state feeds: the
	// footer counter, the forwards table, and the tree annotations. Must run on
	// the UI goroutine.
	refreshForwardViews = func() {
		if updateStatus != nil {
			updateStatus()
		}
		renderForwards()
		renderContainers()
	}

	active := "containers"
	mouseEnabled = true
	var screen tcell.Screen // set just before Run; used for clipboard (OSC52)
	curHelp := ""
	// updateStatus composes the footer status line from the active context, the
	// last cluster probe, the forward count and (on the volumes tab) the number
	// of selected volumes. Assigned here — after `active` exists — and called by
	// the refreshers, setTab and the volume-selection toggle.
	updateStatus = func() {
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
	// helpFor builds the footer key hints from the live keymap, so remapped keys
	// show correctly. j/k, Enter and Tab/1-6 are fixed and stay literal.
	helpFor := func(name string) string {
		kl := keyLabel
		tail := fmt.Sprintf("[yellow]%s[white] copy  [yellow]%s[white] mouse  [yellow]Tab/1-6[white] tabs  [yellow]%s[white] refresh  [yellow]%s[white] quit",
			kl(km.Copy), kl(km.ToggleMouse), kl(km.Refresh), kl(km.Quit))
		switch name {
		case "containers":
			return fmt.Sprintf(" [yellow]j/k[white] up/down  [yellow]%s/%s[white] fold  [yellow]%s[white] search  [yellow]Enter[white] menu  [yellow]%s[white] forward  %s",
				kl(km.Fold), kl(km.Unfold), kl(km.Search), kl(km.Forward), tail)
		case "volumes":
			return fmt.Sprintf(" [yellow]j/k[white] up/down  [yellow]%s[white] search  [yellow]%s[white] select  [yellow]%s[white] all  [yellow]%s[white] attach  [yellow]%s[white] delete  [yellow]%s[white] prune  [yellow]Enter[white] nodes  [yellow]%s[white] used by  [yellow]%s[white] sort  %s",
				kl(km.Search), kl(km.VolSelect), kl(km.VolSelectAll), kl(km.VolAttach), kl(km.VolDelete), kl(km.VolPrune), kl(km.VolUsedBy), kl(km.VolSort), tail)
		case "networks":
			return fmt.Sprintf(" [yellow]j/k[white] up/down  [yellow]Enter/%s[white] attached  %s", kl(km.NetAttached), tail)
		case "secrets":
			return fmt.Sprintf(" [yellow]j/k[white] up/down  [yellow]Enter[white] details  %s", tail)
		case "contexts":
			return fmt.Sprintf(" [yellow]j/k[white] up/down  [yellow]%s[white] use  [yellow]%s[white] new  [yellow]%s[white] delete  %s",
				kl(km.CtxUse), kl(km.CtxNew), kl(km.CtxDelete), tail)
		default:
			return fmt.Sprintf(" [yellow]j/k[white] up/down  [yellow]Enter[white] details  [yellow]%s[white] stop  [yellow]%s[white] copy url  %s",
				kl(km.FwdStop), kl(km.FwdCopyURL), tail)
		}
	}
	// tabChrome renders the tab bar with one tab highlighted, so adding a tab is
	// a single list entry instead of five hand-aligned strings.
	tabList := []struct{ key, label string }{
		{"containers", "Containers (1)"},
		{"volumes", "Volumes (2)"},
		{"forwards", "Forwards (3)"},
		{"networks", "Networks (4)"},
		{"secrets", "Secrets (5)"},
		{"contexts", "Contexts (6)"},
	}
	renderTabBar := func(active string) {
		var b strings.Builder
		for _, t := range tabList {
			if t.key == active {
				fmt.Fprintf(&b, " [black:teal] %s [-:-]  ", t.label)
			} else {
				fmt.Fprintf(&b, " %s  ", t.label)
			}
		}
		tabBar.SetText(b.String())
	}
	setTab := func(name string) {
		active = name
		content.SwitchToPage(name)
		curHelp = helpFor(name)
		help.SetText(curHelp)
		renderTabBar(name)
		updateStatus() // the selection count shows only on the volumes tab
		switch name {
		case "containers":
			app.SetFocus(ctree)
		case "volumes":
			app.SetFocus(vtable)
			loadVolumes()
		case "forwards":
			app.SetFocus(ftable)
			renderForwards()
		case "networks":
			app.SetFocus(nettable)
			loadNetworks()
		case "secrets":
			app.SetFocus(sectable)
			loadSecrets()
		case "contexts":
			app.SetFocus(cxtable)
			loadContexts()
		}
	}

	// flash briefly replaces the footer with a status message, then restores it.
	flash = func(msg string) {
		help.SetText(msg)
		go func() {
			time.Sleep(1500 * time.Millisecond)
			app.QueueUpdateDraw(func() { help.SetText(curHelp) })
		}()
	}

	// pushOverlayHelp points the single bottom footer at an overlay's keys. It
	// saves the current footer text and returns a setter (to update while open)
	// and a restore (to call on close). Because each call captures the then-
	// current text, nested overlays restore correctly. It also tracks how many
	// overlays are open (overlayDepth) so the background tree refresh can pause —
	// otherwise its periodic renderContainers on the UI goroutine competes with
	// keystrokes in an overlay and makes them feel laggy.
	pushOverlayHelp = func(markup string) (func(string), func()) {
		prev := help.GetText(false)
		help.SetText(markup)
		overlayDepth.Add(1)
		var once sync.Once
		restore := func() {
			once.Do(func() {
				overlayDepth.Add(-1)
				help.SetText(prev)
			})
		}
		return func(m string) { help.SetText(m) }, restore
	}

	// yankCurrent copies the active tab's list to the system clipboard via the
	// terminal (OSC52), so it also works over ssh when the terminal supports it.
	yankCurrent := func() {
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
		flash(" [green]✓ copied to clipboard[white]")
	}

	// toggleMouse flips tview's mouse capture. With it off, the terminal's own
	// text selection / copy works again (tview otherwise grabs the mouse).
	toggleMouse = func() {
		mouseEnabled = !mouseEnabled
		app.EnableMouse(mouseEnabled)
		if mouseEnabled {
			flash(" [green]mouse ON[white] — app handles the mouse")
		} else {
			flash(" [green]mouse OFF[white] — select & copy with your terminal (m to re-enable)")
		}
	}

	// Toggleable log viewer: an overlay showing the in-memory log ring, refreshed
	// live while open. Opened/closed with the backtick key from any tab.
	var (
		logViewStop    chan struct{}
		logViewPrev    tview.Primitive
		logViewRestore func()
	)
	closeLogView := func() {
		if logViewStop != nil {
			close(logViewStop)
			logViewStop = nil
		}
		if logViewRestore != nil {
			logViewRestore()
			logViewRestore = nil
		}
		pages.RemovePage("logview")
		if logViewPrev != nil {
			app.SetFocus(logViewPrev)
		}
	}
	openLogView := func() {
		logViewPrev = app.GetFocus()
		_, logViewRestore = pushOverlayHelp(footerKeys("`", "toggle/close", "Esc/q", "close", "↑/↓", "scroll"))
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
				closeLogView()
				return nil
			}
			return ev
		})
		stop := make(chan struct{})
		logViewStop = stop
		pages.AddPage("logview", tv, true, true)
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
	toggleLogView := func() {
		if pages.HasPage("logview") {
			closeLogView()
		} else {
			openLogView()
		}
	}

	// tabOrder drives Tab cycling; every tab joins it.
	tabOrder := []string{"containers", "volumes", "forwards", "networks", "secrets", "contexts"}
	tabKeys := func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune && ev.Rune() == '`' {
			toggleLogView()
			return nil
		}
		if ev.Key() == tcell.KeyTab {
			for i, name := range tabOrder {
				if name == active {
					setTab(tabOrder[(i+1)%len(tabOrder)])
					break
				}
			}
			return nil
		}
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case '1':
				setTab("containers")
				return nil
			case '2':
				setTab("volumes")
				return nil
			case '3':
				setTab("forwards")
				return nil
			case '4':
				setTab("networks")
				return nil
			case '5':
				setTab("secrets")
				return nil
			case '6':
				setTab("contexts")
				return nil
			case km.Quit:
				app.Stop()
				return nil
			case km.Refresh:
				switch active {
				case "containers":
					loadContainers()
				case "volumes":
					loadVolumes()
				case "networks":
					loadNetworks()
				case "secrets":
					loadSecrets()
				case "contexts":
					loadContexts()
				default:
					renderForwards()
				}
				refreshCluster()
				return nil
			case km.Copy:
				yankCurrent()
				return nil
			case km.ToggleMouse:
				toggleMouse()
				return nil
			case 'j':
				return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
			case 'k':
				return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
			}
		}
		return ev
	}
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
	editList := func(title, applyVerb string, items []string, validate func(string) (string, error), onApply func([]string) error, suggest func(string) []string, allowEdit, multiline bool, confirmNote func([]string) string, entryAction *listEntryAction, formPrompt func(initial string, submit func(raw string) error, cancel func()) tview.Primitive, back tview.Primitive, after func()) {
		cur := append([]string{}, items...)
		list := tview.NewList().ShowSecondaryText(false)
		list.SetBorder(true).SetTitle(fmt.Sprintf(" %s ", title))
		keyPairs := []string{"a", "add"}
		if allowEdit {
			keyPairs = append(keyPairs, "e", "edit")
		}
		keyPairs = append(keyPairs, "d", "delete", "y", "copy")
		if entryAction != nil {
			keyPairs = append(keyPairs, string(entryAction.key), entryAction.label)
		}
		keyPairs = append(keyPairs, "u", "undo", "w", "apply", "j/k", "move", "Esc", "cancel")
		// history stacks snapshots of cur before each staged change, so `u` undoes
		// one step (add/edit/delete) as long as nothing has been applied yet.
		var history [][]string
		render := func() {
			idx := list.GetCurrentItem()
			list.Clear()
			if len(cur) == 0 && len(items) == 0 {
				list.AddItem("(none)", "", 0, nil)
			} else {
				// Highlight staged (not-yet-applied) changes: an entry not present in
				// the original set is added/edited (green); originals missing from cur
				// are shown as dim-red "removed" ghosts. Diff is a multiset so it
				// survives undo without any parallel bookkeeping.
				origCount := map[string]int{}
				for _, it := range items {
					origCount[it]++
				}
				for _, it := range cur {
					label := tview.Escape(it)
					if origCount[it] > 0 {
						origCount[it]-- // unchanged — matches an original
					} else {
						label = "[green::b]" + label + " (new)[-:-:-]"
					}
					list.AddItem(label, "", 0, nil)
				}
				// origCount now holds the leftovers = removed items; show them (in the
				// original order) after the live rows. They sit beyond len(cur), so the
				// e/d/y handlers (which guard i<len(cur)) treat them as inert.
				shown := map[string]int{}
				for _, it := range items {
					if shown[it] < origCount[it] {
						shown[it]++
						list.AddItem("[red::d]- "+tview.Escape(it)+" (removed)[-:-:-]", "", 0, nil)
					}
				}
			}
			if idx >= 0 && idx < list.GetItemCount() {
				list.SetCurrentItem(idx)
			}
		}
		render()
		snapshot := func() { history = append(history, append([]string{}, cur...)) }
		undo := func() {
			if len(history) == 0 {
				return
			}
			cur = history[len(history)-1]
			history = history[:len(history)-1]
			render()
		}
		setHelp, restoreHelp := pushOverlayHelp(footerKeys(keyPairs...))
		closeEd := func() { restoreHelp(); pages.RemovePage("listedit"); app.SetFocus(back) }
		// commit validates a typed entry and applies it via done.
		commit := func(raw string, done func(string)) {
			txt := strings.TrimSpace(raw)
			if txt == "" {
				return
			}
			norm, err := validate(txt)
			if err != nil {
				info(err.Error())
				return
			}
			done(norm)
			render()
		}
		prompt := func(label, initial string, done func(string)) {
			if formPrompt != nil {
				// Custom form: submit validates+stages+closes on success; the form
				// keeps focus on failure (it surfaces the error itself).
				cancel := func() { pages.RemovePage("listeditprompt"); app.SetFocus(list) }
				submit := func(raw string) error {
					norm, err := validate(strings.TrimSpace(raw))
					if err != nil {
						return err
					}
					done(norm)
					render()
					pages.RemovePage("listeditprompt")
					app.SetFocus(list)
					return nil
				}
				form := formPrompt(initial, submit, cancel)
				pages.AddPage("listeditprompt", centered(form, 78, 15), true, true)
				app.SetFocus(form)
				return
			}
			if multiline {
				// Long / multi-line values (e.g. GITLAB_OMNIBUS_CONFIG) are painful
				// in a one-line field, so edit them in a scrollable TextArea. Place
				// the cursor at the START (not the end): cursor-at-end scrolls a
				// freshly-built, not-yet-sized TextArea to its bottom, so the operator
				// only sees the tail of the value with the cursor pinned to the last
				// row. Start-of-text shows the value from the top.
				ta := tview.NewTextArea().SetText(initial, false)
				ta.SetBorder(true).SetTitle(" " + label + " — Ctrl-S save · Esc cancel ")
				ta.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
					switch ev.Key() {
					case tcell.KeyEscape:
						pages.RemovePage("listeditprompt")
						app.SetFocus(list)
						return nil
					case tcell.KeyCtrlS:
						txt := ta.GetText()
						pages.RemovePage("listeditprompt")
						app.SetFocus(list)
						commit(txt, done)
						return nil
					}
					return ev
				})
				pages.AddPage("listeditprompt", centered(ta, 96, 22), true, true)
				app.SetFocus(ta)
				return
			}
			in := tview.NewInputField().SetLabel(label).SetText(initial).SetFieldWidth(40)
			if suggest != nil {
				in.SetAutocompleteFunc(suggest)
			}
			in.SetDoneFunc(func(k tcell.Key) {
				pages.RemovePage("listeditprompt")
				app.SetFocus(list)
				if k != tcell.KeyEnter {
					return
				}
				commit(in.GetText(), done)
			})
			in.SetBorder(true)
			pages.AddPage("listeditprompt", centered(in, 60, 3), true, true)
			app.SetFocus(in)
		}
		// hasChanges reports whether anything is staged but not yet applied (cur
		// differs from the original set — order included; j/k only navigate here).
		hasChanges := func() bool {
			if len(cur) != len(items) {
				return true
			}
			for i := range cur {
				if cur[i] != items[i] {
					return true
				}
			}
			return false
		}
		// applyFlow runs the rolling-update confirmation and, on Apply, commits cur.
		// Shared by `w` and the "unapplied changes" reminder shown when leaving.
		applyFlow := func() {
			text := fmt.Sprintf("%s?\n\nThis triggers a rolling update of the service.", applyVerb)
			if confirmNote != nil {
				if note := confirmNote(cur); note != "" {
					text = note + "\n\n" + text
				}
			}
			confirm := tview.NewModal().
				SetText(text).
				AddButtons([]string{"Apply", "Cancel"}).
				SetDoneFunc(func(_ int, lbl string) {
					pages.RemovePage("listeditconfirm")
					if lbl != "Apply" {
						app.SetFocus(list)
						return
					}
					go func() {
						err := onApply(cur)
						app.QueueUpdateDraw(func() {
							if err != nil {
								info("update failed: " + err.Error())
								app.SetFocus(list)
								return
							}
							restoreHelp()
							pages.RemovePage("listedit")
							info("service updated — rolling update started")
							if after != nil {
								after()
							}
						})
					}()
				})
			pages.AddPage("listeditconfirm", confirm, true, true)
			app.SetFocus(confirm)
		}
		list.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape:
				if hasChanges() {
					// Don't silently drop staged edits — make the operator choose.
					leave := tview.NewModal().
						SetText("You have unapplied changes.\n\nApply them now, discard them, or keep editing?\n(Esc discards and leaves.)").
						AddButtons([]string{"Apply", "Discard", "Keep editing"}).
						SetDoneFunc(func(_ int, lbl string) {
							pages.RemovePage("listeditleave")
							switch lbl {
							case "Apply":
								applyFlow()
							case "Keep editing":
								app.SetFocus(list)
							default:
								// "Discard" button or Esc (empty label): leave the
								// editor, dropping the staged (never-applied) edits.
								// Esc must be an exit here — mapping it to "keep
								// editing" traps the operator, since Esc on the list
								// only re-opens this same guard, forever.
								closeEd()
							}
						})
					pages.AddPage("listeditleave", leave, true, true)
					app.SetFocus(leave)
					return nil
				}
				closeEd()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'a':
				prompt("add: ", "", func(n string) { snapshot(); cur = append(cur, n) })
				return nil
			case allowEdit && ev.Key() == tcell.KeyRune && ev.Rune() == 'e':
				if i := list.GetCurrentItem(); i >= 0 && i < len(cur) {
					prompt("edit: ", cur[i], func(n string) { snapshot(); cur[i] = n })
				}
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
				if i := list.GetCurrentItem(); i >= 0 && i < len(cur) {
					snapshot()
					cur = append(cur[:i], cur[i+1:]...)
					render()
				}
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'y':
				if i := list.GetCurrentItem(); i >= 0 && i < len(cur) && screen != nil {
					screen.SetClipboard([]byte(cur[i]))
					setHelp(" [green]✓ copied[white] · " + footerKeys(keyPairs...))
				}
				return nil
			case entryAction != nil && ev.Key() == tcell.KeyRune && ev.Rune() == entryAction.key:
				if i := list.GetCurrentItem(); i >= 0 && i < len(cur) {
					entry := cur[i]
					closeEd()
					entryAction.run(entry)
				}
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'u':
				undo()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'w':
				applyFlow()
				return nil
			}
			return vimListKeys(ev)
		})
		pages.AddPage("listedit", centered(list, 72, 18), true, true)
		app.SetFocus(list)
	}
	// openPortsEditor / openLabelsEditor fetch the service's current ports/labels
	// off the UI goroutine, then open the staged editor.
	openPortsEditor := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			items, err := currentServicePorts(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load ports: " + err.Error())
					return
				}
				editList("ports of "+svcName, "Update the published ports", items,
					func(s string) (string, error) {
						p, e := parseServicePort(s)
						if e != nil {
							return "", e
						}
						return formatServicePort(p), nil
					},
					func(list []string) error {
						ports, e := portsFromStrings(list)
						if e != nil {
							return e
						}
						return setServicePorts(ctx, dcli, svcName, ports)
					}, nil, true, false, nil, nil, nil, back, after)
			})
		}()
	}
	openLabelsEditor := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			items, err := currentServiceLabels(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load labels: " + err.Error())
					return
				}
				editList("labels of "+svcName, "Update the labels", items,
					func(s string) (string, error) {
						k, v, e := parseLabel(s)
						if e != nil {
							return "", e
						}
						return k + "=" + v, nil
					},
					func(list []string) error {
						labels, e := labelsFromStrings(list)
						if e != nil {
							return e
						}
						return setServiceLabels(ctx, dcli, svcName, labels)
					}, nil, true, false, nil, nil, nil, back, after)
			})
		}()
	}
	// openAliasEditorForNet edits a service's DNS aliases on one network. Reached
	// from the networks editor (select a network, press A), so aliases only show
	// in that context, not as a top-level inspect key.
	openAliasEditorForNet := func(svcName, netName, target string, back tview.Primitive, after func()) {
		go func() {
			att, err := serviceAttachedNetworks(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load aliases: " + err.Error())
					return
				}
				var aliases []string
				found := false
				for _, a := range att {
					if a.Target == target || a.Name == netName {
						aliases = a.Aliases
						found = true
					}
				}
				if !found {
					info(fmt.Sprintf("service %q is not attached to network %q yet — apply the network first, then set aliases", svcName, netName))
					return
				}
				editList("aliases of "+svcName+" on "+netName, "Update the aliases", aliases,
					func(s string) (string, error) {
						s = strings.TrimSpace(s)
						if s == "" {
							return "", fmt.Errorf("alias must not be empty")
						}
						return s, nil
					},
					func(list []string) error { return setNetworkAliases(ctx, dcli, svcName, target, list) },
					nil, true, false, nil, nil, nil, back, after)
			})
		}()
	}
	// openNetworksEditor edits the networks a service is attached to, with
	// autocomplete of network names (add/remove), applied in one ServiceUpdate.
	// Selecting a network and pressing A edits that network's DNS aliases.
	openNetworksEditor := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			idByName, idToName, allNames := listNetworkRefs(ctx, dcli)
			current, err := currentServiceNetworks(ctx, dcli, svcName, idToName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load networks: " + err.Error())
					return
				}
				attached := map[string]bool{}
				for _, c := range current {
					attached[c] = true
				}
				editList("networks of "+svcName, "Update the networks", current,
					func(s string) (string, error) {
						if _, ok := idByName[s]; ok {
							return s, nil // known name
						}
						if n := idToName[s]; n != "" {
							return n, nil // an id → show its name
						}
						return "", fmt.Errorf("unknown network %q", s)
					},
					func(list []string) error {
						ids := make([]string, 0, len(list))
						for _, name := range list {
							id := idByName[name]
							if id == "" {
								id = name // already an id
							}
							ids = append(ids, id)
						}
						return setServiceNetworks(ctx, dcli, svcName, ids)
					},
					func(text string) []string {
						text = strings.ToLower(strings.TrimSpace(text))
						var out []string
						for _, n := range allNames {
							if attached[n] {
								continue
							}
							if text == "" || strings.Contains(strings.ToLower(n), text) {
								out = append(out, n)
							}
						}
						return out
					}, false, false, nil, &listEntryAction{
						key:   'A',
						label: "aliases",
						run:   func(net string) { openAliasEditorForNet(svcName, net, idByName[net], back, after) },
					}, nil, back, after)
			})
		}()
	}
	// openSecretsEditor edits the secrets a service references, with autocomplete
	// of secret names (add/remove), applied in one ServiceUpdate. Works even when
	// the service has none yet.
	openSecretsEditor := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			all := secretNames(ctx, dcli)
			current, err := currentServiceSecrets(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load secrets: " + err.Error())
					return
				}
				known := map[string]bool{}
				for _, n := range all {
					known[n] = true
				}
				attached := map[string]bool{}
				for _, c := range current {
					attached[c] = true
				}
				editList("secrets of "+svcName, "Update the secrets", current,
					func(s string) (string, error) {
						if known[s] {
							return s, nil
						}
						return "", fmt.Errorf("unknown secret %q", s)
					},
					func(list []string) error { return setServiceSecrets(ctx, dcli, svcName, list) },
					func(text string) []string {
						text = strings.ToLower(strings.TrimSpace(text))
						var out []string
						for _, n := range all {
							if attached[n] {
								continue
							}
							if text == "" || strings.Contains(strings.ToLower(n), text) {
								out = append(out, n)
							}
						}
						return out
					}, false, false, nil, nil, nil, back, after)
			})
		}()
	}
	// openMountsEditor edits a service's mounts (volumes + binds, with a
	// read-only flag). For bind mounts it can't verify the host path (no host
	// access), so on apply it warns which nodes the service could run on and that
	// each bind source must already exist on all of them.
	// openEnvEditor edits a service's environment variables (KEY=VALUE), with
	// edit allowed (adjust a value in place), add and remove.
	openEnvEditor := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			items, err := currentServiceEnv(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load env: " + err.Error())
					return
				}
				editList("env of "+svcName, "Update the environment", items,
					func(s string) (string, error) {
						k, v, e := parseEnv(s)
						if e != nil {
							return "", e
						}
						return k + "=" + v, nil
					},
					func(list []string) error {
						env, e := envFromStrings(list)
						if e != nil {
							return e
						}
						return setServiceEnv(ctx, dcli, svcName, env)
					}, nil, true, true, nil, nil, nil, back, after)
			})
		}()
	}
	// placementListEditor opens a staged list editor whose add/edit input
	// autocompletes cluster-derived candidates (excluding ones already staged).
	// Shared by the constraints and the spread-preferences editors.
	placementListEditor := func(title, applyVerb string, items, cand []string, validate func(string) (string, error), apply func([]string) error, back tview.Primitive, after func()) {
		have := map[string]bool{}
		for _, it := range items {
			have[it] = true
		}
		editList(title, applyVerb, items, validate, apply,
			func(text string) []string {
				text = strings.ToLower(strings.TrimSpace(text))
				var out []string
				for _, c := range cand {
					if have[c] {
						continue
					}
					if text == "" || strings.Contains(strings.ToLower(c), text) {
						out = append(out, c)
					}
				}
				return out
			}, true, false, nil, nil, nil, back, after)
	}
	// openPlacementConstraintsEditor edits the service's hard placement
	// constraints (node.* / engine.* == / !=) with node-derived autocomplete.
	openPlacementConstraintsEditor := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			items, err := currentServicePlacement(ctx, dcli, svcName)
			cand, cerr := placementSuggestions(ctx, dcli)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load placement: " + err.Error())
					return
				}
				if cerr != nil {
					cand = nil // autocomplete is best-effort
				}
				placementListEditor("constraints of "+svcName, "Update the placement constraints",
					items, cand, validatePlacementConstraint,
					func(list []string) error { return setServicePlacement(ctx, dcli, svcName, list) },
					back, after)
			})
		}()
	}
	// openSpreadEditor edits the service's spread placement preferences — bare
	// node attributes (e.g. node.labels.zone) that Swarm spreads tasks over — in
	// priority order, with node-derived autocomplete.
	openSpreadEditor := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			items, err := currentServiceSpread(ctx, dcli, svcName)
			cand, cerr := spreadSuggestions(ctx, dcli)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load spread preferences: " + err.Error())
					return
				}
				if cerr != nil {
					cand = nil
				}
				placementListEditor("spread of "+svcName, "Update the spread preferences",
					items, cand, validateSpreadDescriptor,
					func(list []string) error { return setServiceSpread(ctx, dcli, svcName, list) },
					back, after)
			})
		}()
	}
	// openPlacementMenu groups the two placement concerns (hard constraints and
	// soft spread preferences) under one key so the inspect footer stays short.
	openPlacementMenu := func(svcName string, back tview.Primitive, after func()) {
		m := tview.NewModal().
			SetText("Edit placement for " + svcName).
			AddButtons([]string{"Constraints", "Spread preferences", "Cancel"}).
			SetDoneFunc(func(_ int, lbl string) {
				pages.RemovePage("placementmenu")
				switch lbl {
				case "Constraints":
					openPlacementConstraintsEditor(svcName, back, after)
				case "Spread preferences":
					openSpreadEditor(svcName, back, after)
				default:
					app.SetFocus(back)
				}
			})
		pages.AddPage("placementmenu", m, true, true)
		app.SetFocus(m)
	}
	openMountsEditor := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			items, err := currentServiceMountSpecs(ctx, dcli, svcName)
			nodes, uneval, nerr := candidateNodesForService(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load mounts: " + err.Error())
					return
				}
				confirmNote := func(list []string) string {
					var binds []string
					for _, it := range list {
						if m, e := parseServiceMount(it); e == nil && m.Type == mount.TypeBind {
							binds = append(binds, m.Source)
						}
					}
					if len(binds) == 0 {
						return ""
					}
					where := "(could not determine nodes)"
					if nerr == nil {
						where = fmt.Sprintf("%d node(s): %s", len(nodes), strings.Join(nodes, ", "))
					}
					msg := fmt.Sprintf("⚠ bind sources must ALREADY exist on every node this service can run on — swarmexec cannot verify this.\nnodes: %s\nbinds: %s", where, strings.Join(binds, ", "))
					if nerr == nil && len(uneval) > 0 {
						msg += fmt.Sprintf("\n(unevaluated constraints: %s — the real node set may be narrower)", strings.Join(uneval, ", "))
					}
					return msg
				}
				// volNames feeds the volume-mode autocomplete. Seed from whatever the
				// Volumes tab already loaded, then refresh in the background (volume
				// listing is a per-node agent fan-out, so we don't block opening).
				volNames := make([]string, 0, len(vols))
				for _, v := range vols {
					volNames = append(volNames, v.Name)
				}
				go func() {
					vnodes, e := r.Nodes(ctx)
					if e != nil {
						return
					}
					vs, _ := indexVolumes(ctx, cfg, vnodes, f.connectTimeout)
					names := make([]string, 0, len(vs))
					for _, v := range vs {
						names = append(names, v.Name)
					}
					sort.Strings(names)
					app.QueueUpdateDraw(func() { volNames = names })
				}()
				// mountForm splits a mount into separate inputs: a "bind mount" toggle,
				// a source (host path when bind, else a volume name with autocomplete),
				// the container path, and a read-only toggle. It replaces the old single
				// "type:source:target[:ro]" text entry.
				mountForm := func(initial string, submit func(raw string) error, cancel func()) tview.Primitive {
					isBind, src, tgt, ro := false, "", "", false
					if m, e := parseServiceMount(initial); e == nil {
						isBind, src, tgt, ro = m.Type == mount.TypeBind, m.Source, m.Target, m.ReadOnly
					}
					form := tview.NewForm()
					srcField := tview.NewInputField().SetText(src).SetFieldWidth(48)
					tgtField := tview.NewInputField().SetLabel("container path").SetText(tgt).SetFieldWidth(48).SetPlaceholder("/data")
					roCheck := tview.NewCheckbox().SetLabel("read-only").SetChecked(ro)
					volSuggest := func(text string) []string {
						text = strings.ToLower(strings.TrimSpace(text))
						var out []string
						for _, n := range volNames {
							if text == "" || strings.Contains(strings.ToLower(n), text) {
								out = append(out, n)
							}
						}
						return out
					}
					applyMode := func(bind bool) {
						if bind {
							srcField.SetLabel("host path").SetPlaceholder("/opt/app/config").SetAutocompleteFunc(nil)
						} else {
							srcField.SetLabel("volume").SetPlaceholder("volume name").SetAutocompleteFunc(volSuggest)
						}
					}
					bindCheck := tview.NewCheckbox().SetLabel("bind mount").SetChecked(isBind).
						SetChangedFunc(func(checked bool) { applyMode(checked) })
					applyMode(isBind)
					setTitle := func(t string) { form.SetTitle(tview.Escape(t)) }
					save := func() {
						typ := "volume"
						if bindCheck.IsChecked() {
							typ = "bind"
						}
						s := strings.TrimSpace(srcField.GetText())
						t := strings.TrimSpace(tgtField.GetText())
						if s == "" || t == "" {
							setTitle(" ⚠ source and container path are required ")
							return
						}
						raw := typ + ":" + s + ":" + t
						if roCheck.IsChecked() {
							raw += ":ro"
						}
						if e := submit(raw); e != nil {
							setTitle(" ⚠ " + e.Error() + " ")
						}
					}
					form.AddFormItem(bindCheck)
					form.AddFormItem(srcField)
					form.AddFormItem(tgtField)
					form.AddFormItem(roCheck)
					form.AddButton("Save", save)
					form.AddButton("Cancel", cancel)
					form.SetCancelFunc(cancel)
					form.SetBorder(true)
					if initial == "" {
						setTitle(" add mount · Tab moves · Enter on Save ")
					} else {
						setTitle(" edit mount · Tab moves · Enter on Save ")
					}
					return form
				}
				editList("mounts of "+svcName, "Update the mounts", items,
					func(s string) (string, error) {
						m, e := parseServiceMount(s)
						if e != nil {
							return "", e
						}
						return formatServiceMount(m), nil
					},
					func(list []string) error {
						ms, e := mountsFromStrings(list)
						if e != nil {
							return e
						}
						return setServiceMounts(ctx, dcli, svcName, ms)
					}, nil, true, false, confirmNote, nil, mountForm, back, after)
			})
		}()
	}
	// openScalePrompt asks for a new replica count and scales the service.
	openScalePrompt := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			cur, replicated, err := currentServiceReplicas(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load service: " + err.Error())
					return
				}
				if !replicated {
					info(fmt.Sprintf("service %q is not replicated and cannot be scaled", svcName))
					return
				}
				in := tview.NewInputField().SetLabel("replicas: ").SetText(fmt.Sprintf("%d", cur)).
					SetFieldWidth(8).SetAcceptanceFunc(tview.InputFieldInteger)
				in.SetDoneFunc(func(k tcell.Key) {
					pages.RemovePage("scaleprompt")
					app.SetFocus(back)
					if k != tcell.KeyEnter {
						return
					}
					n, perr := strconv.ParseUint(strings.TrimSpace(in.GetText()), 10, 64)
					if perr != nil {
						info("invalid replica count")
						return
					}
					go func() {
						serr := scaleService(ctx, dcli, svcName, n)
						app.QueueUpdateDraw(func() {
							if serr != nil {
								info("scale failed: " + serr.Error())
								return
							}
							info(fmt.Sprintf("scaled %q to %d — reconciling", svcName, n))
							if after != nil {
								after()
							}
						})
					}()
				})
				in.SetBorder(true).SetTitle(" scale service ")
				pages.AddPage("scaleprompt", centered(in, 50, 3), true, true)
				app.SetFocus(in)
			})
		}()
	}
	// openResourcesEditor sets/edits/clears the service's CPU and memory limits
	// (and reservations) in a form. An empty field clears that limit; applying
	// does one ServiceUpdate (rolling update).
	openResourcesEditor := func(svcName string, back tview.Primitive, after func()) {
		go func() {
			rc, err := currentServiceResources(ctx, dcli, svcName)
			app.QueueUpdateDraw(func() {
				if err != nil {
					info("cannot load resources: " + err.Error())
					return
				}
				form := tview.NewForm()
				cpuL := tview.NewInputField().SetLabel("CPU limit (cores)").SetText(rc.CPULimit).SetFieldWidth(16).SetPlaceholder("e.g. 0.5 — empty = none")
				memL := tview.NewInputField().SetLabel("Memory limit").SetText(rc.MemLimit).SetFieldWidth(16).SetPlaceholder("e.g. 512m — empty = none")
				cpuR := tview.NewInputField().SetLabel("CPU reservation").SetText(rc.CPUReservation).SetFieldWidth(16).SetPlaceholder("optional")
				memR := tview.NewInputField().SetLabel("Memory reservation").SetText(rc.MemReservation).SetFieldWidth(16).SetPlaceholder("optional")
				setTitle := func(t string) { form.SetTitle(tview.Escape(t)) }
				_, restoreHelp := pushOverlayHelp(footerKeys("Tab", "move", "Enter", "button", "Esc", "cancel"))
				closeForm := func() {
					restoreHelp()
					pages.RemovePage("resedit")
					app.SetFocus(back)
				}
				apply := func() {
					cl, e1 := parseCPUCores(cpuL.GetText())
					ml, e2 := parseMemBytes(memL.GetText())
					cr, e3 := parseCPUCores(cpuR.GetText())
					mr, e4 := parseMemBytes(memR.GetText())
					for _, e := range []error{e1, e2, e3, e4} {
						if e != nil {
							setTitle(" ⚠ " + e.Error() + " ")
							return
						}
					}
					if cl > 0 && cr > cl {
						setTitle(" ⚠ CPU reservation exceeds the limit ")
						return
					}
					if ml > 0 && mr > ml {
						setTitle(" ⚠ memory reservation exceeds the limit ")
						return
					}
					confirm := tview.NewModal().
						SetText("Update resource limits for " + svcName + "?\n\nThis triggers a rolling update of the service.").
						AddButtons([]string{"Apply", "Cancel"}).
						SetDoneFunc(func(_ int, lbl string) {
							pages.RemovePage("resconfirm")
							if lbl != "Apply" {
								app.SetFocus(form)
								return
							}
							go func() {
								aerr := setServiceResources(ctx, dcli, svcName, cl, ml, cr, mr)
								app.QueueUpdateDraw(func() {
									if aerr != nil {
										info("update failed: " + aerr.Error())
										app.SetFocus(form)
										return
									}
									closeForm()
									info("service updated — rolling update started")
									if after != nil {
										after()
									}
								})
							}()
						})
					pages.AddPage("resconfirm", confirm, true, true)
					app.SetFocus(confirm)
				}
				form.AddFormItem(cpuL)
				form.AddFormItem(memL)
				form.AddFormItem(cpuR)
				form.AddFormItem(memR)
				form.AddButton("Apply", apply)
				form.AddButton("Cancel", closeForm)
				form.SetCancelFunc(closeForm)
				form.SetBorder(true)
				setTitle(" resource limits of " + svcName + " — empty clears a limit ")
				pages.AddPage("resedit", centered(form, 70, 15), true, true)
				app.SetFocus(form)
			})
		}()
	}

	showInspect := func(title string, op string, editSvc string, fetch func() ([]inspLine, string, error)) {
		table := tview.NewTable().SetSelectable(true, false)
		table.SetBorder(true)
		var lines []inspLine
		var rawJSON string
		var plain []string            // plain text of each current row, for copy
		rowNet := map[int]string{}    // table row -> network name, for collapsible net rows
		expanded := map[string]bool{} // which networks are expanded
		loaded := false
		showRaw := false

		keysText := func() string {
			toggle := "raw JSON"
			if showRaw {
				toggle = "table"
			}
			parts := []string{"[yellow]j/k[white] move", "[yellow]y/Enter[white] copy line", "[yellow]t[white] " + toggle}
			if editSvc != "" {
				parts = append(parts, "[yellow]d[white] diff", "[yellow]s[white] scale", "[yellow]p[white] ports", "[yellow]l[white] labels", "[yellow]e[white] env", "[yellow]n[white] networks", "[yellow]S[white] secrets", "[yellow]v[white] mounts", "[yellow]r[white] resources", "[yellow]P[white] placement")
			}
			parts = append(parts, "[yellow]Esc/q[white] close")
			return " " + strings.Join(parts, "  ")
		}
		// The single bottom footer shows this overlay's keys; pushed on open below.
		var setHelp func(string) = func(string) {}
		var restoreHelp func()
		setFooter := func(status string) {
			t := keysText()
			if status != "" {
				t = " " + status + "  ·" + t
			}
			setHelp(t)
		}
		populate := func() {
			mode := "table"
			if showRaw {
				mode = "raw json"
			}
			table.SetTitle(fmt.Sprintf(" inspect %s — %s ", title, mode))
			keepRow, _ := table.GetSelection()
			table.Clear()
			plain = plain[:0]
			for k := range rowNet {
				delete(rowNet, k)
			}
			if !loaded {
				table.SetCell(0, 0, tview.NewTableCell("loading…").SetSelectable(false))
				plain = append(plain, "")
				setFooter("")
				return
			}
			firstSel, r := -1, 0
			put := func(text string, color tcell.Color, bold, selectable bool, plainText, net string) {
				cell := tview.NewTableCell(tview.Escape(text)).SetTextColor(color).SetSelectable(selectable)
				if bold {
					cell.SetAttributes(tcell.AttrBold)
				}
				table.SetCell(r, 0, cell)
				plain = append(plain, plainText)
				if net != "" {
					rowNet[r] = net
				}
				if selectable && firstSel < 0 {
					firstSel = r
				}
				r++
			}
			if showRaw {
				for _, ln := range strings.Split(rawJSON, "\n") {
					put(ln, tcell.ColorWhite, false, true, ln, "")
				}
			} else {
				for _, ln := range lines {
					switch ln.Kind {
					case inspTitle:
						put(ln.Text, tcell.ColorAqua, true, false, ln.Text, "")
					case inspHeader:
						put(ln.Text, tcell.ColorAqua, false, false, ln.Text, "")
					case inspDim:
						put(ln.Text, tcell.ColorGray, false, false, ln.Text, "")
					case inspBlank:
						put("", tcell.ColorWhite, false, false, "", "")
					case inspNet:
						marker := "+"
						if expanded[ln.Net] {
							marker = "-"
						}
						put(fmt.Sprintf("  %s %s (%d dns names)", marker, ln.Text, ln.Count), tcell.ColorWhite, false, true, ln.Text, ln.Net)
						if expanded[ln.Net] {
							for _, c := range ln.Children {
								put("      "+c, tcell.ColorGray, false, true, c, "")
							}
						}
					default: // inspField
						put(ln.Text, tcell.ColorWhite, false, true, ln.Text, "")
					}
				}
			}
			table.ScrollToBeginning()
			if firstSel < 0 {
				firstSel = 0
			}
			if keepRow > 0 && keepRow < r {
				table.Select(keepRow, 0)
			} else {
				table.Select(firstSel, 0)
			}
			setFooter("")
		}
		copyLine := func() {
			r, _ := table.GetSelection()
			if r < 0 || r >= len(plain) {
				return
			}
			txt := strings.TrimSpace(plain[r])
			if txt == "" || screen == nil {
				return
			}
			screen.SetClipboard([]byte(txt))
			clientlog.L().Debug("inspect copy", "text", txt)
			setFooter("[green]✓ copied to clipboard[white]")
		}
		closeInspect := func() {
			if restoreHelp != nil {
				restoreHelp()
			}
			pages.RemovePage("inspect")
			app.SetFocus(ctree)
		}
		// reload re-fetches the inspect (after an edit) and refreshes the tree.
		reload := func() {
			loadContainers()
			go func() {
				f, raw, err := fetch()
				app.QueueUpdateDraw(func() {
					if err != nil || !pages.HasPage("inspect") {
						return
					}
					lines, rawJSON, loaded = f, raw, true
					populate()
				})
			}()
		}
		// openDiff shows a unified diff of the service's current spec against its
		// PreviousSpec (what the last rolling update changed). Service-only.
		openDiff := func() {
			tv := tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false)
			tv.SetBorder(true).SetTitle(fmt.Sprintf(" diff %s — previous → current ", editSvc))
			_, restoreDiff := pushOverlayHelp(footerKeys("j/k", "scroll", "g/G", "top/bottom", "Esc", "close"))
			closeDiff := func() {
				restoreDiff()
				pages.RemovePage("inspectdiff")
				app.SetFocus(table)
			}
			tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
				if ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'd')) {
					closeDiff()
					return nil
				}
				return ev
			})
			tv.SetText("loading…")
			pages.AddPage("inspectdiff", centered(tv, 100, 32), true, true)
			app.SetFocus(tv)
			go func() {
				lines, hasPrev, derr := serviceDiffLines(ctx, dcli, editSvc)
				app.QueueUpdateDraw(func() {
					if !pages.HasPage("inspectdiff") {
						return
					}
					switch {
					case derr != nil:
						tv.SetText("[red]error: " + tview.Escape(derr.Error()) + "[white]")
					case !hasPrev:
						tv.SetText("[gray]No previous version to diff — this service has not been updated since it was created.[white]")
					case len(lines) == 0:
						tv.SetText("[gray]No field-level differences between the current and previous spec (only metadata changed).[white]")
					default:
						var b strings.Builder
						b.WriteString("previous → current   ([red]- removed[white]  [green]+ added[white])\n\n")
						for _, l := range lines {
							switch {
							case strings.HasPrefix(l, "+ "):
								b.WriteString("[green]" + tview.Escape(l) + "[white]\n")
							case strings.HasPrefix(l, "- "):
								b.WriteString("[red]" + tview.Escape(l) + "[white]\n")
							default:
								b.WriteString(tview.Escape(l) + "\n")
							}
						}
						tv.SetText(b.String())
						tv.ScrollToBeginning()
					}
				})
			}()
		}
		table.SetSelectionChangedFunc(func(int, int) { setFooter("") })
		table.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape || (ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i')):
				closeInspect()
				return nil
			case ev.Key() == tcell.KeyEnter:
				// Enter on a collapsible network row toggles it; otherwise copies.
				r, _ := table.GetSelection()
				if net, ok := rowNet[r]; ok {
					expanded[net] = !expanded[net]
					populate()
				} else {
					copyLine()
				}
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'y':
				copyLine()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 't':
				showRaw = !showRaw
				populate()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'j':
				return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'k':
				return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
				openDiff()
				return nil
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 's':
				openScalePrompt(editSvc, table, reload)
				return nil
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'p':
				openPortsEditor(editSvc, table, reload)
				return nil
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'l':
				openLabelsEditor(editSvc, table, reload)
				return nil
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'n':
				openNetworksEditor(editSvc, table, reload)
				return nil
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'S':
				openSecretsEditor(editSvc, table, reload)
				return nil
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'v':
				openMountsEditor(editSvc, table, reload)
				return nil
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'e':
				openEnvEditor(editSvc, table, reload)
				return nil
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'r':
				openResourcesEditor(editSvc, table, reload)
				return nil
			case editSvc != "" && ev.Key() == tcell.KeyRune && ev.Rune() == 'P':
				openPlacementMenu(editSvc, table, reload)
				return nil
			}
			return ev
		})
		setHelp, restoreHelp = pushOverlayHelp(keysText())
		populate() // shows "loading…"
		pages.AddPage("inspect", centered(table, 110, 40), true, true)
		app.SetFocus(table)
		go func() {
			start := time.Now()
			f, raw, err := fetch()
			clientlog.Timed(op, start, err)
			app.QueueUpdateDraw(func() {
				if !pages.HasPage("inspect") {
					return
				}
				if err != nil {
					table.Clear()
					table.SetCell(0, 0, tview.NewTableCell("error: "+err.Error()).SetTextColor(tcell.ColorRed).SetSelectable(false))
					setFooter("")
					return
				}
				lines, rawJSON, loaded = f, raw, true
				populate()
			})
		}()
	}
	// inspectCurrent shows service inspect on a service node, task inspect on a
	// container leaf (both from the manager).
	inspectCurrent := func() {
		n := ctree.GetCurrentNode()
		if n == nil {
			return
		}
		if c, ok := n.GetReference().(resolve.Candidate); ok {
			showInspect(fmt.Sprintf("task %s (%s)", shortID(c.ContainerID), orDash(c.Service)), "ui.inspect.task", "",
				func() ([]inspLine, string, error) { return taskInspectViews(ctx, dcli, c.TaskID) })
			return
		}
		if ref, ok := n.GetReference().(svcRef); ok {
			showInspect(fmt.Sprintf("service %s", ref.name), "ui.inspect.service", ref.name,
				func() ([]inspLine, string, error) { return serviceInspectViews(ctx, dcli, ref.name) })
		}
	}
	ctree.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case km.Search:
				startSearch("containers")
				return nil
			case km.ContainerInspect:
				inspectCurrent()
				return nil
			case km.Fold:
				// Collapse. tview's TreeView has no fold key — Left/Right only
				// move the cursor — so fold explicitly. On a container leaf,
				// step out to its service (press h again to fold it).
				if n := ctree.GetCurrentNode(); n != nil {
					if isServiceNode(n) {
						n.SetExpanded(false)
						markService(n)
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
						markService(n)
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
						portPrompt(c)
					} else if kids := n.GetChildren(); len(kids) > 0 {
						if c, ok := kids[0].GetReference().(resolve.Candidate); ok {
							portPrompt(c)
						}
					}
				}
				return nil
			}
		}
		return tabKeys(ev)
	})
	// Enter shows the full detail of a forward. The table truncates the state
	// column, so this is where a failure reason is actually readable.
	ftable.SetSelectedFunc(func(int, int) {
		row, ok := selectedForward()
		if !ok {
			return
		}
		// Re-read from the registry: the rendered row's connErr is only as
		// fresh as the last redraw, and redraws are deliberately rare.
		e, ok := forwards.get(row.id)
		if !ok {
			return
		}
		// A left-aligned TextView, not the info() modal: tview.Modal centers
		// each line on its own, which shears a padded key/value block out of
		// alignment. Values are escaped because dynamic colors are on and an
		// error string can contain "[".
		esc := tview.Escape
		var b strings.Builder
		fmt.Fprintf(&b, "local:      127.0.0.1:%d\n", e.boundPort())
		fmt.Fprintf(&b, "remote:     %d\n", e.remote)
		fmt.Fprintf(&b, "container:  %s\n", esc(shortID(e.cand.ContainerID)))
		fmt.Fprintf(&b, "service:    %s\n", esc(orDash(e.cand.Service)))
		fmt.Fprintf(&b, "node:       %s\n", esc(orDash(e.cand.NodeName)))
		fmt.Fprintf(&b, "state:      %s", esc(e.state.String()))
		if e.err != nil {
			fmt.Fprintf(&b, "\n\n[red]failed:[-] %s", esc(e.err.Error()))
		}
		if e.connErr != nil {
			fmt.Fprintf(&b, "\n\n[yellow]last connection failed:[-]\n%s", esc(e.connErr.Error()))
		}
		b.WriteString("\n\n[gray]d[-] stop   [gray]Esc[-] close")

		tv := tview.NewTextView().SetDynamicColors(true).SetText(b.String())
		tv.SetBorder(true).SetTitle(fmt.Sprintf(" forward #%d ", e.id))
		closeDetail := func() { pages.RemovePage("fwddetail"); app.SetFocus(ftable) }
		tv.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
			switch {
			case ev.Key() == tcell.KeyEscape, ev.Key() == tcell.KeyEnter:
				closeDetail()
				return nil
			case ev.Key() == tcell.KeyRune && (ev.Rune() == 'q' || ev.Rune() == 'i'):
				closeDetail()
				return nil
			case ev.Key() == tcell.KeyRune && ev.Rune() == 'd':
				// Stop straight from the detail view — the operator is already
				// looking at what they are about to kill.
				forwards.remove(e.id)
				closeDetail()
				refreshForwardViews()
				flash(fmt.Sprintf(" [green]stopped[white] forward to %s:%d", shortID(e.cand.ContainerID), e.remote))
				return nil
			}
			return ev
		})
		// Height tracks the content so a short forward gets a snug box and a
		// failed one grows to fit its reason.
		lines := strings.Count(b.String(), "\n") + 1
		pages.AddPage("fwddetail", centered(tv, 66, lines+2), true, true)
		app.SetFocus(tv)
	})
	// On the forwards table: d stops the selected forward, o copies its URL.
	ftable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case km.FwdStop:
				if e, ok := selectedForward(); ok {
					forwards.remove(e.id)
					refreshForwardViews()
					flash(fmt.Sprintf(" [green]stopped[white] forward to %s:%d", shortID(e.cand.ContainerID), e.remote))
				}
				return nil
			case km.FwdCopyURL:
				// Copy rather than launch a browser: the UI often runs over
				// ssh, where opening a local browser would target the wrong
				// machine — and the forward is bound on the operator's side.
				if e, ok := selectedForward(); ok && e.state == forwardActive {
					url := fmt.Sprintf("http://127.0.0.1:%d", e.boundPort())
					if screen != nil {
						screen.SetClipboard([]byte(url))
					}
					flash(" [green]copied[white] " + url)
				}
				return nil
			}
		}
		return tabKeys(ev)
	})
	// On the volumes table, "i" shows which services/containers use the volume.
	// attachVolumeToService mounts a volume into a service from the Volumes tab:
	// pick a service (autocomplete), enter the container target path, choose
	// read-only or not, then a ServiceUpdate adds the mount.
	attachVolumeToService := func(volName string) {
		in := tview.NewInputField().SetLabel("service: ").SetFieldWidth(42).
			SetPlaceholder("type or ↓ to pick; Enter next, Esc cancel")
		names := serviceNamesFromCache()
		in.SetAutocompleteFunc(func(text string) []string {
			text = strings.ToLower(strings.TrimSpace(text))
			var out []string
			for _, s := range names {
				if text == "" || strings.Contains(strings.ToLower(s), text) {
					out = append(out, s)
				}
			}
			return out
		})
		in.SetDoneFunc(func(key tcell.Key) {
			svc := strings.TrimSpace(in.GetText())
			pages.RemovePage("volattach")
			if key != tcell.KeyEnter || svc == "" {
				app.SetFocus(vtable)
				return
			}
			tin := tview.NewInputField().SetLabel("target path: ").SetFieldWidth(42).SetPlaceholder("/data")
			tin.SetDoneFunc(func(k tcell.Key) {
				target := strings.TrimSpace(tin.GetText())
				pages.RemovePage("volattachtgt")
				if k != tcell.KeyEnter || target == "" {
					app.SetFocus(vtable)
					return
				}
				if !strings.HasPrefix(target, "/") {
					info("target must be an absolute path")
					return
				}
				m := tview.NewModal().
					SetText(fmt.Sprintf("Attach volume %q to service %q at %s?\n\nThis triggers a rolling update of the service.", volName, svc, target)).
					AddButtons([]string{"Attach", "Attach read-only", "Cancel"}).
					SetDoneFunc(func(_ int, lbl string) {
						pages.RemovePage("volattachconfirm")
						if lbl == "Cancel" || lbl == "" {
							app.SetFocus(vtable)
							return
						}
						mnt := mount.Mount{Type: mount.TypeVolume, Source: volName, Target: target, ReadOnly: lbl == "Attach read-only"}
						go func() {
							err := addServiceMount(ctx, dcli, svc, mnt)
							app.QueueUpdateDraw(func() {
								app.SetFocus(vtable)
								if err != nil {
									info("attach failed: " + err.Error())
									return
								}
								loadVolumes()
								info(fmt.Sprintf("attached volume %q to %q at %s — rolling update started", volName, svc, target))
							})
						}()
					})
				pages.AddPage("volattachconfirm", m, true, true)
				app.SetFocus(m)
			})
			tin.SetBorder(true).SetTitle(fmt.Sprintf(" attach %s → %s ", volName, svc))
			pages.AddPage("volattachtgt", centered(tin, 64, 3), true, true)
			app.SetFocus(tin)
		})
		in.SetBorder(true).SetTitle(fmt.Sprintf(" attach volume %q to service ", volName))
		pages.AddPage("volattach", centered(in, 64, 3), true, true)
		app.SetFocus(in)
	}
	vtable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case km.Search:
				startSearch("volumes")
				return nil
			case km.VolAttach:
				if v, ok := selectedVolume(); ok {
					attachVolumeToService(v.Name)
				}
				return nil
			case km.VolSelect:
				// Toggle the current volume's selection for a bulk delete.
				if v, ok := selectedVolume(); ok {
					if selectedVols[v.Name] {
						delete(selectedVols, v.Name)
					} else {
						selectedVols[v.Name] = true
					}
					renderVolumeTable()
					updateStatus()
				}
				return nil
			case km.VolSelectAll:
				// Select or deselect all currently displayed volumes.
				all := len(shownVols) > 0
				for _, v := range shownVols {
					if !selectedVols[v.Name] {
						all = false
						break
					}
				}
				for _, v := range shownVols {
					if all {
						delete(selectedVols, v.Name)
					} else {
						selectedVols[v.Name] = true
					}
				}
				renderVolumeTable()
				updateStatus()
				return nil
			case km.VolDelete:
				// Delete the selected volumes, or the one under the cursor.
				var targets []swarmVolume
				if len(selectedVols) > 0 {
					for _, v := range vols {
						if selectedVols[v.Name] {
							targets = append(targets, v)
						}
					}
				} else if v, ok := selectedVolume(); ok {
					targets = []swarmVolume{v}
				}
				deleteVolumes(targets, fmt.Sprintf("Remove %d volume(s) on every node that holds them?", len(targets)))
				return nil
			case km.VolPrune:
				pruneVolumes()
				return nil
			case km.VolUsedBy:
				if v, ok := selectedVolume(); ok {
					showVolumeConsumers(v)
				}
				return nil
			case km.VolSort:
				// Cycle the sort field; pick a sensible default direction for it.
				sortField = (sortField + 1) % 5
				sortDesc = sortField != volSortName && sortField != volSortAge
				renderVolumeTable()
				return nil
			case km.VolSortRev:
				sortDesc = !sortDesc
				renderVolumeTable()
				return nil
			}
		}
		return tabKeys(ev)
	})
	// On the networks table, "i" (like the volumes tab) shows the attached
	// services/containers; Enter does the same.
	nettable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune && ev.Rune() == km.NetAttached {
			if n, ok := selectedNetwork(); ok {
				showNetworkMembers(n)
			}
			return nil
		}
		return tabKeys(ev)
	})
	sectable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		return tabKeys(ev)
	})
	// On the contexts table: "n" creates, "d" removes, "u" (or Enter) activates.
	cxtable.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		if ev.Key() == tcell.KeyRune {
			switch ev.Rune() {
			case km.CtxNew:
				showCreateContext()
				return nil
			case km.CtxDelete:
				if c, ok := selectedContext(); ok {
					deleteContext(c)
				}
				return nil
			case km.CtxUse:
				if c, ok := selectedContext(); ok {
					activateContext(c)
				}
				return nil
			}
		}
		return tabKeys(ev)
	})
	cxtable.SetSelectedFunc(func(int, int) {
		if c, ok := selectedContext(); ok {
			activateContext(c)
		}
	})

	loadContainersSync() // startup: before app.Run, so fetch+apply inline
	setTab("containers")
	refreshCluster()

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
				autoRefreshContainers()
			}
		}
	}()

	// Surface any keys.yaml problems once, non-fatally, over the started UI.
	if len(keyWarnings) > 0 {
		info("keys.yaml:\n\n" + strings.Join(keyWarnings, "\n"))
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
	app.SetScreen(screen)

	if err := app.SetRoot(pages, true).EnableMouse(true).Run(); err != nil {
		return "", &cliError{code: session.TransportFailure, err: fmt.Errorf("ui: %w", err)}
	}
	return switchTo, nil
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
func serviceRow(s resolve.Service, c svcColumns) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-*s  %-*s",
		c.name, orDash(s.Name), c.mode, orDash(s.Mode), c.repl, fmt.Sprintf("%d/%d", s.Running, s.Desired))
	if c.image > 0 {
		fmt.Fprintf(&b, "  %-*s", c.image, orDash(s.Image))
	}
	if s.Ports != "" {
		fmt.Fprintf(&b, "  %s", s.Ports)
	}
	return strings.TrimRight(b.String(), " ")
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

// centered wraps p in a width×height box centered on screen (for modals).
func centered(p tview.Primitive, width, height int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(p, height, 1, true).
			AddItem(nil, 0, 1, false), width, 1, true).
		AddItem(nil, 0, 1, false)
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
