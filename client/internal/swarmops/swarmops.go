// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package swarmops is the client's manager-side logic: what swarmexec reads
// from and changes on the Swarm manager through the Docker API, without a
// screen attached (review A-01 step 2).
//
// It holds the inspect and diff model (inspect.go), the service-spec editors'
// current*/Set* round trips and their parsers (serviceedit.go), networks,
// secrets and configs with their member services (netsec.go), the overlay/
// underlay overlap analysis, the placement diagnosis, stack removal, secret
// rotation, the agent service with its prune schedule, and the registry
// update check.
//
// What it no longer holds is anything that asks a node agent. Six of its
// files did, which made "manager-side logic" false for a third of the package
// and put the cluster readings — the topology read, per-node stats, node
// endpoints, the security gathering — in the same place as the service
// editors. Those live in clusterread now (review ARC-02), which imports this
// package; the dependency runs one way.
//
// Everything takes a swarmapi.Client (or a narrower subset), so it runs
// against a plain Go fake. The cli package and its TUI call it; it never calls
// back: swarmops imports neither cli nor tview/tcell, and no longer agentcall
// either (boundary_test.go). The agent side lives in agentcall, the error
// wording in agenterr.
package swarmops
