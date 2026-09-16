// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/docker/docker/client"
	"github.com/rivo/tview"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/resolve"
)

// Switching cluster used to mean tearing the whole UI down and building it
// again: runUI returned the chosen context name and the caller called it afresh.
// Everything the operator had was collateral — where the cursor stood, which
// services were unfolded, the "/" filter, and every running port-forward.
//
// clusterState is the alternative: the part of the UI that belongs to ONE
// cluster, kept per cluster, so a switch swaps a pointer instead of rebuilding a
// screen. The widgets are shared and re-rendered from whichever state is
// current; the connections, the caches and the operator's position are not.
//
// It is embedded in *ui BY POINTER, and that is load-bearing twice over. It
// keeps every existing `u.dcli` / `u.vols` / `u.filter` reference compiling
// unchanged (Go promotes an embedded pointer's fields), so this is a change of
// lifetime, not a rewrite of two hundred call sites. And it avoids copying the
// atomics in here — an embedded value would have to be assigned to switch, which
// is exactly what `go vet`'s copylocks check forbids.
//
// What is NOT in here is as deliberate as what is: the widgets, the keymap, the
// mouse mode, the sort preference, the fold-by-stack toggle and the forward
// registry stay on *ui. They belong to the operator's session, not to a cluster,
// and a switch must not reset them.
type clusterState struct {
	// name is what this cluster is CALLED — the footer, the Contexts tab's
	// marker, the map key. Always a real name.
	//
	// ctxOverride is what it is RESOLVED from, which is not the same thing: at
	// startup it is whatever --context/$DOCKER_CONTEXT said, and "" there means
	// "whatever docker would use" — including $DOCKER_HOST, which resolving the
	// name "default" would NOT pick up. Collapsing the two would quietly change
	// which daemon a bare `swarmexec ui` talks to.
	name        string
	ctxOverride string

	// The two channels to this cluster, from one resolution (see dockerEndpoint).
	dcli *client.Client
	r    *resolve.Resolver
	cfg  config.Config

	// ctx bounds this cluster's background work — the tree refresh, the usage
	// poll, every load. It is cancelled when the UI switches AWAY, which is how
	// "only poll the visible cluster" is enforced: not by a flag each loader has
	// to remember to check, but by the context they already take.
	//
	// Forwards deliberately do NOT hang off this. They outlive the tab that
	// started them and now outlive a cluster switch too, so they take the
	// run-wide context instead (ui.runCtx).
	ctx    context.Context
	cancel context.CancelFunc

	// err is why the last attempt to reach this cluster failed, if it did. A
	// cluster that cannot be reached must not take the UI down with it, and it
	// must not take the operator with it either: the switch is refused, this is
	// what they are told, and the Contexts tab keeps it marked so the next
	// glance answers "that one is down" without trying again.
	err error

	// loaded says whether the first fetch has happened. A cluster is connected
	// lazily on first switch, so "no services" and "not looked yet" stay
	// distinguishable.
	loaded bool

	statsGate *statsGate     // nodes whose agent cannot serve stats, so we stop asking
	regCache  *registryCache // :latest version / newer-tag resolver

	// containers tab
	lastCands       []resolve.Candidate
	lastSvcs        []resolve.Service
	svcByName       map[string]resolve.Service
	svcCols         svcColumns
	autoRefreshBusy atomic.Bool

	// live resource usage, from this cluster's node agents
	usage     map[string]containerUsage
	leafBase  map[string]string
	nodeUse   map[string]nodeUsage
	usageBusy atomic.Bool

	// where the operator was standing in this cluster. Restored on a switch
	// back, which is the whole point of keeping the state at all.
	filter    string // container-tree "/" query
	volFilter string // volumes "/" query
	pos       treePos
	// restorePos makes the next tree render take its cursor and folds from pos
	// instead of from the widget. Set by a switch INTO this cluster, and true
	// for exactly one render — every later refresh must keep whatever the
	// operator has since selected.
	restorePos bool

	clusterText string // last cluster-probe summary for the footer

	// volumes tab
	selectedVols    map[string]bool
	shownVols       []swarmVolume
	vols            []swarmVolume
	volUsage        map[string][]volumeConsumer
	volSizes        map[string]int64
	volErrs         map[string]error
	volSizesLoading bool

	// networks / secrets / configs tabs
	nets []swarmNetwork
	secs []swarmSecret
	cfgs []swarmConfig

	// nodes tab
	nodeInfos      []swarmNodeInfo
	nodeVolCounts  map[string]int
	nodeVolsLoaded bool
	nodeImgs       map[string]nodeImages
}

// treePos is where the cursor stood and what was unfolded in one cluster's tree.
//
// The tree already restores both across a refresh, but it reads them off the
// widget it is about to rebuild — which is right for a refresh and wrong for a
// switch, where the widget still holds the PREVIOUS cluster. So the position is
// captured before a switch and handed to the next render explicitly.
type treePos struct {
	container string // cursor on a container, by id
	service   string // else on a service, by name
	stack     string // else on a stack, by name
	svcFolds  map[string]bool
	stackFold map[string]bool
}

// treePosFromWidget reads the cursor and fold state out of the tree as it
// stands. This is the normal source: a refresh must put the operator back
// exactly where they were.
func (u *ui) treePosFromWidget() treePos {
	pos := treePos{svcFolds: map[string]bool{}, stackFold: map[string]bool{}}
	if n := u.ctree.GetCurrentNode(); n != nil {
		switch ref := n.GetReference().(type) {
		case resolve.Candidate:
			pos.container = ref.ContainerID
		case svcRef:
			pos.service = ref.name
		case stackRef:
			pos.stack = ref.name
		}
	}
	u.croot.Walk(func(n, _ *tview.TreeNode) bool {
		switch ref := n.GetReference().(type) {
		case svcRef:
			pos.svcFolds[ref.name] = n.IsExpanded()
			return false // a service's children are containers; nothing to record
		case stackRef:
			pos.stackFold[ref.name] = n.IsExpanded()
		}
		return true
	})
	return pos
}

// newClusterState prepares a cluster the UI has not connected to yet. Connecting
// is separate (and lazy) so the switch itself cannot block on a dead bastion.
func newClusterState(name, ctxOverride string) *clusterState {
	return &clusterState{
		name:         name,
		ctxOverride:  ctxOverride,
		statsGate:    newStatsGate(),
		selectedVols: map[string]bool{},
	}
}

// cluster returns the state for a docker context, creating an empty one the
// first time that context is visited. Keyed by NAME, so the cluster the session
// started on and the same context activated later are one entry — not two
// connections to the same daemon.
func (u *ui) cluster(name, ctxOverride string) *clusterState {
	if c, ok := u.clusters[name]; ok {
		return c
	}
	c := newClusterState(name, ctxOverride)
	u.clusters[name] = c
	return c
}

// switchCluster points the UI at another docker context.
//
// The connect happens BEFORE anything visible changes, and that ordering is the
// design. Swapping first and connecting after would mean a window in which the
// visible cluster has no client — every action would need a nil check, and a
// cluster behind a dead bastion would leave the operator on an empty screen
// they cannot use and did not ask for. Connecting first means a failure costs
// nothing: the message names the cluster, and the UI is still on the one that
// works.
//
// It also means the first switch to a cluster takes as long as reaching it
// does. That is unavoidable — but only once. The connection is kept afterwards,
// so every later switch back is immediate, which is the point of the feature.
func (u *ui) switchCluster(name string) {
	if u.switching {
		u.flash(" [gray]still connecting[white] — one switch at a time")
		return
	}
	if name == u.activeCtx {
		u.flash(" [gray]already on[white] context " + name)
		return
	}
	target := u.cluster(name, name)
	if target.dcli != nil {
		u.activateCluster(target)
		return
	}
	u.switching = true
	u.flash(" [aqua]connecting[white] to " + name + " …")
	go func() {
		err := u.connectCluster(target)
		if err == nil {
			// And then actually reach it. Building a client does not dial, and
			// the version check inside connect deliberately forgives a failed
			// ping — it refuses an OLD daemon, not an absent one, because
			// reporting "unreachable" as "too old" sends the operator looking in
			// the wrong place. That leniency is right there and wrong here:
			// without this probe the switch succeeds against a host that does
			// not exist, and the operator lands on an empty screen whose only
			// clue is "cluster: unreachable" in the footer. Measured against a
			// context pointing at a nonexistent host — the switch went through.
			if err = target.reach(); err != nil {
				// Leave nothing half-built: an unreachable cluster that kept its
				// client would look connected to the next attempt and switch
				// instantly into the same dead end.
				target.close()
			}
		}
		u.app.QueueUpdateDraw(func() {
			u.switching = false
			if err != nil {
				// Deliberately not fatal, and deliberately not a flash: a switch
				// that did not happen has to be unmissable, and the operator is
				// still on a working cluster.
				target.err = err
				u.renderContexts() // mark it unreachable in the list
				u.info(fmt.Sprintf("cannot switch to context %q:\n\n%v", name, err))
				return
			}
			u.activateCluster(target)
		})
	}()
}

// activateCluster makes an already-connected cluster the visible one. UI
// goroutine only.
func (u *ui) activateCluster(c *clusterState) {
	if u.clusterState == c {
		return
	}
	// Save where the operator was standing before the widgets are rebuilt for
	// somebody else's data.
	u.pos = u.treePosFromWidget()

	// Stop the outgoing cluster's polling. This is what "only poll the visible
	// cluster" means in practice: not a flag every loader has to check, but the
	// context they already take. The connection itself stays — and costs
	// nothing while idle, because the shared ssh master expires on its own
	// (see sshMuxPersist).
	if u.cancel != nil {
		u.cancel()
	}
	// Past this point, anything still in flight for the old cluster is stale.
	u.gen.Add(1)

	u.clusterState = c
	if c.ctx == nil || c.ctx.Err() != nil {
		c.ctx, c.cancel = context.WithCancel(u.runCtx)
	}
	u.activeCtx = c.name
	u.restorePos = true

	// Now that the UI is actually on this cluster, make it docker's current
	// context too, so a `docker` command in the next terminal agrees with what
	// the footer says. A failure here is worth saying but not worth undoing the
	// switch over: the operator asked for this cluster and is now on it.
	if err := dockerctx.Use(c.name); err != nil {
		u.flash(" [yellow]switched[white], but docker's current context was not updated: " + err.Error())
	}

	// From cache first: if this cluster has been visited, the operator is back
	// where they were before the network is touched at all.
	u.renderContainers()
	// The sidebar is the thing that says WHERE you are, so it has to be
	// repainted by the switch itself. It used to be a tab, and repainting came
	// free with activating that tab; now nothing else would do it, and the
	// markers would keep pointing at the cluster you just left.
	u.renderContexts()
	u.updateStatus()
	u.setTab(u.active) // re-renders the visible tab and kicks its own load

	if c.loaded {
		u.flash(" [green]switched to[white] " + c.name)
	} else {
		u.flash(" [green]switched to[white] " + c.name + " [gray]— loading[white]")
	}
	u.loadContainers()
	u.refreshCluster()
}

// clusterProbeTimeout bounds the reachability check on a switch.
//
// Generous enough for an ssh handshake through a bastion — and since T1 that is
// usually a channel on a connection that already exists — but short enough that
// a dead host does not hold the operator for the ssh client's own 30-second
// connect timeout while they wait to be told it did not work.
const clusterProbeTimeout = 15 * time.Second

// reach answers the only question a switch actually cares about: is this
// cluster there? A refusal here is not a failure of the tool — it is the switch
// declining to happen, which is why it is worth waiting for.
func (c *clusterState) reach() error {
	ctx, cancel := context.WithTimeout(c.ctx, clusterProbeTimeout)
	defer cancel()
	_, err := c.dcli.Ping(ctx)
	return err
}

// close releases one cluster: its background work, then its connections.
func (c *clusterState) close() {
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if c.dcli != nil {
		c.dcli.Close()
		c.dcli = nil
	}
	c.ctx, c.r = nil, nil
}

// closeClusters releases every cluster the session connected to, not just the
// one that happened to be visible at the end. A session that visited three
// clusters holds three sets of connections, and leaving them for process exit
// would keep the ssh transports alive until their own timeout — visible to the
// operator as ssh processes outliving the tool they started them with.
func (u *ui) closeClusters() {
	for _, c := range u.clusters {
		c.close()
	}
}

// generation is the counter that makes stale results harmless.
//
// A load started against one cluster can finish after the operator has switched
// to another — the fetch is off the UI goroutine and the switch is not. Applying
// it then would write one cluster's services into another cluster's tree, and
// nothing on the screen would say so. Cancelling the old context stops most of
// it, but a result already queued for the UI goroutine is past that check.
//
// So every background load captures the generation it started in, and the apply
// is dropped if the generation has moved. This is the whole answer to the data
// race the old restart-on-switch comment worried about: not a lock around every
// access, but one comparison at the only place a stale result can do damage.
func (u *ui) generation() uint64 { return u.gen.Load() }

// onCluster runs fn on the UI goroutine, unless the cluster it was started for
// is no longer the visible one. Every background load's apply goes through here.
func (u *ui) onCluster(gen uint64, fn func()) {
	u.app.QueueUpdateDraw(func() {
		if u.gen.Load() != gen {
			return
		}
		fn()
	})
}
