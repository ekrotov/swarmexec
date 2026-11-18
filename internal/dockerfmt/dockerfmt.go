// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package dockerfmt renders the handful of values that look the same wherever
// Docker shows them: a short id, a missing value, a byte count.
//
// It exists because each of these was written out again in every package that
// prints a table — the cli, the MCP server, the log picker, the manager-side
// helpers and the agent's prune report. They never disagreed, which is the
// point: five identical copies is five chances for the sixth to be different,
// and an id that is 12 characters in one view and 10 in another is a bug an
// operator reports as "the ids don't match".
package dockerfmt

import "github.com/docker/go-units"

// ShortIDLen is how many characters of an id Docker shows, and therefore how
// many an operator recognises a container or task by.
const ShortIDLen = 12

// ShortID is the form `docker ps` prints an id in. An id that is already short
// (or empty) is returned unchanged rather than padded.
func ShortID(id string) string {
	if len(id) > ShortIDLen {
		return id[:ShortIDLen]
	}
	return id
}

// OrDash renders an empty value as "-", so a column stays a column.
func OrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Bytes renders a byte count for display, or "-" when it is unknown. Negative
// means unknown on purpose: a size the daemon did not report is not zero, and
// printing "0B" for it has sent people looking for a volume that was never
// measured.
func Bytes(n int64) string {
	if n < 0 {
		return "-"
	}
	return units.HumanSize(float64(n))
}
