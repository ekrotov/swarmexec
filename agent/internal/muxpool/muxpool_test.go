// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package muxpool

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"swarmexec/internal/forwardmux"
)

// liveSidecars reports how many sidecars the pool holds. Test-only (Q-03).
func (p *Pool) liveSidecars() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subs)
}

// fakeChannel is the agent's end of an in-memory sidecar stdio; the test holds
// the other end as a fakeSidecar.
type fakeChannel struct {
	net.Conn
	closed atomic.Int32
}

func (c *fakeChannel) Close() {
	c.closed.Add(1)
	_ = c.Conn.Close()
}

// fakeSidecar is the sidecar side: it reads the frames the pool writes and
// answers with frames of its own.
type fakeSidecar struct {
	t    *testing.T
	conn net.Conn
	ch   *fakeChannel
}

func (s *fakeSidecar) recv() forwardmux.Frame {
	s.t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	f, err := forwardmux.Read(s.conn)
	if err != nil {
		s.t.Fatalf("sidecar read: %v", err)
	}
	return f
}

func (s *fakeSidecar) send(f forwardmux.Frame) {
	s.t.Helper()
	if err := forwardmux.Write(s.conn, f); err != nil {
		s.t.Fatalf("sidecar write: %v", err)
	}
}

// acceptOpen answers the next open frame with OpenOK and returns its id.
func (s *fakeSidecar) acceptOpen() uint32 {
	s.t.Helper()
	f := s.recv()
	if f.Kind != forwardmux.KindOpen {
		s.t.Fatalf("want an open frame, got %s", forwardmux.KindName(f.Kind))
	}
	s.send(forwardmux.Frame{Conn: f.Conn, Kind: forwardmux.KindOpenOK})
	return f.Conn
}

// opener is an OpenFunc that hands out fake sidecars and lets the test pick
// each one up.
type opener struct {
	t       *testing.T
	mu      sync.Mutex
	opened  int
	ports   []uint32
	failure error
	started chan *fakeSidecar
}

func newOpener(t *testing.T) *opener {
	return &opener{t: t, started: make(chan *fakeSidecar, 8)}
}

func (o *opener) open(_ context.Context, _ string, port uint32) (Channel, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failure != nil {
		return nil, o.failure
	}
	o.opened++
	o.ports = append(o.ports, port)
	agent, side := net.Pipe()
	ch := &fakeChannel{Conn: agent}
	o.started <- &fakeSidecar{t: o.t, conn: side, ch: ch}
	return ch, nil
}

func (o *opener) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened
}

func (o *opener) next() *fakeSidecar {
	o.t.Helper()
	select {
	case sc := <-o.started:
		return sc
	case <-time.After(3 * time.Second):
		o.t.Fatal("no sidecar was started")
		return nil
	}
}

// dial runs OpenConn in the background, since it blocks until the sidecar
// answers.
func dial(p *Pool, container string, port uint32) <-chan dialResult {
	out := make(chan dialResult, 1)
	go func() {
		c, err := p.OpenConn(context.Background(), container, port)
		out <- dialResult{c, err}
	}()
	return out
}

type dialResult struct {
	conn *Conn
	err  error
}

func await(t *testing.T, r <-chan dialResult) dialResult {
	t.Helper()
	select {
	case res := <-r:
		return res
	case <-time.After(3 * time.Second):
		t.Fatal("OpenConn did not return")
		return dialResult{}
	}
}

// One sidecar per (container, port): a second connection to the same target
// reuses it; another port gets its own.
func TestPool_OneSidecarPerTarget(t *testing.T) {
	o := newOpener(t)
	p := New(o.open)
	defer p.CloseAll()

	r1 := dial(p, "c", 80)
	sc := o.next()
	id1 := sc.acceptOpen()
	c1 := await(t, r1)
	if c1.err != nil {
		t.Fatal(c1.err)
	}

	r2 := dial(p, "c", 80)
	id2 := sc.acceptOpen()
	c2 := await(t, r2)
	if c2.err != nil || id1 == id2 {
		t.Fatalf("second conn: err %v, ids %d/%d", c2.err, id1, id2)
	}
	if o.count() != 1 || p.liveSidecars() != 1 {
		t.Errorf("opened %d, live %d; want one sidecar for one target", o.count(), p.liveSidecars())
	}

	r3 := dial(p, "c", 443)
	o.next().acceptOpen()
	if res := await(t, r3); res.err != nil {
		t.Fatal(res.err)
	}
	if o.count() != 2 || p.liveSidecars() != 2 {
		t.Errorf("opened %d, live %d; want a second sidecar for a second port", o.count(), p.liveSidecars())
	}
}

// Data is framed per connection both ways, and closing one connection sends
// its close frame without touching the sidecar.
func TestPool_ConnCarriesItsOwnFrames(t *testing.T) {
	o := newOpener(t)
	p := New(o.open)
	defer p.CloseAll()

	r := dial(p, "c", 80)
	sc := o.next()
	id := sc.acceptOpen()
	c := await(t, r).conn

	go func() { _, _ = c.Write([]byte("ping")) }()
	if f := sc.recv(); f.Kind != forwardmux.KindData || f.Conn != id || string(f.Payload) != "ping" {
		t.Fatalf("got %s on %d: %q", forwardmux.KindName(f.Kind), f.Conn, f.Payload)
	}

	// The connection's read side is a synchronous pipe, so the reader has to
	// be running before the sidecar delivers.
	var got bytes.Buffer
	copied := make(chan error, 1)
	go func() { copied <- c.CopyTo(&got) }()
	sc.send(forwardmux.Frame{Conn: id, Kind: forwardmux.KindData, Payload: []byte("pong")})
	sc.send(forwardmux.Frame{Conn: id, Kind: forwardmux.KindClose})
	select {
	case err := <-copied:
		if err != nil || got.String() != "pong" {
			t.Fatalf("CopyTo = %q, %v", got.String(), err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("CopyTo did not end on the close frame")
	}

	go c.CloseWrite()
	if f := sc.recv(); f.Kind != forwardmux.KindCloseWrite || f.Conn != id {
		t.Fatalf("want close-write on %d, got %s on %d", id, forwardmux.KindName(f.Kind), f.Conn)
	}
	c.Close()
	if sc.ch.closed.Load() != 0 {
		t.Error("closing a connection closed the sidecar")
	}
}

// A refused dial inside the sidecar fails that connection with the reason; the
// sidecar it was tried on is dropped rather than retried, because it was fresh.
func TestPool_DialRefusalIsReported(t *testing.T) {
	o := newOpener(t)
	p := New(o.open)
	defer p.CloseAll()

	r := dial(p, "c", 80)
	sc := o.next()
	f := sc.recv()
	sc.send(forwardmux.Frame{Conn: f.Conn, Kind: forwardmux.KindOpenErr, Payload: []byte("connection refused")})
	res := await(t, r)
	if res.err == nil || !strings.Contains(res.err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the refusal", res.err)
	}
	if o.count() != 1 {
		t.Errorf("a refused dial on a fresh sidecar was retried: %d opened", o.count())
	}
}

// A sidecar that cannot be started is the caller's error, unchanged — the
// server's sidecar cap reports ResourceExhausted this way.
func TestPool_OpenFailurePassesThrough(t *testing.T) {
	o := newOpener(t)
	o.failure = errors.New("limit reached")
	p := New(o.open)
	if _, err := p.OpenConn(context.Background(), "c", 80); err == nil || err.Error() != "limit reached" {
		t.Fatalf("err = %v", err)
	}
	if p.liveSidecars() != 0 {
		t.Error("a failed open left a sidecar in the pool")
	}
}

// When the sidecar dies every connection on it ends, and the pool forgets it.
func TestPool_SidecarDeathEndsEveryConnection(t *testing.T) {
	o := newOpener(t)
	p := New(o.open)
	defer p.CloseAll()

	r := dial(p, "c", 80)
	sc := o.next()
	sc.acceptOpen()
	c := await(t, r).conn

	_ = sc.conn.Close() // the container is gone
	done := make(chan error, 1)
	go func() { done <- c.CopyTo(&bytes.Buffer{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a connection on a dead sidecar ended without a reason")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a connection outlived its sidecar")
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.liveSidecars() != 0 || sc.ch.closed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("dead sidecar kept: live %d, closed %d", p.liveSidecars(), sc.ch.closed.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

// A sidecar with no connections lingers, then is removed.
func TestPool_IdleSidecarIsRemoved(t *testing.T) {
	o := newOpener(t)
	p := New(o.open)
	p.idleLinger = 20 * time.Millisecond

	r := dial(p, "c", 80)
	sc := o.next()
	sc.acceptOpen()
	c := await(t, r).conn
	go func() {
		// Drain the close frame so the writer is never stuck on the pipe.
		for {
			if _, err := forwardmux.Read(sc.conn); err != nil {
				return
			}
		}
	}()
	c.Close()

	deadline := time.Now().Add(2 * time.Second)
	for p.liveSidecars() != 0 || sc.ch.closed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("idle sidecar kept: live %d, closed %d", p.liveSidecars(), sc.ch.closed.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

// CloseAll, the shutdown path, removes every sidecar.
func TestPool_CloseAll(t *testing.T) {
	o := newOpener(t)
	p := New(o.open)
	var sidecars []*fakeSidecar
	for _, port := range []uint32{80, 443} {
		r := dial(p, "c", port)
		sc := o.next()
		sc.acceptOpen()
		await(t, r)
		sidecars = append(sidecars, sc)
	}
	p.CloseAll()
	if p.liveSidecars() != 0 {
		t.Errorf("%d sidecars after CloseAll", p.liveSidecars())
	}
	for i, sc := range sidecars {
		if sc.ch.closed.Load() == 0 {
			t.Errorf("sidecar %d not closed", i)
		}
	}
}

// The race behind a rare CI failure: the sidecar answers an open and dies
// before dial looks, so dial's select finds the open result AND the sidecar's
// death ready at once and picks at random. Picking death reported a successful
// open as failed, and OpenConn re-dialled it on a brand-new sidecar — which, in
// production, silently moved an established connection to another container.
// The hook holds dial until the sidecar is dead, so both are always ready; the
// loop makes a coin flip fail the old code with certainty.
func TestPool_OpenThatSucceededIsNotRedialledWhenTheSidecarDies(t *testing.T) {
	waitForDeath := func(sc *muxSidecar) { <-sc.done }
	defer muxBeforeOpenWait.Store(nil)
	for i := 0; i < 20; i++ {
		o := newOpener(t)
		p := New(o.open)

		muxBeforeOpenWait.Store(&waitForDeath)
		r := dial(p, "c", 80)
		sc := o.next()
		sc.acceptOpen()
		_ = sc.conn.Close() // answered, then gone

		res := await(t, r)
		muxBeforeOpenWait.Store(nil)
		if res.err != nil {
			t.Fatalf("run %d: a successful open was reported as failed: %v", i, res.err)
		}
		// The connection ends with its sidecar instead of moving to a new one.
		if err := res.conn.CopyTo(&bytes.Buffer{}); err == nil {
			t.Errorf("run %d: the connection did not end with its sidecar", i)
		}
		if n := o.count(); n != 1 {
			t.Fatalf("run %d: %d sidecars opened, want 1 — the open was retried", i, n)
		}
		p.CloseAll()
	}
}
