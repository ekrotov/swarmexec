// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
)

// One node is "1 node", not "1 nodes" — a single-node swarm is the first
// thing most people point the tool at.
func TestClusterSummaryPluralisesNodes(t *testing.T) {
	for _, c := range []struct {
		nodes, agents int
		want          string
	}{
		{1, 1, "1 node · 1/1 agents"},
		{3, 3, "3 nodes · 3/3 agents"},
	} {
		if got := colorTags.ReplaceAllString(clusterSummary(c.nodes, c.agents), ""); !strings.HasPrefix(got, c.want) {
			t.Errorf("clusterSummary(%d, %d) = %q, want %q", c.nodes, c.agents, got, c.want)
		}
	}
	if got := plural(1, "node"); got != "1 node" {
		t.Errorf("plural(1) = %q", got)
	}
}
