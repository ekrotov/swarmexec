// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"sync"

	"swarmexec/client/internal/resolve"
)

// nodeFanoutLimit caps how many node agents the cli dials concurrently, so a
// large swarm does not open hundreds of TLS connections at once.
const nodeFanoutLimit = 16

// volumeDeleteFanout caps how many volumes are deleted in parallel in the UI.
// Kept modest because each volume delete already fans out across its nodes (up
// to nodeFanoutLimit), so the product bounds the load on the agents.
const volumeDeleteFanout = 6

// forEachNode runs fn for every node with bounded concurrency. fn receives the
// node's index so it can write into a pre-sized result slice without locking.
func forEachNode(nodes []resolve.Node, fn func(i int, n resolve.Node)) {
	sem := make(chan struct{}, nodeFanoutLimit)
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, n resolve.Node) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i, n)
		}(i, n)
	}
	wg.Wait()
}
