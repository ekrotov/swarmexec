// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Capability tracking (review A-07).
//
// A cluster is rarely upgraded all at once. An agent older than an RPC
// answers it Unimplemented (CONTRACT.md §7), and will keep answering so until
// someone redeploys it; asking again on every refresh is pure waste. The
// stats poll learnt that first (it used to cost a full timeout per node per
// cycle); capGate is that lesson for every RPC the client asks on its own —
// per node AND per RPC, because an agent that predates the prune stats may
// well serve the volume list.

// Benching. An Unimplemented answer is a fact about the agent, so it waits
// long; any other failure of a background poll — a node that is down or slow
// — waits briefly, so a node that comes back is noticed soon.
const (
	unsupportedFor = 15 * time.Minute
	pollRetryAfter = 45 * time.Second
)

// The RPCs the gate knows. Only reads the client issues by itself are gated:
// an operator's explicit action (a prune, a volume removal) is always sent, so
// they hear the agent's own answer — Unimplemented comes back at once anyway.
const (
	rpcStats        = "Stats"
	rpcListImages   = "ListImages"
	rpcReclaimStats = "ReclaimStats"
	rpcListVolumes  = "ListVolumes"
	rpcEvents       = "WatchContainerEvents"
)

// retryAfter is how long a non-Unimplemented failure benches an RPC: only the
// periodic polls are benched for those, where asking a dead node every cycle
// costs a timeout each time. Absent means "never for an ordinary failure": a
// volume listing the operator just asked for must try, and fail, for real.
var retryAfter = map[string]time.Duration{
	rpcStats:        pollRetryAfter,
	rpcListImages:   pollRetryAfter,
	rpcReclaimStats: pollRetryAfter,
}

// capGate remembers, per agent and RPC, an answer not worth asking for again
// yet. Keyed by the node's dial host — the agent we actually reach.
type capGate struct {
	mu    sync.Mutex
	bench map[gateKey]benched
}

type gateKey struct{ host, rpc string }

type benched struct {
	until time.Time
	err   error // what the agent said; repeated to callers while benched
}

func newCapGate() *capGate { return &capGate{bench: map[gateKey]benched{}} }

// check returns the remembered failure when rpc is benched on host, nil when
// it may be asked.
func (g *capGate) check(host, rpc string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.bench[gateKey{host, rpc}]
	if !ok {
		return nil
	}
	if time.Now().After(b.until) {
		delete(g.bench, gateKey{host, rpc})
		return nil
	}
	return b.err
}

// record notes the outcome of asking rpc on host. err is the agent's answer
// (or the dial's); ctx is the CALLER's context: a call cut short because the
// caller went away (a cluster switch, a closed view) says nothing about the
// agent and is not recorded.
func (g *capGate) record(ctx context.Context, host, rpc string, err error) {
	if ctx.Err() != nil && err != nil {
		return
	}
	var wait time.Duration
	switch {
	case err == nil:
	case status.Code(err) == codes.Unimplemented:
		wait = unsupportedFor
	default:
		wait = retryAfter[rpc]
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if wait == 0 {
		delete(g.bench, gateKey{host, rpc})
		return
	}
	g.bench[gateKey{host, rpc}] = benched{until: time.Now().Add(wait), err: err}
}

// forget clears everything known about host. Called when a NEW connection to
// it comes up: the agent may have been restarted — redeployed by `init
// --force` or an image update, which is exactly how an old agent learns new
// RPCs — so its capabilities are asked again rather than assumed for the rest
// of the quarter hour.
func (g *capGate) forget(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k := range g.bench {
		if k.host == host {
			delete(g.bench, k)
		}
	}
}

// gated runs call unless rpc is benched on host — then it returns what the
// agent said last time, without a round trip — and records the outcome.
func gated[T any](c *Client, ctx context.Context, host, rpc string, call func() (T, error)) (T, error) {
	if err := c.gate.check(host, rpc); err != nil {
		var zero T
		return zero, err
	}
	v, err := call()
	c.gate.record(ctx, host, rpc, err)
	return v, err
}
