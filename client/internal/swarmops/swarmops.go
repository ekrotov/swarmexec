// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package swarmops is the client's manager-side logic: everything swarmexec
// reads from or changes on the Swarm manager through the Docker API, without a
// screen attached (review A-01 step 2).
//
// It holds the inspect and diff model (inspect.go), the service-spec editors'
// current*/Set* round trips and their parsers (serviceedit.go), networks,
// secrets and configs with their member services (netsec.go), nodes, labels
// and availability (nodes.go), the placement diagnosis (placement.go), stack
// removal, secret rotation, the agent service and its prune schedule, the
// registry update check and the security-report gathering — plus the
// readings both front-ends (the TUI and the MCP server) render the same way:
// usage against limits, a service's health tally, an agent's version verdict,
// the topology read (readings.go).
//
// Everything takes a swarmapi.Client (or a narrower subset), so it runs
// against a plain Go fake. The cli package and its TUI call it; it never calls
// back: swarmops imports neither cli nor tview/tcell (boundary_test.go). The
// agent side lives in agentcall, the error wording in agenterr.
package swarmops

import ()
