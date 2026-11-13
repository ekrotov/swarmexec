// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package fanout runs one operation per item with bounded concurrency and stops
// early when the caller's context ends.
//
// It is the cli's node fan-out (one agent RPC per swarm node), pulled out of
// package cli because it knows nothing about the TUI: the first piece of the
// data layer that stands on its own, and a seed for the agent-call package the
// architecture review proposes (A-01).
package fanout

import (
	"context"
	"errors"
	"sync"

	"swarmexec/client/internal/resolve"
)

// NodeLimit caps how many node agents the cli dials concurrently, so a large
// swarm does not open hundreds of TLS connections at once.
const NodeLimit = 16

// ErrSkipped marks an item Each never started because the context ended
// first. Callers that collect a per-item result pre-fill it with this, so a
// node that was never asked cannot read as a node that answered "nothing".
var ErrSkipped = errors.New("not attempted: the operation was cancelled")

// Each runs fn for every item, at most limit at a time. fn receives the item's
// index so it can write into a pre-sized result slice without locking.
//
// When ctx ends, Each starts no further items and returns ctx.Err(); fn is not
// called for the rest. It still waits for the calls already running — they
// may be writing into the caller's result slice — but those were handed ctx
// and so end promptly. This is what lets a refresh whose result is going to be
// discarded stop waiting for the slowest of sixteen nodes.
//
// Each returns nil once every item was started, even if ctx ended while the
// last ones ran: every result slot was then written by fn, and fn saw ctx.
//
// A limit below 1 is treated as 1.
func Each[T any](ctx context.Context, items []T, limit int, fn func(ctx context.Context, i int, item T)) error {
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	defer wg.Wait()
	for i, it := range items {
		// Checked before the semaphore as well: a select picks at random among
		// ready cases, so a free slot could otherwise win over a done context.
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(ctx, i, it)
		}()
	}
	return nil
}

// Nodes is Each over swarm nodes at NodeLimit.
func Nodes(ctx context.Context, nodes []resolve.Node, fn func(ctx context.Context, i int, n resolve.Node)) error {
	return Each(ctx, nodes, NodeLimit, fn)
}
