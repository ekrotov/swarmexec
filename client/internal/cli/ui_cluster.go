// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmapi"
	"swarmexec/client/internal/swarmops"
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
// keeps every existing `u.dcli` / `u.ctx` reference compiling
// unchanged (Go promotes an embedded pointer's fields), so this is a change of
// lifetime, not a rewrite of two hundred call sites. And it avoids copying the
// atomics in here — an embedded value would have to be assigned to switch, which
// is exactly what `go vet`'s copylocks check forbids.
//
// What is NOT in here is as deliberate as what is: the widgets, the keymap, the
// mouse mode and the forward registry stay on *ui, and a tab's preferences (a
// sort order, the fold-by-stack toggle) on the tab. They belong to the
// operator's session, not to a cluster, and a switch must not reset them.
type clusterState struct {
	// name is what this cluster is CALLED — the footer, the Contexts tab's
	// marker, the map key. Always a real name.
	//
	// ctxOverride is what it is RESOLVED from, which is not the same thing: at
	// startup it is whatever --context said, and "" there means "whatever
	// docker would use" — $DOCKER_CONTEXT, then $DOCKER_HOST, then the stored
	// current context. Both come out of dockerctx.ResolveName/ResolveHost, so
	// the name always names the daemon dialled: with $DOCKER_HOST set and no
	// context chosen the first cluster is "default", the context that IS
	// DOCKER_HOST — never the stored current one, which may be production.
	name        string
	ctxOverride string

	// The two channels to this cluster, from one resolution (see dockerEndpoint).
	dcli swarmapi.Client
	r    *resolve.Resolver
	cfg  config.Config
	// agents talks to this cluster's node agents, with cfg. Created with the
	// manager client and closed with it: never shared with another cluster.
	agents *agentcall.Client

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

	// sshAddOffered: the ssh-add offer for a failed sign-in was shown, and is
	// not shown again on every refresh (see offerSSHAdd).
	sshAddOffered bool

	regCache *swarmops.RegistryCache // :latest version / newer-tag resolver

	// autoRefreshBusy keeps the tree's background re-list from overlapping
	// itself. Here rather than in the tree tab's per-cluster state because
	// it is read off the UI goroutine, by the poll and the event stream —
	// perCluster is UI-goroutine only — like usageBusy below.
	autoRefreshBusy atomic.Bool

	// live resource usage, from this cluster's node agents
	usage     map[string]containerUsage
	usageHist map[string]*usageRing // the last minutes of usage per container
	nodeUse   map[string]nodeUsage
	usageBusy atomic.Bool

	clusterText string // last cluster-probe summary for the footer

	// Every tab keeps its per-cluster state itself (see perCluster): a tab
	// owns what it shows, and this struct does not know it exists. Where the
	// operator was standing — cursor, folds, filters — comes back with the
	// cluster because each tab keeps it per cluster.
}

// newClusterState prepares a cluster the UI has not connected to yet. Connecting
// is separate (and lazy) so the switch itself cannot block on a dead bastion.
func newClusterState(name, ctxOverride string) *clusterState {
	return &clusterState{
		name:        name,
		ctxOverride: ctxOverride,
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
	resetAskpassDeclines() // a deliberate switch may ask ssh's questions again
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
				if u.offerSSHAdd(target, err, func() { u.switchCluster(name) }) {
					return
				}
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
	// Where the operator was standing is saved by each tab as it re-renders
	// for the new cluster (see treeTab.remember).
	closed := u.leaveCluster()

	u.clusterState = c
	if c.ctx == nil || c.ctx.Err() != nil {
		c.ctx, c.cancel = context.WithCancel(u.runCtx)
	}
	c.agents.Resume() // pool agent connections again (see leaveCluster)
	// One topology watcher per activation, on this cluster's context — so it
	// dies with the switch away, exactly like the polling does.
	go u.watchTopology(c.ctx, c)
	u.activeCtx = c.name

	// The switch is the TUI's own business: docker's stored current context —
	// what `docker` in every other terminal talks to — is left alone. It used
	// to be written here, and a glance at another cluster in the TUI then
	// changed where `docker stack deploy` in the next terminal went; no other
	// terminal UI has that side effect, and nobody expects it from one.
	//
	// ui.sync_docker_context opts back in, for operators who want the shell to
	// follow. It is written only now that the UI is actually on this cluster,
	// and a failure is worth saying but not worth undoing the switch over: the
	// operator asked for this cluster and is now on it.
	//
	// $DOCKER_HOST does not undo an explicit switch: the cluster is dialled by
	// its NAME from here on (ctxOverride == name), and a named stored context
	// resolves to its own endpoint whatever DOCKER_HOST says — exactly as
	// `docker --context X` does. DOCKER_HOST keeps exactly one meaning for the
	// rest of the session: it is what "default" dials. A shell that still
	// exports it ignores the stored current context, though, so the flash says
	// so instead of promising agreement it cannot deliver.
	sync := c.cfg.UI.SyncDockerContext
	var useErr error
	if sync {
		useErr = dockerctx.Use(c.name)
	}

	// From cache first: if this cluster has been visited, the operator is back
	// where they were before the network is touched at all.
	u.treeTab().render()
	// The sidebar is the thing that says WHERE you are, so it has to be
	// repainted by the switch itself. It used to be a tab, and repainting came
	// free with activating that tab; now nothing else would do it, and the
	// markers would keep pointing at the cluster you just left.
	u.renderContexts()
	u.updateStatus()
	u.setTab(u.active) // re-renders the visible tab and kicks its own load

	u.refocusTopOverlay() // a session overlay the switch kept (askpass, client log) keeps the keyboard

	msg := switchedFlash(c.name, u.treeTab().st().loaded, sync, useErr, os.Getenv("DOCKER_HOST") != "")
	if closed > 0 {
		msg += fmt.Sprintf(" [gray]— closed %d dialog(s) of the previous cluster[white]", closed)
	}
	u.flash(msg)
	u.treeTab().load()
	u.refreshCluster()
}

// leaveCluster is everything a switch does to the cluster it leaves, before
// the next one becomes visible, and returns how many of its overlays it
// closed. UI goroutine only.
//
// The order matters. The overlays go first, while their cluster is still the
// visible one. Then the polling stops: that is what "only poll the visible
// cluster" means in practice — not a flag every loader has to check, but the
// context they already take. The manager connection itself stays, and costs
// nothing while idle (the shared ssh master expires on its own, see
// sshMuxPersist). The pooled agent connections do not: one per node, each an
// ssh channel behind a bastion, is a lot to keep for a cluster nobody looks
// at, so they are dropped — except the ones still in use, like a port-forward,
// which outlives the switch. Last the generation moves: past it, anything still in flight for the old
// cluster is stale and async drops it — and the overlay it would have closed
// is already gone.
func (u *ui) leaveCluster() int {
	closed := u.closeClusterOverlays()
	if u.cancel != nil {
		u.cancel()
	}
	if u.agents != nil {
		u.agents.Suspend()
	}
	u.gen.Add(1)
	return closed
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
	ctx, cancel := promptAwareTimeout(c.ctx, clusterProbeTimeout)
	defer cancel()
	_, err := c.dcli.Ping(ctx, client.PingOptions{})
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
	if c.agents != nil {
		c.agents.Close()
		c.agents = nil
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
// is no longer the visible one. Every background load's apply goes through here
// — through async (ui_async.go) for new code, which captures gen itself.
func (u *ui) onCluster(gen uint64, fn func()) { u.deliver(gen, "", fn) }

// switchedFlash is the message a completed switch leaves in the footer, and it
// says what happened to the shell either way: "TUI only" by default, "docker
// context is now X (shell too)" with ui.sync_docker_context. A failed `docker
// context use` is reported here rather than in a flash of its own, which the
// "switched to" flash would have overwritten at once. With $DOCKER_HOST set, a
// stored context is now docker's current one but a shell exporting DOCKER_HOST
// still goes to that host — worth one line, since the sync promises the two
// agree.
func switchedFlash(name string, loaded, synced bool, useErr error, dockerHostSet bool) string {
	if useErr != nil {
		return " [green]switched to[white] " + name + " [yellow]— docker's current context was not updated:[white] " + useErr.Error()
	}
	msg := " [green]switched to[white] " + name
	if !loaded {
		msg += " [gray]— loading[white]"
	}
	if !synced {
		return msg + " [gray](TUI only — your shell's docker context is unchanged)[white]"
	}
	msg += " [gray]— docker context is now " + name + " (shell too)[white]"
	if dockerHostSet && name != "default" {
		msg += " [gray]($DOCKER_HOST still wins for docker in your shell)[white]"
	}
	return msg
}
