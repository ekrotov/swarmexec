// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
