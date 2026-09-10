// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"sort"
	"strings"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/secscan"
)

// noStackLabel names the bucket for services that carry no stack label — one
// created with `docker service create` rather than `docker stack deploy`. They
// are grouped rather than hidden, so the tab still accounts for every service.
const noStackLabel = "(no stack)"

// stackRow is one row of the Stacks tab: a stack and the aggregate state of the
// services in it.
type stackRow struct {
	Name     string            // stack name, or noStackLabel
	Services []resolve.Service // members, sorted by name
	Running  int               // summed running tasks
	Desired  int               // summed desired tasks
	Updating int               // services in an active rolling update
	Risky    int               // services with an actionable security finding
	Ports    int               // services publishing at least one port
}

// groupByStack turns a flat service list into stack rows. Stacks sort by name;
// the unstacked bucket sorts last so it never pushes real stacks down the list.
// Swarm has no stack object — the com.docker.stack.namespace label is the only
// link — so this is derived entirely from the service list already fetched.
func groupByStack(svcs []resolve.Service) []stackRow {
	byName := map[string]*stackRow{}
	for _, s := range svcs {
		name := strings.TrimSpace(s.Stack)
		if name == "" {
			name = noStackLabel
		}
		row := byName[name]
		if row == nil {
			row = &stackRow{Name: name}
			byName[name] = row
		}
		row.Services = append(row.Services, s)
		row.Running += s.Running
		row.Desired += s.Desired
		if _, _, active := updateStatusLabel(s.UpdateState); active {
			row.Updating++
		}
		if secscan.Actionable(s.Risks) {
			row.Risky++
		}
		if s.Ports != "" {
			row.Ports++
		}
	}

	out := make([]stackRow, 0, len(byName))
	for _, row := range byName {
		sort.Slice(row.Services, func(i, j int) bool { return row.Services[i].Name < row.Services[j].Name })
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		// The unstacked bucket always sinks to the bottom.
		if (out[i].Name == noStackLabel) != (out[j].Name == noStackLabel) {
			return out[j].Name == noStackLabel
		}
		return out[i].Name < out[j].Name
	})
	return out
}
