// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

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
	r := newForwardRegistry()
	stopped := false
	e := r.add(cand("abc123456789def", "api", "node-1"), 9090, 8080, func() { stopped = true })

	if e.state != forwardStarting {
		t.Errorf("new entry state = %v, want starting", e.state)
	}
	if total, active := r.counts(); total != 1 || active != 0 {
		t.Errorf("counts = (%d,%d), want (1,0)", total, active)
	}

	r.markActive(e.id, "127.0.0.1:9090")
	list := r.list()
	if len(list) != 1 || list[0].state != forwardActive {
		t.Fatalf("after markActive: %+v", list)
	}
	if got := list[0].boundPort(); got != 9090 {
		t.Errorf("boundPort = %d, want 9090", got)
	}
	if total, active := r.counts(); total != 1 || active != 1 {
		t.Errorf("counts = (%d,%d), want (1,1)", total, active)
	}

	r.remove(e.id)
	if !stopped {
		t.Error("remove did not invoke the stop function")
	}
	if total, _ := r.counts(); total != 0 {
		t.Errorf("total after remove = %d, want 0", total)
	}
}

// A failed forward must stay listed: it is the only place the operator can read
// why it failed.
func TestForwardRegistry_FailedStaysListed(t *testing.T) {
	r := newForwardRegistry()
	e := r.add(cand("abc", "api", "n1"), 9090, 8080, func() {})
	r.markFailed(e.id, errors.New("connection refused"))

	list := r.list()
	if len(list) != 1 {
		t.Fatalf("failed forward dropped from the list: %+v", list)
	}
	if list[0].state != forwardFailed {
		t.Errorf("state = %v, want failed", list[0].state)
	}
	if list[0].err == nil || !strings.Contains(list[0].err.Error(), "connection refused") {
		t.Errorf("err = %v, want the reason preserved", list[0].err)
	}
	if _, active := r.counts(); active != 0 {
		t.Errorf("a failed forward counted as active")
	}
}

// A forward whose listener is up but whose connections all fail must not read
// as a healthy "active" — that is what an outdated agent looks like.
func TestForwardRegistry_ConnErrorKeepsForwardActive(t *testing.T) {
	r := newForwardRegistry()
	e := r.add(cand("abc", "api", "n1"), 9090, 8080, func() {})
	r.markActive(e.id, "127.0.0.1:9090")
	r.noteConnError(e.id, errors.New("agent is older than this client"))

	list := r.list()
	if list[0].state != forwardActive {
		t.Errorf("state = %v, want still active (the listener is up)", list[0].state)
	}
	if list[0].connErr == nil {
		t.Fatal("connErr not recorded")
	}
	// It still counts as active, because the listener really is bound.
	if _, active := r.counts(); active != 1 {
		t.Errorf("active count = %d, want 1", active)
	}
}

// The kernel picks the port when local is 0; only the bound address knows it.
func TestForwardEntry_BoundPortFromAddr(t *testing.T) {
	e := forwardEntry{local: 0, localAddr: "127.0.0.1:54321"}
	if got := e.boundPort(); got != 54321 {
		t.Errorf("boundPort = %d, want 54321", got)
	}
	// Before the listener is up, fall back to what was requested.
	pending := forwardEntry{local: 9090}
	if got := pending.boundPort(); got != 9090 {
		t.Errorf("boundPort (pending) = %d, want 9090", got)
	}
	// A malformed address must not panic or invent a port.
	broken := forwardEntry{local: 7, localAddr: "not-an-address"}
	if got := broken.boundPort(); got != 7 {
		t.Errorf("boundPort (broken addr) = %d, want the requested 7", got)
	}
}

func TestForwardRegistry_ListIsStablyOrdered(t *testing.T) {
	r := newForwardRegistry()
	for i := 0; i < 5; i++ {
		r.add(cand(fmt.Sprintf("c%d", i), "svc", "n1"), uint32(9000+i), 80, func() {})
	}
	for round := 0; round < 3; round++ {
		list := r.list()
		for i := 1; i < len(list); i++ {
			if list[i-1].id >= list[i].id {
				t.Fatalf("list not ordered by id: %v", list)
			}
		}
	}
}

func TestForwardRegistry_ForContainer(t *testing.T) {
	r := newForwardRegistry()
	r.add(cand("aaa", "api", "n1"), 1, 80, func() {})
	r.add(cand("aaa", "api", "n1"), 2, 443, func() {})
	r.add(cand("bbb", "db", "n2"), 3, 5432, func() {})

	if got := r.forContainer("aaa"); len(got) != 2 {
		t.Errorf("forContainer(aaa) = %d entries, want 2", len(got))
	}
	if got := r.forContainer("bbb"); len(got) != 1 {
		t.Errorf("forContainer(bbb) = %d entries, want 1", len(got))
	}
	if got := r.forContainer("zzz"); len(got) != 0 {
		t.Errorf("forContainer(zzz) = %d entries, want 0", len(got))
	}
}

// stopAll runs when the UI exits; nothing may outlive it.
func TestForwardRegistry_StopAll(t *testing.T) {
	r := newForwardRegistry()
	var mu sync.Mutex
	stopped := 0
	for i := 0; i < 4; i++ {
		r.add(cand("c", "svc", "n"), uint32(9000+i), 80, func() {
			mu.Lock()
			stopped++
			mu.Unlock()
		})
	}
	r.stopAll()

	mu.Lock()
	defer mu.Unlock()
	if stopped != 4 {
		t.Errorf("stopped %d forwards, want 4", stopped)
	}
	if total, _ := r.counts(); total != 0 {
		t.Errorf("registry not empty after stopAll: %d", total)
	}
}

// Pressing "d" twice on the same row must not panic or double-stop.
func TestForwardRegistry_RemoveIsIdempotent(t *testing.T) {
	r := newForwardRegistry()
	calls := 0
	e := r.add(cand("c", "svc", "n"), 9090, 80, func() { calls++ })
	r.remove(e.id)
	r.remove(e.id)
	if calls != 1 {
		t.Errorf("stop called %d times, want 1", calls)
	}
}

func TestAnnotateForwards(t *testing.T) {
	base := "abc123  node-1  up 3h"
	if got := annotateForwards(base, nil); got != base {
		t.Errorf("no forwards should leave the label untouched, got %q", got)
	}

	// Each marker carries the remote (container) port, so the mapping is
	// visible on the container row without opening the Forwards tab.
	active := forwardEntry{state: forwardActive, localAddr: "127.0.0.1:9090", remote: 8080}
	starting := forwardEntry{state: forwardStarting, local: 6000, remote: 5432}
	failed := forwardEntry{state: forwardFailed, local: 7070, remote: 443}

	got := annotateForwards(base, []forwardEntry{active, starting, failed})
	for _, want := range []string{base, "9090→8080", "…→5432", "✗→443"} {
		if !strings.Contains(got, want) {
			t.Errorf("annotation %q missing %q", got, want)
		}
	}
}

// The registry is written from forward goroutines and read by the UI goroutine;
// this is the race detector's job to police.
func TestForwardRegistry_ConcurrentAccess(t *testing.T) {
	r := newForwardRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := r.add(cand(fmt.Sprintf("c%d", i), "svc", "n"), uint32(9000+i), 80, func() {})
			r.markActive(e.id, fmt.Sprintf("127.0.0.1:%d", 9000+i))
			_ = r.list()
			_, _ = r.counts()
			_ = r.forContainer(fmt.Sprintf("c%d", i))
			r.remove(e.id)
		}(i)
	}
	wg.Wait()
	if total, _ := r.counts(); total != 0 {
		t.Errorf("total = %d after all removed, want 0", total)
	}
}
