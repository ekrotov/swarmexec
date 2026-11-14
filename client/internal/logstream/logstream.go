// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package logstream streams container logs from the node agents: one
// container (Stream), one target followed across container replacements
// (Follow), or many sources at once into shared, prefixed sinks (StreamMany),
// with the format detection and level/grep filtering in front of the output
// (FilterWriter, BuildFilter).
//
// The `logs` command, the TUI's log viewer and the MCP logs tool all read
// logs through it. It has no screen of its own: output goes to the
// io.Writers in a Run.
package logstream

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
