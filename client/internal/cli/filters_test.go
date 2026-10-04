// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"sort"

	"github.com/moby/moby/client"
)

// filterValues lists the values a filter term is set to, sorted — what
// filters.Args.Get returned before client.Filters became a plain map.
func filterValues(f client.Filters, term string) []string {
	var out []string
	for v := range f[term] {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
