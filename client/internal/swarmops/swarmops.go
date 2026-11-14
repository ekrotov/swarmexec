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
// registry update check and the security-report gathering.
//
// Everything takes a swarmapi.Client (or a narrower subset), so it runs
// against a plain Go fake. The cli package and its TUI call it; it never calls
// back: swarmops imports neither cli nor tview/tcell (boundary_test.go). The
// agent side lives in agentcall, the error wording in agenterr.
package swarmops

import (
	units "github.com/docker/go-units"
)

// shortID is the 12-character form Docker shows ids in.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// orDash renders an empty value as "-".
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// humanBytes renders a byte count for display, or "-" when unknown (negative).
func humanBytes(n int64) string {
	if n < 0 {
		return "-"
	}
	return units.HumanSize(float64(n))
}
