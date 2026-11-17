// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"context"
	"errors"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// A failed peer lookup must not blank the addresses for the rest of the
// session: the TUI and MCP keep one Resolver, and the first lookup may run on
// a context a view switch cancels (NEB-06).
func TestPeerAddr_FailureIsRetried(t *testing.T) {
	f := newFake()
	f.peers = []swarm.Peer{{NodeID: "node-z", Addr: "10.0.1.5:2377"}}
	f.infoErr = errors.New("manager hiccup")
	r := New(f, AddrIP)
	r.peersRetry = 0 // retry on the next call

	if got := r.peerAddr(context.Background(), "node-z"); got != "" {
		t.Fatalf("while Info fails: peerAddr = %q, want empty", got)
	}
	f.infoErr = nil
	if got := r.peerAddr(context.Background(), "node-z"); got != "10.0.1.5" {
		t.Fatalf("after recovery: peerAddr = %q, want 10.0.1.5", got)
	}
	calls := f.infoCals
	r.peerAddr(context.Background(), "node-z")
	if f.infoCals != calls {
		t.Errorf("Info called again (%d -> %d), want the success cached", calls, f.infoCals)
	}
}

// A failure is not retried on every lookup: that would cost one Info per node.
func TestPeerAddr_FailureBacksOff(t *testing.T) {
	f := newFake()
	f.infoErr = errors.New("manager hiccup")
	r := New(f, AddrIP)

	r.peerAddr(context.Background(), "a")
	r.peerAddr(context.Background(), "b")
	if f.infoCals != 1 {
		t.Errorf("Info called %d times, want 1 within the retry window", f.infoCals)
	}
}
