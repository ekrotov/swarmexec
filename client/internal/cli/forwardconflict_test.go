// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
)

// The message has to name the cluster. Since forwards survive a cluster switch,
// the thing holding port 8080 is often something the operator started somewhere
// they are not currently looking — and "address already in use" never says so.
func TestPortConflictNamesTheClusterAndService(t *testing.T) {
	u := &ui{forwards: newForwardRegistry()}
	e := u.forwards.add("prod", cand("abc123456789", "api", "node-1"), 8080, 80, func() {})
	u.forwards.markActive(e.id, "127.0.0.1:8080")

	msg, clash := u.portConflict(8080)
	if !clash {
		t.Fatal("an active forward on that port must be reported as a conflict")
	}
	for _, want := range []string{"8080", "api", "prod"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

// A forward that never got its listener holds nothing, so it must not block the
// port it failed to take — otherwise one bad attempt poisons that port for the
// rest of the session.
func TestFailedForwardDoesNotHoldThePort(t *testing.T) {
	r := newForwardRegistry()
	e := r.add("prod", cand("abc", "api", "n1"), 8080, 80, func() {})
	r.markFailed(e.id, net.ErrClosed)

	if _, ok := r.byLocalPort(8080); ok {
		t.Error("a failed forward must not be reported as holding its port")
	}
}

// A forward still starting HAS asked for the port, so a second one must be
// refused rather than allowed to race it to the bind.
func TestStartingForwardAlreadyClaimsThePort(t *testing.T) {
	r := newForwardRegistry()
	r.add("prod", cand("abc", "api", "n1"), 8080, 80, func() {})

	if _, ok := r.byLocalPort(8080); !ok {
		t.Error("a starting forward must claim its port")
	}
}

// Port 0 means "any free port". It is not a port and cannot collide — treating
// it as one would make the second such forward impossible.
func TestZeroPortNeverCollides(t *testing.T) {
	r := newForwardRegistry()
	r.add("prod", cand("abc", "api", "n1"), 0, 80, func() {})

	if _, ok := r.byLocalPort(0); ok {
		t.Error("port 0 must never match")
	}
}

// …but once the kernel has chosen one, that port IS taken, and a later forward
// naming it explicitly has to be refused.
func TestKernelChosenPortIsProtected(t *testing.T) {
	r := newForwardRegistry()
	e := r.add("prod", cand("abc", "api", "n1"), 0, 80, func() {})
	r.markActive(e.id, "127.0.0.1:51234")

	if _, ok := r.byLocalPort(51234); !ok {
		t.Error("a kernel-assigned port must be protected once bound")
	}
}

// The order of bind and dial decides which failure the operator sees first. With
// the port already taken, startForwarder must come back with the local error
// immediately instead of spending the connect timeout reaching a machine it will
// never need.
func TestOccupiedPortFailsBeforeDialling(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, portStr, _ := net.SplitHostPort(held.Addr().String())
	port, _ := strconv.ParseUint(portStr, 10, 32)

	// An address that will not answer, with a connect timeout long enough that
	// dialling it first would be unmistakable in the elapsed time.
	ep := resolve.Endpoint{DialHost: "192.0.2.1", ContainerID: "abc"} // TEST-NET-1, RFC 5737
	cfg := config.Config{Port: 9443, Insecure: true}

	start := time.Now()
	fw, err := startForwarder(context.Background(), cfg, ep, forwardParams{
		address:        "127.0.0.1",
		localPort:      uint32(port),
		remotePort:     80,
		connectTimeout: 10 * time.Second,
	})
	elapsed := time.Since(start)

	if err == nil {
		fw.Close()
		t.Fatal("binding an occupied port must fail")
	}
	if !strings.Contains(err.Error(), "listen on") {
		t.Errorf("expected the local bind error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v — the dial happened before the bind", elapsed)
	}
}
