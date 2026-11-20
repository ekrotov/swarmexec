// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"sync"
	"time"

	"github.com/moby/moby/api/types/events"
)

// What the manager's event stream reported, kept so it can be looked at.
//
// The stream has been subscribed since !184, but only to decide WHEN to
// re-list the tree; every message was then thrown away. The question an
// operator asks after an incident — "when did what change?" — was therefore
// one the UI could not answer about data it had already received.
//
// Deliberately in memory and deliberately small. This is not a log: it starts
// when the UI connects to the cluster and is gone when the UI ends, which the
// view says in its header so nobody takes it for a record. A cluster that
// needs a real history has the stack definitions in git and `stack diff`; an
// event ring is the answer to "what just happened", not to "what happened in
// April".
const activityCapacity = 500

// activityEntry is ONE event, reduced to the fields that mean the same thing
// on every daemon.
//
// Docker hangs the whole attribute map on an event, and for a service that
// includes every label its deployer wrote — arbitrary strings that end up on
// an operator's screen. The same reasoning CONTRACT.md §3.5 applies to
// container events applies here: take the typed fields, take the few
// attributes whose meaning is defined, and drop the rest.
type activityEntry struct {
	At     time.Time
	Kind   string // "service", "node", "secret", "config", "network"
	Action string // "create", "update", "remove", …
	Name   string // the object's name, or its short id when it has none
	Detail string // one defined attribute, e.g. "updatestate: completed"
}

// activityLog is a cluster's ring of recent events. Written by the event
// watcher's goroutine and read by the UI goroutine, so it locks.
type activityLog struct {
	mu      sync.Mutex
	entries []activityEntry // oldest first, at most activityCapacity
	dropped int             // how many fell out of the ring, for the header
}

// record appends one message, dropping the oldest when the ring is full.
func (a *activityLog) record(ev events.Message) {
	if a == nil || skipEvent(ev) {
		return
	}
	e := activityEntry{
		At:     eventTime(ev),
		Kind:   string(ev.Type),
		Action: string(ev.Action),
		Name:   eventName(ev),
		Detail: eventDetail(ev),
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.entries) == activityCapacity {
		a.entries = append(a.entries[:0], a.entries[1:]...)
		a.dropped++
	}
	a.entries = append(a.entries, e)
}

// list returns a copy, newest first — the order the view reads in.
func (a *activityLog) list() (entries []activityEntry, dropped int) {
	if a == nil {
		return nil, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]activityEntry, len(a.entries))
	for i, e := range a.entries {
		out[len(a.entries)-1-i] = e
	}
	return out, a.dropped
}

// skipEvent drops what would drown the ring.
//
// A network emits connect and disconnect for EVERY container that joins or
// leaves it — on a node with any churn that is most of the stream, and it says
// nothing about the cluster changing shape: it is the shape working. Watched
// live on a real swarm, creating one service produced one service event and
// two network connects. Keeping them would push the service event out of a
// 500-entry ring within minutes of normal operation.
func skipEvent(ev events.Message) bool {
	if ev.Type != events.NetworkEventType {
		return false
	}
	switch ev.Action {
	case events.ActionConnect, events.ActionDisconnect:
		return true
	}
	return false
}

// eventTime is the event's own timestamp, falling back to now for a daemon
// that sent none — an entry with the zero time would sort to 1970 and read as
// a bug in the view.
func eventTime(ev events.Message) time.Time {
	switch {
	case ev.TimeNano > 0:
		return time.Unix(0, ev.TimeNano)
	case ev.Time > 0:
		return time.Unix(ev.Time, 0)
	default:
		return time.Now()
	}
}

// eventName is the object's name. Every swarm event carries one; the short id
// is the fallback, because a row that names nothing is noise.
func eventName(ev events.Message) string {
	if n := ev.Actor.Attributes["name"]; n != "" {
		return n
	}
	if id := ev.Actor.ID; len(id) > 12 {
		return id[:12]
	}
	return ev.Actor.ID
}

// detailKeys are the attributes worth showing, per event type: the ones whose
// key AND meaning are defined by the Engine rather than by whoever wrote the
// compose file. Everything else — labels above all — is dropped.
var detailKeys = map[string][]string{
	string(events.ServiceEventType): {"updatestate.new", "replicas.new"},
	string(events.NodeEventType):    {"state.new", "availability.new"},
	string(events.NetworkEventType): {"type"},
}

// eventDetail is the first defined attribute the event carries, rendered as
// "key: value" without its ".new" suffix, or empty.
func eventDetail(ev events.Message) string {
	for _, k := range detailKeys[string(ev.Type)] {
		if v := ev.Actor.Attributes[k]; v != "" {
			label := k
			if i := len(label) - len(".new"); i > 0 && label[i:] == ".new" {
				label = label[:i]
			}
			return label + ": " + v
		}
	}
	return ""
}
