// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package portfwd

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"swarmexec/client/internal/resolve"
)

func cand(id, service, node string) resolve.Candidate {
	return resolve.Candidate{ContainerID: id, Service: service, NodeName: node}
}

func TestForwardRegistry_Lifecycle(t *testing.T) {
	r := NewRegistry()
	stopped := false
	e := r.Add("cs", cand("abc123456789def", "api", "node-1"), 9090, 8080, func() { stopped = true })

	if e.State != StateStarting {
		t.Errorf("new entry state = %v, want starting", e.State)
	}
	if total, active := r.Counts(); total != 1 || active != 0 {
		t.Errorf("counts = (%d,%d), want (1,0)", total, active)
	}

	r.MarkActive(e.ID, "127.0.0.1:9090")
	list := r.List()
	if len(list) != 1 || list[0].State != StateActive {
		t.Fatalf("after markActive: %+v", list)
	}
	if got := list[0].BoundPort(); got != 9090 {
		t.Errorf("boundPort = %d, want 9090", got)
	}
	if total, active := r.Counts(); total != 1 || active != 1 {
		t.Errorf("counts = (%d,%d), want (1,1)", total, active)
	}

	r.Remove(e.ID)
	if !stopped {
		t.Error("remove did not invoke the stop function")
	}
	if total, _ := r.Counts(); total != 0 {
		t.Errorf("total after remove = %d, want 0", total)
	}
}

// A failed forward must stay listed: it is the only place the operator can read
// why it failed.
func TestForwardRegistry_FailedStaysListed(t *testing.T) {
	r := NewRegistry()
	e := r.Add("cs", cand("abc", "api", "n1"), 9090, 8080, func() {})
	r.MarkFailed(e.ID, errors.New("connection refused"))

	list := r.List()
	if len(list) != 1 {
		t.Fatalf("failed forward dropped from the list: %+v", list)
	}
	if list[0].State != StateFailed {
		t.Errorf("state = %v, want failed", list[0].State)
	}
	if list[0].Err == nil || !strings.Contains(list[0].Err.Error(), "connection refused") {
		t.Errorf("err = %v, want the reason preserved", list[0].Err)
	}
	if _, active := r.Counts(); active != 0 {
		t.Errorf("a failed forward counted as active")
	}
}

// A forward whose listener is up but whose connections all fail must not read
// as a healthy "active" — that is what an outdated agent looks like.
func TestForwardRegistry_ConnErrorKeepsForwardActive(t *testing.T) {
	r := NewRegistry()
	e := r.Add("cs", cand("abc", "api", "n1"), 9090, 8080, func() {})
	r.MarkActive(e.ID, "127.0.0.1:9090")
	r.NoteConnError(e.ID, errors.New("agent is older than this client"))

	list := r.List()
	if list[0].State != StateActive {
		t.Errorf("state = %v, want still active (the listener is up)", list[0].State)
	}
	if list[0].ConnErr == nil {
		t.Fatal("connErr not recorded")
	}
	// It still counts as active, because the listener really is bound.
	if _, active := r.Counts(); active != 1 {
		t.Errorf("active count = %d, want 1", active)
	}
}

// The kernel picks the port when local is 0; only the bound address knows it.
func TestForwardEntry_BoundPortFromAddr(t *testing.T) {
	e := Entry{Local: 0, LocalAddr: "127.0.0.1:54321"}
	if got := e.BoundPort(); got != 54321 {
		t.Errorf("boundPort = %d, want 54321", got)
	}
	// Before the listener is up, fall back to what was requested.
	pending := Entry{Local: 9090}
	if got := pending.BoundPort(); got != 9090 {
		t.Errorf("boundPort (pending) = %d, want 9090", got)
	}
	// A malformed address must not panic or invent a port.
	broken := Entry{Local: 7, LocalAddr: "not-an-address"}
	if got := broken.BoundPort(); got != 7 {
		t.Errorf("boundPort (broken addr) = %d, want the requested 7", got)
	}
}

func TestForwardRegistry_ListIsStablyOrdered(t *testing.T) {
	r := NewRegistry()
	for i := 0; i < 5; i++ {
		r.Add("cs", cand(fmt.Sprintf("c%d", i), "svc", "n1"), uint32(9000+i), 80, func() {})
	}
	for round := 0; round < 3; round++ {
		list := r.List()
		for i := 1; i < len(list); i++ {
			if list[i-1].ID >= list[i].ID {
				t.Fatalf("list not ordered by ID: %v", list)
			}
		}
	}
}

func TestForwardRegistry_ForContainer(t *testing.T) {
	r := NewRegistry()
	r.Add("cs", cand("aaa", "api", "n1"), 1, 80, func() {})
	r.Add("cs", cand("aaa", "api", "n1"), 2, 443, func() {})
	r.Add("cs", cand("bbb", "db", "n2"), 3, 5432, func() {})

	if got := r.ForContainer("cs", "aaa"); len(got) != 2 {
		t.Errorf("forContainer(aaa) = %d entries, want 2", len(got))
	}
	if got := r.ForContainer("cs", "bbb"); len(got) != 1 {
		t.Errorf("forContainer(bbb) = %d entries, want 1", len(got))
	}
	if got := r.ForContainer("cs", "zzz"); len(got) != 0 {
		t.Errorf("forContainer(zzz) = %d entries, want 0", len(got))
	}
}

// stopAll runs when the UI exits; nothing may outlive it.
func TestForwardRegistry_StopAll(t *testing.T) {
	r := NewRegistry()
	var mu sync.Mutex
	stopped := 0
	for i := 0; i < 4; i++ {
		r.Add("cs", cand("c", "svc", "n"), uint32(9000+i), 80, func() {
			mu.Lock()
			stopped++
			mu.Unlock()
		})
	}
	r.StopAll()

	mu.Lock()
	defer mu.Unlock()
	if stopped != 4 {
		t.Errorf("stopped %d forwards, want 4", stopped)
	}
	if total, _ := r.Counts(); total != 0 {
		t.Errorf("registry not empty after stopAll: %d", total)
	}
}

// Pressing the stop key (X) twice on the same row must not panic or double-stop.
func TestForwardRegistry_RemoveIsIdempotent(t *testing.T) {
	r := NewRegistry()
	calls := 0
	e := r.Add("cs", cand("c", "svc", "n"), 9090, 80, func() { calls++ })
	r.Remove(e.ID)
	r.Remove(e.ID)
	if calls != 1 {
		t.Errorf("stop called %d times, want 1", calls)
	}
}

// The marker a container row carries for one forward: the local side shows
// state, the remote side is the anchor, because it is what identifies the
// target. Assembling several of them is the tree's business (cli).
func TestEntryLabelShowsBothEnds(t *testing.T) {
	cases := []struct {
		name string
		e    Entry
		want string
	}{
		{"active", Entry{State: StateActive, LocalAddr: "127.0.0.1:9090", Remote: 8080}, "9090\u21928080"},
		{"starting", Entry{State: StateStarting, Local: 6000, Remote: 5432}, "\u2026\u21925432"},
		{"failed", Entry{State: StateFailed, Local: 7070, Remote: 443}, "\u2717\u2192443"},
	}
	for _, tc := range cases {
		if got := tc.e.Label(); got != tc.want {
			t.Errorf("%s: Label() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The registry is written from forward goroutines and read by the UI goroutine;
// this is the race detector's job to police.
func TestForwardRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := r.Add("cs", cand(fmt.Sprintf("c%d", i), "svc", "n"), uint32(9000+i), 80, func() {})
			r.MarkActive(e.ID, fmt.Sprintf("127.0.0.1:%d", 9000+i))
			_ = r.List()
			_, _ = r.Counts()
			_ = r.ForContainer("cs", fmt.Sprintf("c%d", i))
			r.Remove(e.ID)
		}(i)
	}
	wg.Wait()
	if total, _ := r.Counts(); total != 0 {
		t.Errorf("total = %d after all removed, want 0", total)
	}
}

// The warning marker is "the last connection failed": once a connection works
// again it has to go, or it stays for the life of the forward.
func TestForwardRegistry_ConnErrorClearsOnSuccess(t *testing.T) {
	r := NewRegistry()
	e := r.Add("cs", cand("abc", "api", "n1"), 9090, 8080, func() {})
	r.MarkActive(e.ID, "127.0.0.1:9090")
	if r.ClearConnError(e.ID) {
		t.Error("cleared an error that was never there")
	}
	r.NoteConnError(e.ID, errors.New("boom"))
	if !r.ClearConnError(e.ID) {
		t.Fatal("a success did not clear the recorded failure")
	}
	if r.List()[0].ConnErr != nil {
		t.Error("connErr survived a successful connection")
	}
	if !r.NoteConnError(e.ID, errors.New("again")) {
		t.Error("a failure after a success must announce itself again")
	}
}

// How a forward names itself. The bind address is the REQUESTED one and the
// port is the BOUND one, and the asymmetry is deliberate: Go binds a 0.0.0.0
// wildcard as the dual-stack [::], so the listener would report "[::]" for
// what the operator typed, while only the listener knows which port the kernel
// picked for 0.
func TestEntryRendersItsLocalEnd(t *testing.T) {
	cases := []struct {
		name                string
		e                   Entry
		bind, endpoint, url string
	}{
		{"default address", Entry{Local: 8080}, DefaultAddress, "127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"wildcard keeps what was typed", Entry{Address: "0.0.0.0", Local: 8080, LocalAddr: "[::]:8080"},
			"0.0.0.0", "0.0.0.0:8080", "http://127.0.0.1:8080"},
		{"v6 wildcard browses loopback", Entry{Address: "::", Local: 9090},
			"::", "[::]:9090", "http://[::1]:9090"},
		{"kernel-chosen port", Entry{Address: "127.0.0.1", Local: 0, LocalAddr: "127.0.0.1:51234"},
			"127.0.0.1", "127.0.0.1:51234", "http://127.0.0.1:51234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.BindAddress(); got != tc.bind {
				t.Errorf("BindAddress() = %q, want %q", got, tc.bind)
			}
			if got := tc.e.LocalEndpoint(); got != tc.endpoint {
				t.Errorf("LocalEndpoint() = %q, want %q", got, tc.endpoint)
			}
			if got := tc.e.URL(); got != tc.url {
				t.Errorf("URL() = %q, want %q", got, tc.url)
			}
		})
	}
}

// Get returns a live snapshot, which is the point: the detail view reads the
// current connErr, not the one the last redraw rendered.
func TestGetReturnsTheCurrentState(t *testing.T) {
	r := NewRegistry()
	e := r.Add("prod", cand("abc", "api", "n1"), 8080, 80, func() {})
	r.NoteConnError(e.ID, errNoted)

	got, ok := r.Get(e.ID)
	if !ok {
		t.Fatal("a registered forward must be findable")
	}
	if got.ConnErr != errNoted {
		t.Errorf("ConnErr = %v, want the one just noted", got.ConnErr)
	}
	if _, ok := r.Get(e.ID + 1000); ok {
		t.Error("an unknown id must not be found")
	}
}

// The state names show up in the Forwards table; a missing one would render as
// an empty column rather than as a compile error.
func TestStateNames(t *testing.T) {
	for s, want := range map[State]string{
		StateStarting: "starting",
		StateActive:   "active",
		StateFailed:   "failed",
	} {
		if got := s.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", s, got, want)
		}
	}
}

var errNoted = errors.New("connection refused")
