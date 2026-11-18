// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package portfwd

import (
	"fmt"
	"net"
	"sort"
	"strconv"

	"sync"
	"time"

	"swarmexec/client/internal/resolve"
)

// State is where a forward is in its life.
type State int

const (
	// StateStarting covers dialling the agent and binding the local port.
	// Dialling can take up to the connect timeout, so this state is visible.
	StateStarting State = iota
	StateActive
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateActive:
		return "active"
	default:
		return "failed"
	}
}

// Entry is one port forward the operator started from the UI.
//
// Unlike every other stream in this UI, a forward is not tied to a visible
// page: it keeps running after its overlay closes, until it is stopped
// explicitly or the UI exits. That is the whole point of the feature, and the
// reason this registry exists at all.
type Entry struct {
	ID int
	// cluster is the docker context this forward's container lives in. A
	// forward now outlives a cluster switch, so the entry has to say which
	// cluster it belongs to — otherwise the Forwards tab lists ports whose
	// CONTAINER and NODE columns name things the visible cluster does not have,
	// and the tree would annotate a same-named container on the wrong cluster.
	Cluster   string
	Cand      resolve.Candidate
	Address   string // requested local bind address; empty means DefaultAddress
	Local     uint32 // requested local port; 0 means "kernel picks"
	Remote    uint32
	LocalAddr string // actually bound address, known once active
	State     State
	Err       error
	// connErr is the most recent per-connection failure. The listener stays up
	// through these (one bad connection must not kill a forward), so without
	// surfacing it the forward would read "active" while every connection
	// silently fails — exactly what happens against an outdated agent.
	ConnErr error
	Started time.Time

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
func (e *Entry) Label() string {
	switch e.State {
	case StateActive:
		return fmt.Sprintf("%d→%d", e.BoundPort(), e.Remote)
	case StateStarting:
		return fmt.Sprintf("…→%d", e.Remote)
	default:
		return fmt.Sprintf("✗→%d", e.Remote)
	}
}

// boundPort is the local port in effect: the requested one, or the one the
// kernel chose once the listener is up.
func (e *Entry) BoundPort() uint32 {
	if e.LocalAddr != "" {
		if _, p, err := net.SplitHostPort(e.LocalAddr); err == nil {
			if n, cerr := strconv.ParseUint(p, 10, 32); cerr == nil {
				return uint32(n)
			}
		}
	}
	return e.Local
}

// bindAddress is the local address the forward was asked to listen on.
//
// The requested address, not the bound one: Go binds a 0.0.0.0 wildcard as the
// dual-stack [::], so the listener's own address would show "[::]" for what the
// operator typed as 0.0.0.0. The port is the other way round (see boundPort),
// because only the listener knows which port the kernel picked for 0.
func (e *Entry) BindAddress() string {
	if e.Address == "" {
		return DefaultAddress
	}
	return e.Address
}

// localEndpoint renders address:port for the table and the detail view.
func (e *Entry) LocalEndpoint() string {
	return net.JoinHostPort(e.BindAddress(), strconv.FormatUint(uint64(e.BoundPort()), 10))
}

// url is what the copy-URL key puts on the clipboard. A forward bound to
// 0.0.0.0 or :: listens on loopback too, and loopback is what a browser on this
// machine can actually connect to.
func (e *Entry) URL() string {
	return "http://" + net.JoinHostPort(BrowseHost(e.BindAddress()), strconv.FormatUint(uint64(e.BoundPort()), 10))
}

// Registry tracks the live forwards. Every method is safe to call from
// any goroutine: forwards are started and torn down off the UI goroutine, but
// rendered on it.
type Registry struct {
	mu      sync.Mutex
	entries map[int]*Entry
	nextID  int
}

func NewRegistry() *Registry {
	return &Registry{entries: map[int]*Entry{}}
}

// add registers a loopback forward in the starting state and returns it.
func (r *Registry) Add(cluster string, cand resolve.Candidate, local, remote uint32, stop func()) *Entry {
	return r.AddAt(cluster, DefaultAddress, cand, local, remote, stop)
}

// addAt registers a forward bound to address in the starting state.
func (r *Registry) AddAt(cluster, address string, cand resolve.Candidate, local, remote uint32, stop func()) *Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	e := &Entry{
		ID:      r.nextID,
		Cluster: cluster,
		Cand:    cand,
		Address: address,
		Local:   local,
		Remote:  remote,
		State:   StateStarting,
		Started: time.Now(),
		stop:    stop,
	}
	r.entries[e.ID] = e
	return e
}

// markActive records the address the forward actually bound.
func (r *Registry) MarkActive(id int, localAddr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.State, e.LocalAddr, e.Err = StateActive, localAddr, nil
	}
}

// markFailed records why a forward is unusable but KEEPS it listed. A forward
// that vanished on failure would leave the operator with no idea what went
// wrong; they remove it themselves once they have read the reason.
func (r *Registry) MarkFailed(id int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		e.State, e.Err = StateFailed, err
	}
}

// noteConnError records a per-connection failure without changing state: the
// listener is still up and the next connection may well succeed. It reports
// whether this is the first failure on that forward, so the caller can announce
// it once instead of on every rejected connection.
func (r *Registry) NoteConnError(id int, err error) (first bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok {
		first = e.ConnErr == nil
		e.ConnErr = err
	}
	return first
}

// clearConnError forgets the last per-connection failure once a connection
// succeeds. Without it the warning marker stayed on a forward for as long as it
// lived, long after the problem (a restarting container, a flapping bastion)
// was gone. It reports whether there was one to clear, so the caller redraws
// only on a change.
func (r *Registry) ClearConnError(id int) (cleared bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[id]; ok && e.ConnErr != nil {
		e.ConnErr = nil
		cleared = true
	}
	return cleared
}

// remove stops the forward and drops it from the registry.
func (r *Registry) Remove(id int) {
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
func (r *Registry) StopAll() {
	r.mu.Lock()
	all := make([]*Entry, 0, len(r.entries))
	for _, e := range r.entries {
		all = append(all, e)
	}
	r.entries = map[int]*Entry{}
	r.mu.Unlock()
	for _, e := range all {
		if e.stop != nil {
			e.stop()
		}
	}
}

// list returns a stable snapshot, oldest first, so the table does not reshuffle
// under the cursor on every redraw.
func (r *Registry) List() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// forContainer returns the forwards attached to one container of one cluster,
// for the tree annotation.
//
// The cluster is part of the question, not a refinement of it: forwards survive
// a switch, and a container id is only unique within its own daemon. Matching on
// the id alone would let one cluster's forward annotate another cluster's row.
func (r *Registry) ForContainer(cluster, containerID string) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Entry
	for _, e := range r.entries {
		if e.Cluster == cluster && e.Cand.ContainerID == containerID {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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
func (r *Registry) ByLocalPort(port uint32) (Entry, bool) {
	if port == 0 {
		return Entry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.State != StateFailed && e.BoundPort() == port {
			return *e, true
		}
	}
	return Entry{}, false
}

// get returns a live snapshot of one forward. The detail view uses it rather
// than the rendered row, whose connErr is only as fresh as the last redraw.
func (r *Registry) Get(id int) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// counts reports total and active forwards for the footer indicator.
func (r *Registry) Counts() (total, active int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		total++
		if e.State == StateActive {
			active++
		}
	}
	return total, active
}
