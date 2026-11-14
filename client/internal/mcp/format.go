// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
