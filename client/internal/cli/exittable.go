// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"strings"
)

// Markers around the generated rows of the exit-code table in the site's
// configuration page.
const (
	exitTableBegin = "<!-- exit-table:begin"
	exitTableEnd   = "<!-- exit-table:end -->"
)

// ApplyExitTable replaces the rows between the exit-table markers of page
// with the ones rendered from exitTable, leaving the markers in place.
func ApplyExitTable(page string) (string, error) {
	i := strings.Index(page, exitTableBegin)
	j := strings.Index(page, exitTableEnd)
	if i < 0 || j < i {
		return "", errors.New("exit-table markers not found")
	}
	nl := strings.Index(page[i:], "\n")
	if nl < 0 || i+nl > j {
		return "", errors.New("exit-table begin marker is malformed")
	}
	start := i + nl + 1
	// The end marker keeps its indentation: cut back to the line start.
	end := strings.LastIndex(page[:j], "\n") + 1
	return page[:start] + ExitTableHTML() + page[end:], nil
}
