// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"swarmexec/client/internal/resolve"
)

// forwardState is where a forward is in its life.
type forwardState int

const (
	// forwardStarting covers dialling the agent and binding the local port.
	// Dialling can take up to the connect timeout, so this state is visible.
	forwardStarting forwardState = iota
	forwardActive
	forwardFailed
)

func (s forwardState) String() string {
	switch s {
	case forwardStarting:
		return "starting"
	case forwardActive:
		return "active"
	default:
		return "failed"
	}
}

// forwardEntry is one port forward the operator started from the UI.
//
// Unlike every other stream in this UI, a forward is not tied to a visible
// page: it keeps running after its overlay closes, until it is stopped
// explicitly or the UI exits. That is the whole point of the feature, and the
// reason this registry exists at all.
type forwardEntry struct {
	id int
	// cluster is the docker context this forward's container lives in. A
	// forward now outlives a cluster switch, so the entry has to say which
	// cluster it belongs to — otherwise the Forwards tab lists ports whose
	// CONTAINER and NODE columns name things the visible cluster does not have,
	// and the tree would annotate a same-named container on the wrong cluster.
	cluster   string
	cand      resolve.Candidate
	local     uint32 // requested local port; 0 means "kernel picks"
	remote    uint32
	localAddr string // actually bound address, known once active
	state     forwardState
	err       error
	// connErr is the most recent per-connection failure. The listener stays up
	// through these (one bad connection must not kill a forward), so without
	// surfacing it the forward would read "active" while every connection
	// silently fails — exactly what happens against an outdated agent.
	connErr error
	started time.Time

	// stop tears the forward down. Guarded by the registry mutex, and
	// idempotent so a double "d" cannot panic.
	stop func()
}

// label renders the forward for the tree annotation next to its container.
// label renders "local→remote" so the container row shows both ends: the port
// to connect to locally and the container port it lands on. Showing only the
// local port (as this once did) left the mapping invisible on the main view —
// you had to open the Forwards tab to learn which container port was hit. The
// state lives on the local side: a number when bound, … while starting, ✗ when
// failed; the remote port is always the anchor, since it identifies the target.
func (e *forwardEntry) label() string {
	switch e.state {
	case forwardActive:
		return fmt.Sprintf("%d→%d", e.boundPort(), e.remote)
	case forwardStarting:
		return fmt.Sprintf("…→%d", e.remote)
	default:
		return fmt.Sprintf("✗→%d", e.remote)
	}
}

// boundPort is the local port in effect: the requested one, or the one the
// kernel chose once the listener is up.
func (e *forwardEntry) boundPort() uint32 {
	if e.localAddr != "" {
		if _, p, err := net.SplitHostPort(e.localAddr); err == nil {
			if n, cerr := strconv.ParseUint(p, 10, 32); cerr == nil {
				return uint32(n)
			}
		}
	}
	return e.local
}

// forwardRegistry tracks the live forwards. Every method is safe to call from
// any goroutine: forwards are started and torn down off the UI goroutine, but
// rendered on it.
type forwardRegistry struct {
	mu      sync.Mutex
	entries map[int]*forwardEntry
	nextID  int
}

func newForwardRegistry() *forwardRegistry {
	return &forwardRegistry{entries: map[int]*forwardEntry{}}
}

// add registers a forward in the starting state and returns it.
func (r *forwardRegistry) add(cluster string, cand resolve.Candidate, local, remote uint32, stop func()) *forwardEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	e := &forwardEntry{
		id:      r.nextID,
		cluster: cluster,
		cand:    cand,
		local:   local,
		remote:  remote,
		state:   forwardStarting,
		started: time.Now(),
		stop:    stop,
	}
	r.entries[e.id] = e
	return e
}

// markActive records the address the forward actually bound.
func (r *forwardRegistry) markActive(id int, localAddr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.state, e.localAddr, e.err = forwardActive, localAddr, nil
	}
}

// markFailed records why a forward is unusable but KEEPS it listed. A forward
// that vanished on failure would leave the operator with no idea what went
// wrong; they remove it themselves once they have read the reason.
func (r *forwardRegistry) markFailed(id int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.state, e.err = forwardFailed, err
	}
}

// noteConnError records a per-connection failure without changing state: the
// listener is still up and the next connection may well succeed. It reports
// whether this is the first failure on that forward, so the caller can announce
// it once instead of on every rejected connection.
func (r *forwardRegistry) noteConnError(id int, err error) (first bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		first = e.connErr == nil
		e.connErr = err
	}
	return first
}

// remove stops the forward and drops it from the registry.
func (r *forwardRegistry) remove(id int) {
	r.mu.Lock()
	e, ok := r.entries[id]
	if ok {
		delete(r.entries, id)
	}
	r.mu.Unlock()
	if ok && e.stop != nil {
		e.stop()
	}
}

// stopAll tears every forward down; used when the UI exits so no goroutine or
// gRPC connection outlives the process's main loop.
func (r *forwardRegistry) stopAll() {
	r.mu.Lock()
	all := make([]*forwardEntry, 0, len(r.entries))
	for _, e := range r.entries {
		all = append(all, e)
	}
	r.entries = map[int]*forwardEntry{}
	r.mu.Unlock()
	for _, e := range all {
		if e.stop != nil {
			e.stop()
		}
	}
}

// list returns a stable snapshot, oldest first, so the table does not reshuffle
// under the cursor on every redraw.
func (r *forwardRegistry) list() []forwardEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]forwardEntry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// forContainer returns the forwards attached to one container of one cluster,
// for the tree annotation.
//
// The cluster is part of the question, not a refinement of it: forwards survive
// a switch, and a container id is only unique within its own daemon. Matching on
// the id alone would let one cluster's forward annotate another cluster's row.
func (r *forwardRegistry) forContainer(cluster, containerID string) []forwardEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []forwardEntry
	for _, e := range r.entries {
		if e.cluster == cluster && e.cand.ContainerID == containerID {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// byLocalPort returns the forward holding a local port, if any.
//
// It exists so a second forward onto an occupied port can be refused with a
// sentence that names the culprit. The kernel's "address already in use" is
// true but useless here: the holder is usually THIS tool, on a cluster the
// operator is not currently looking at, and nothing on that screen says so.
//
// Matched on the BOUND port, not the requested one, so a forward that asked for
// 0 and was given 51234 by the kernel is protected too. A requested 0 never
// matches — it means "any free port", which cannot collide with anything. And a
// failed forward is skipped: it holds no listener (the bind never happened, or
// Serve returned and Close ran), so it must not block the port it never got.
func (r *forwardRegistry) byLocalPort(port uint32) (forwardEntry, bool) {
	if port == 0 {
		return forwardEntry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.state != forwardFailed && e.boundPort() == port {
			return *e, true
		}
	}
	return forwardEntry{}, false
}

// get returns a live snapshot of one forward. The detail view uses it rather
// than the rendered row, whose connErr is only as fresh as the last redraw.
func (r *forwardRegistry) get(id int) (forwardEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return forwardEntry{}, false
	}
	return *e, true
}

// counts reports total and active forwards for the footer indicator.
func (r *forwardRegistry) counts() (total, active int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		total++
		if e.state == forwardActive {
			active++
		}
	}
	return total, active
}

// annotateForwards appends the forward markers for a container to its tree row.
func annotateForwards(label string, fws []forwardEntry) string {
	if len(fws) == 0 {
		return label
	}
	parts := make([]string, 0, len(fws))
	for i := range fws {
		parts = append(parts, fws[i].label())
	}
	return label + "  " + strings.Join(parts, " ")
}
