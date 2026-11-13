// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

func version(t *testing.T, ac *Client, n resolve.Node) {
	t.Helper()
	if _, _, err := ac.Version(context.Background(), n, testTimeout); err != nil {
		t.Fatalf("version on %s: %v", n.Name, err)
	}
}

// One node, many calls, one connection — and concurrent first calls share
// the one dial instead of racing to open their own.
func TestPoolReusesOneConnectionPerNode(t *testing.T) {
	fc := newFakeCluster(t, "node-a", "node-b")
	fc.delay = 50 * time.Millisecond // keep the first dial in flight while the others arrive
	ac := New(fc.cfg)
	defer ac.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ac.Usage(context.Background(), fc.nodes, testTimeout)
			ac.Volumes(context.Background(), fc.nodes, testTimeout)
		}()
	}
	wg.Wait()
	if got := fc.dials.Load(); got != 2 {
		t.Errorf("%d dials for 2 nodes, want one each", got)
	}
	if got := fc.handshakes.Load(); got != 2 {
		t.Errorf("%d TLS handshakes for 2 nodes, want one each", got)
	}
	if got := ac.pool.size(); got != 2 {
		t.Errorf("pool holds %d connections, want 2", got)
	}
	if bad := fc.badBindings(); bad != 0 {
		t.Errorf("%d calls carried a binding for the wrong certificate", bad)
	}
}

// Two clusters never share a connection, even to a node of the same name:
// each Client is one cluster, with that cluster's tunnel.
func TestPoolIsPerClient(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	one, two := New(fc.cfg), New(fc.cfg)
	defer one.Close()
	defer two.Close()
	version(t, one, fc.nodes[0])
	version(t, two, fc.nodes[0])
	version(t, one, fc.nodes[0])
	if got := fc.dials.Load(); got != 2 {
		t.Errorf("%d dials, want one per Client", got)
	}
}

// The key covers the credentials: a different secret is a different
// connection, never one authenticated as somebody else.
func TestPoolKeyCoversTheAuthConfig(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	a, err := poolKey(fc.cfg, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	other := fc.cfg
	other.AgentSecret = "another"
	b, _ := poolKey(other, "node-a")
	c, _ := poolKey(fc.cfg, "node-b")
	if a == b || a == c {
		t.Errorf("keys collide: %q %q %q", a, b, c)
	}
	broken := fc.cfg
	broken.AgentSecretFile = "/nonexistent/secret"
	if _, err := New(broken).Conn(context.Background(), "node-a"); err == nil {
		t.Error("an unreadable secret file must fail the call, as the dial did")
	}
}

// An unused connection is closed after the idle period, and the next call
// dials again.
func TestPoolEvictsIdleConnections(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	ac := New(fc.cfg)
	defer ac.Close()
	ac.pool.idle = 50 * time.Millisecond

	version(t, ac, fc.nodes[0])
	deadline := time.Now().Add(5 * time.Second)
	for ac.pool.size() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle connection was never closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	version(t, ac, fc.nodes[0])
	if got := fc.dials.Load(); got != 2 {
		t.Errorf("%d dials, want a fresh one after the idle close", got)
	}
}

// A held connection is not evicted, however long it is held.
func TestPoolKeepsHeldConnections(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	ac := New(fc.cfg)
	defer ac.Close()
	ac.pool.idle = 20 * time.Millisecond
	conn, err := ac.Conn(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := conn.Agent().Version(context.Background(), &pb.VersionRequest{}); err != nil {
		t.Fatalf("held connection stopped working: %v", err)
	}
	conn.Close()
}

// An agent that restarts (a new self-signed certificate, the old transport
// gone) leaves a pooled connection that is no longer READY. The next call
// gets a fresh, working one, and its binding is for the NEW certificate.
func TestPoolReplacesAConnectionWhoseAgentWentAway(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	ac := New(fc.cfg)
	defer ac.Close()
	version(t, ac, fc.nodes[0])
	oldCert := fc.agents["node-a"].cert.Certificate[0]

	conn, _ := ac.Conn(context.Background(), "node-a")
	cc := conn.cc
	conn.Close()
	fc.stop("node-a")
	waitState(t, cc, connectivity.Ready, false)
	fc.start(t, "node-a")
	if string(fc.agents["node-a"].cert.Certificate[0]) == string(oldCert) {
		t.Fatal("restart should have a new certificate")
	}

	version(t, ac, fc.nodes[0])
	if got := fc.dials.Load(); got < 2 {
		t.Errorf("%d dials, want a fresh one after the agent went away", got)
	}
	if bad := fc.badBindings(); bad != 0 {
		t.Errorf("%d calls bound to the old certificate", bad)
	}
}

// waitState waits until cc is (want=true) or is no longer (want=false) in s.
func waitState(t *testing.T, cc interface {
	GetState() connectivity.State
}, s connectivity.State, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for (cc.GetState() == s) != want {
		if time.Now().After(deadline) {
			t.Fatalf("connection state stayed %v", cc.GetState())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A refused node fails at once, as the dial did, and the failure is not
// cached: once the node is back, the next call reaches it.
func TestPoolDoesNotCacheFailures(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	ac := New(fc.cfg)
	defer ac.Close()
	fc.setDown("node-a", true)
	start := time.Now()
	if _, _, err := ac.Version(context.Background(), fc.nodes[0], 10*time.Second); err == nil {
		t.Fatal("a refused node should fail")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("refusal took %v; it must fail fast, not wait out the timeout", took)
	}
	fc.setDown("node-a", false)
	version(t, ac, fc.nodes[0])
}

// A call that fails because the transport is in doubt (Unavailable — also
// what a draining agent answers) retires the connection; any other failure
// leaves it in place.
func TestPoolRetiresOnUnavailable(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	ac := New(fc.cfg)
	defer ac.Close()
	n := fc.nodes[0]
	version(t, ac, n)

	fc.agents["node-a"].setFail("Prune", status.Error(codes.NotFound, "nope"))
	if _, err := ac.Prune(context.Background(), n, &pb.PruneRequest{}, testTimeout); err == nil {
		t.Fatal("want the NotFound")
	}
	version(t, ac, n)
	if got := fc.dials.Load(); got != 1 {
		t.Errorf("%d dials after an ordinary error, want the connection kept", got)
	}

	fc.agents["node-a"].setFail("Prune", status.Error(codes.Unavailable, "agent is draining"))
	if _, err := ac.Prune(context.Background(), n, &pb.PruneRequest{}, testTimeout); status.Code(err) != codes.Unavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
	version(t, ac, n)
	if got := fc.dials.Load(); got != 2 {
		t.Errorf("%d dials after Unavailable, want the connection replaced", got)
	}
}

// A stream that breaks — here the caller abandons an exec half-way — ends
// that stream only. The connection under it keeps serving everybody else.
func TestBrokenStreamDoesNotPoisonTheConnection(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	ac := New(fc.cfg)
	defer ac.Close()
	n := fc.nodes[0]
	version(t, ac, n)

	conn, err := ac.Conn(context.Background(), "node-a")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	stream, err := conn.Agent().Exec(sctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the stream dies mid-flight
	if _, err := stream.Recv(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("stream should have failed, got %v", err)
	}
	conn.Close()

	// A failing exec through the package's own path, too.
	fc.agents["node-a"].setFail("Exec", status.Error(codes.NotFound, "no such container"))
	if _, err := ac.RunNonInteractive(context.Background(), resolve.Endpoint{DialHost: "node-a"}, []string{"true"}, testTimeout, io.Discard, io.Discard); err == nil {
		t.Fatal("exec into a missing container should fail")
	}

	version(t, ac, n)
	if got := fc.dials.Load(); got != 1 {
		t.Errorf("%d dials, want the one connection to survive both broken streams", got)
	}
}

// Suspend (a cluster switched away from) drops what is idle and keeps
// nothing new, but leaves a held connection — a port-forward — working.
// Resume caches again; Close is for good, and a closed Client still works.
func TestSuspendResumeClose(t *testing.T) {
	fc := newFakeCluster(t, "node-a", "node-b")
	ac := New(fc.cfg)
	version(t, ac, fc.nodes[0])
	held, err := ac.Conn(context.Background(), "node-b")
	if err != nil {
		t.Fatal(err)
	}

	ac.Suspend()
	if got := ac.pool.size(); got != 0 {
		t.Errorf("suspended pool holds %d connections", got)
	}
	if _, err := held.Agent().Version(context.Background(), &pb.VersionRequest{}); err != nil {
		t.Errorf("a held connection must survive a suspend: %v", err)
	}
	cc := held.cc
	held.Close()
	waitState(t, cc, connectivity.Shutdown, true)

	before := fc.dials.Load()
	version(t, ac, fc.nodes[0])
	version(t, ac, fc.nodes[0])
	if got := fc.dials.Load() - before; got != 2 {
		t.Errorf("suspended: %d dials for 2 calls, want no caching", got)
	}

	ac.Resume()
	before = fc.dials.Load()
	version(t, ac, fc.nodes[0])
	version(t, ac, fc.nodes[0])
	if got := fc.dials.Load() - before; got != 1 {
		t.Errorf("resumed: %d dials for 2 calls, want one", got)
	}

	ac.Close()
	ac.Resume() // no-op after Close
	if got := ac.pool.size(); got != 0 {
		t.Errorf("closed pool holds %d connections", got)
	}
	version(t, ac, fc.nodes[0]) // still works, uncached
}

// A caller that waited for somebody else's dial, which ran out of ITS time,
// tries on its own budget rather than inheriting a deadline it never had.
func TestWaiterOutlivesALeaderThatTimedOut(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	fc.delay = 200 * time.Millisecond
	ac := New(fc.cfg)
	defer ac.Close()

	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	leaderErr := make(chan error, 1)
	go func() {
		_, err := ac.Conn(short, "node-a")
		leaderErr <- err
	}()
	time.Sleep(10 * time.Millisecond) // let the short one start the dial
	conn, err := ac.Conn(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("the patient caller failed: %v", err)
	}
	conn.Close()
	if err := <-leaderErr; err == nil {
		t.Error("the short dial should have timed out")
	}
}

// A refusal seen by the dial everyone waited on is everyone's answer: a
// refused node is not dialled once per waiter.
func TestWaitersShareARefusal(t *testing.T) {
	fc := newFakeCluster(t, "node-a")
	fc.setDown("node-a", true)
	fc.delay = 50 * time.Millisecond
	ac := New(fc.cfg)
	defer ac.Close()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ac.Conn(context.Background(), "node-a"); err == nil {
				t.Error("a refused node should fail")
			}
		}()
	}
	wg.Wait()
	if got := fc.dials.Load(); got > 2 {
		t.Errorf("%d dials for 5 concurrent callers to a refused node", got)
	}
}
