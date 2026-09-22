// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"

	"swarmexec/client/internal/clientlog"
)

// The tree has always learned about background changes by re-listing every
// treeRefreshInterval. That is correct and slow: a rolling update, a scaled
// service or a removed stack shows up somewhere between instantly and ten
// seconds later, and the operator cannot tell which.
//
// The manager already knows the moment it happens, so this watches its event
// stream and asks for the same re-list as soon as the swarm's shape changes.
//
// The poll STAYS, unchanged, and that is a deliberate refusal to do what the
// plan said ("replaces the 10s poll"). A silently dead event stream is not
// detectable from here: the Docker events endpoint sends nothing while idle, so
// an ssh tunnel or NAT that drops the connection produces a read that blocks
// forever instead of an error. The watcher would look alive and be deaf, and
// with the poll removed the tree would simply stop updating — a worse failure
// than the one being fixed, and a silent one. Events are therefore a latency
// improvement layered on a mechanism that still works when they fail.
// Lengthening the poll once the stream has proven itself in production is a
// separate, measurable change.
const (
	// topologyDebounce coalesces a burst into one re-list. A rolling update
	// emits an event per state transition per task; without this, a ten-replica
	// service would trigger dozens of TaskList calls in a few seconds — trading
	// the poll's latency for a load spike, which is not the trade being made.
	topologyDebounce = 400 * time.Millisecond

	// topologyMinInterval is the floor between two event-driven re-lists, so a
	// pathological event source cannot busy-loop the manager API.
	topologyMinInterval = 2 * time.Second

	// topologyRetryMin/Max bound the reconnect backoff. The stream ends on any
	// transport hiccup — an ssh tunnel blip is routine — and reconnecting must
	// be neither instant (hammering a manager that is down) nor slow enough to
	// matter (the poll covers the gap anyway).
	topologyRetryMin = 1 * time.Second
	topologyRetryMax = 30 * time.Second
)

// topologyTiming is the coalescing behaviour, injectable so the tests can drive
// it in milliseconds instead of waiting out the production values.
type topologyTiming struct {
	debounce    time.Duration
	minInterval time.Duration
}

var defaultTopologyTiming = topologyTiming{debounce: topologyDebounce, minInterval: topologyMinInterval}

// watchTopology re-lists the container tree when the manager reports a change
// to the swarm's shape, until ctx ends.
//
// It is started per cluster with that cluster's context, so switching away
// cancels it along with everything else that cluster was doing. That is the
// same mechanism the polling uses, and for the same reason: "only the visible
// cluster costs anything" should be enforced by the context a worker already
// takes, not by a flag each one has to remember to check.
func (u *ui) watchTopology(ctx context.Context, c *clusterState) {
	// Services carry rolling updates, scaling, creation and removal. Nodes
	// carry availability and drain. There is deliberately no "task" filter:
	// Docker emits no task events at all, which is why the trigger is a service
	// event and the answer still comes from re-listing tasks.
	//
	// Scoped on purpose — secrets, configs and networks have their own tabs
	// that load on demand, and subscribing to them here would re-list the tree
	// for changes it does not show.
	f := filters.NewArgs()
	f.Add("type", string(events.ServiceEventType))
	f.Add("type", string(events.NodeEventType))

	backoff := topologyRetryMin
	for ctx.Err() == nil {
		connected := u.streamTopology(ctx, c, f)
		if ctx.Err() != nil {
			return
		}
		if connected {
			// The stream carried at least one event before it ended, so the
			// endpoint works and this was a hiccup, not a misconfiguration.
			backoff = topologyRetryMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > topologyRetryMax {
			backoff = topologyRetryMax
		}
	}
}

// streamTopology consumes one subscription until it ends. Everything it does
// beyond opening the stream lives in consumeTopology, which takes channels
// rather than a Docker client — the coalescing is the part with decisions in
// it, and a rule that cannot be tested without a live swarm would not be.
func (u *ui) streamTopology(ctx context.Context, c *clusterState, f filters.Args) bool {
	msgs, errs := c.dcli.Events(ctx, events.ListOptions{Filters: f})
	return consumeTopology(ctx, msgs, errs, defaultTopologyTiming, func(ev events.Message) {
		clientlog.L().Debug("topology event",
			"cluster", c.name, "type", ev.Type, "action", ev.Action)
	}, u.autoRefreshContainers)
}

// consumeTopology coalesces a stream of events into refresh calls, and reports
// whether the stream delivered anything before it ended — which is what tells a
// transient drop apart from an endpoint that never works.
func consumeTopology(
	ctx context.Context,
	msgs <-chan events.Message,
	errs <-chan error,
	t topologyTiming,
	observe func(events.Message),
	refresh func(),
) bool {
	var (
		delivered bool
		pending   bool
		last      time.Time
	)
	// A stopped timer: the debounce arms only once an event actually arrives.
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	for {
		select {
		case <-ctx.Done():
			return delivered

		case err := <-errs:
			if err != nil && ctx.Err() == nil {
				clientlog.L().Debug("topology event stream ended", "err", err)
			}
			return delivered

		case ev, ok := <-msgs:
			if !ok {
				return delivered
			}
			delivered = true
			if observe != nil {
				observe(ev)
			}
			if !pending {
				pending = true
				debounce.Reset(t.debounce)
			}

		case <-debounce.C:
			pending = false
			// The floor DEFERS, it does not drop: a change arriving too soon
			// after the last re-list still gets one, just a moment later.
			// Dropping it would leave the tree stale until the next poll — the
			// exact failure this feature exists to remove.
			if wait := t.minInterval - time.Since(last); wait > 0 {
				pending = true
				debounce.Reset(wait)
				continue
			}
			last = time.Now()
			refresh()
		}
	}
}
