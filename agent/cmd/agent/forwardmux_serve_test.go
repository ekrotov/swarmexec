// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"swarmexec/agent/internal/muxpool"
	"swarmexec/internal/forwardmux"
)

// These tests hang the agent's muxpool.Pool on the real muxSidecar.serve over
// two in-memory pipes — the two halves of the forward protocol meeting, with
// real TCP listeners standing in for the target service. muxpool's own tests
// run against a fake sidecar written in the test, and serve had none.

// pipeChannel is the agent's end of the sidecar's stdio.
type pipeChannel struct {
	io.Reader // sidecar stdout
	io.Writer // sidecar stdin
	closers   []func()
	once      sync.Once
}

func (c *pipeChannel) Close() {
	c.once.Do(func() {
		for _, f := range c.closers {
			f()
		}
	})
}

// sidecarRig is a Pool whose OpenFunc starts a real muxSidecar serving addr.
type sidecarRig struct {
	pool   *muxpool.Pool
	served chan error // serve's result, once per sidecar started

	mu      sync.Mutex
	started int
}

func newSidecarRig(t *testing.T, addr string) *sidecarRig {
	t.Helper()
	rig := &sidecarRig{served: make(chan error, 8)}
	rig.pool = muxpool.New(func(context.Context, string, uint32) (muxpool.Channel, error) {
		inR, inW := io.Pipe()   // pool -> sidecar
		outR, outW := io.Pipe() // sidecar -> pool
		m := &muxSidecar{addr: addr, conns: map[uint32]*muxTarget{}}
		go func() {
			err := m.serve(inR, outW)
			_ = outW.CloseWithError(io.EOF)
			rig.served <- err
		}()
		rig.mu.Lock()
		rig.started++
		rig.mu.Unlock()
		return &pipeChannel{Reader: outR, Writer: inW, closers: []func(){
			func() { _ = inW.Close() }, func() { _ = outR.Close() },
		}}, nil
	})
	t.Cleanup(rig.pool.CloseAll)
	return rig
}

func (r *sidecarRig) sidecars() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started
}

func (r *sidecarRig) open(t *testing.T) *muxpool.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := r.pool.OpenConn(ctx, "ctr", 80)
	if err != nil {
		t.Errorf("open: %v", err)
		t.FailNow()
	}
	return c
}

// drain copies what arrives on c into a buffer, closing done when it ends.
func drain(c *muxpool.Conn) (*lockedBuf, <-chan struct{}) {
	b := &lockedBuf{}
	done := make(chan struct{})
	go func() { _ = c.CopyTo(b); close(done) }()
	return b, done
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// waitFor polls until the buffer holds want, or fails.
func (l *lockedBuf) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for l.String() != want {
		if time.Now().After(deadline) {
			t.Errorf("received %d bytes, want %d (%.40q...)", len(l.String()), len(want), l.String())
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitDone(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// target is a TCP service for the sidecar to dial; handle runs per connection.
func target(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
	return lis.Addr().String()
}

func echo(c net.Conn) { defer c.Close(); _, _ = io.Copy(c, c) }

func TestMuxSidecar_OpenWriteAndTheTargetsReply(t *testing.T) {
	rig := newSidecarRig(t, target(t, echo))
	c := rig.open(t)
	got, _ := drain(c)

	for _, part := range []string{"hello ", "mux"} {
		if _, err := c.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	got.waitFor(t, "hello mux")
	c.Close()
}

// closeWrite must reach the target as EOF while the connection stays open for
// the target's reply — the request/response shape every HTTP-style forward has.
func TestMuxSidecar_CloseWriteHalfClosesTheTarget(t *testing.T) {
	addr := target(t, func(c net.Conn) {
		defer c.Close()
		body, _ := io.ReadAll(c) // returns only on the half-close
		_, _ = c.Write([]byte("got:" + string(body)))
	})
	rig := newSidecarRig(t, addr)
	c := rig.open(t)
	got, done := drain(c)

	if _, err := c.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	c.CloseWrite()
	waitDone(t, done, "the reply and the end of the connection")
	if got.String() != "got:request" {
		t.Errorf("reply = %q, want %q", got.String(), "got:request")
	}
}

// A close from the agent's side closes the target connection, and leaves the
// sidecar serving: the next connection on the same forward still works.
func TestMuxSidecar_CloseFromTheAgentClosesTheTarget(t *testing.T) {
	closed := make(chan struct{}, 4)
	addr := target(t, func(c net.Conn) {
		_, _ = io.Copy(io.Discard, c) // returns when the sidecar closes it
		closed <- struct{}{}
		_ = c.Close()
	})
	rig := newSidecarRig(t, addr)
	c := rig.open(t)
	c.Close()
	waitDone(t, closed, "the target to see its connection close")

	c2 := rig.open(t)
	defer c2.Close()
	if n := rig.sidecars(); n != 1 {
		t.Errorf("%d sidecars started, want the one to be reused", n)
	}
}

// A close from the target's side ends the agent's connection: the stream
// toward the client reaches EOF rather than hanging.
func TestMuxSidecar_CloseFromTheTargetEndsTheAgentSide(t *testing.T) {
	addr := target(t, func(c net.Conn) {
		_, _ = c.Write([]byte("bye"))
		_ = c.Close()
	})
	rig := newSidecarRig(t, addr)
	c := rig.open(t)
	got, done := drain(c)
	waitDone(t, done, "EOF after the target closed")
	if got.String() != "bye" {
		t.Errorf("payload = %q, want %q", got.String(), "bye")
	}
}

// Connections on one sidecar do not see each other's bytes.
func TestMuxSidecar_ConnectionsAreIsolated(t *testing.T) {
	rig := newSidecarRig(t, target(t, echo))
	const n = 5
	// The first open starts the sidecar; concurrent first opens may each start
	// one and discard the losers (muxpool.acquire), so the rest wait for it.
	first := rig.open(t)
	defer first.Close()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := rig.open(t)
			defer c.Close()
			got, _ := drain(c)
			msg := strings.Repeat(string(rune('a'+i)), 70000) // spans several frames
			if _, err := c.Write([]byte(msg)); err != nil {
				t.Error(err)
				return
			}
			got.waitFor(t, msg)
		}(i)
	}
	wg.Wait()
	if c := rig.sidecars(); c != 1 {
		t.Errorf("%d sidecars started, want 1 for one forward", c)
	}
}

// A target that is not listening is reported through the open, with the dial's
// reason.
func TestMuxSidecar_OpenErrCarriesTheDialReason(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close() // nothing listens here any more

	rig := newSidecarRig(t, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = rig.pool.OpenConn(ctx, "ctr", 80)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("err = %v, want the dial's reason (connection refused)", err)
	}
}

// serve ends cleanly when the agent closes the sidecar's stdin, and takes the
// target connections with it.
func TestMuxSidecar_ServeEndsOnStdinEOFAndClosesTargets(t *testing.T) {
	gone := make(chan struct{})
	addr := target(t, func(c net.Conn) {
		_, _ = io.Copy(io.Discard, c)
		close(gone)
	})
	rig := newSidecarRig(t, addr)
	_ = rig.open(t)
	rig.pool.CloseAll() // closes the channel: stdin EOF

	select {
	case err := <-rig.served:
		if err != nil {
			t.Errorf("serve = %v, want nil on EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after stdin closed")
	}
	waitDone(t, gone, "the target connection to close")
}

// A frame kind the sidecar does not know ends it with an error: carrying on
// could read a control frame as payload.
func TestMuxSidecar_UnknownFrameEndsServe(t *testing.T) {
	m := &muxSidecar{addr: "127.0.0.1:1", conns: map[uint32]*muxTarget{}}
	var in bytes.Buffer
	if err := forwardmux.Write(&in, forwardmux.Frame{Conn: 1, Kind: forwardmux.KindOpenOK}); err != nil {
		t.Fatal(err)
	}
	err := m.serve(&in, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unexpected frame") {
		t.Fatalf("serve = %v, want an unexpected-frame error", err)
	}
}

// A frame cut off mid-payload is an error, not a clean end of the forward.
func TestMuxSidecar_TruncatedFrameIsAnError(t *testing.T) {
	m := &muxSidecar{addr: "127.0.0.1:1", conns: map[uint32]*muxTarget{}}
	var full bytes.Buffer
	_ = forwardmux.Write(&full, forwardmux.Frame{Conn: 1, Kind: forwardmux.KindData, Payload: []byte("abcdef")})
	cut := full.Bytes()[:full.Len()-3]
	if err := m.serve(bytes.NewReader(cut), io.Discard); err == nil || err == io.EOF {
		t.Fatalf("serve = %v, want an error for a cut frame", err)
	}
}
