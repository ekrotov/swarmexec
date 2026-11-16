// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"strings"
	"testing"
	"time"
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

// The agent count says what it means: all there is aqua, short of the node
// count is yellow, none at all is red and names the fix.
func TestClusterSummaryColoursTheAgentCount(t *testing.T) {
	for _, c := range []struct {
		nodes, agents int
		color, hint   string
	}{
		{3, 3, "[aqua]3/3", ""},
		{3, 2, "[yellow]2/3", ""},
		{3, 0, "[red]0/3", "swarmexec init"},
	} {
		got := clusterSummary(c.nodes, c.agents)
		if !strings.Contains(got, c.color) {
			t.Errorf("clusterSummary(%d, %d) = %q, want %q", c.nodes, c.agents, got, c.color)
		}
		if has := strings.Contains(got, "swarmexec init"); has != (c.hint != "") {
			t.Errorf("clusterSummary(%d, %d) = %q: init hint present = %v", c.nodes, c.agents, got, has)
		}
	}
}

// A node whose agent did not answer has an unknown volume count, not zero.
func TestNodeVolumeCountUnknownIsADash(t *testing.T) {
	ex := &nodesExtra{}
	if got := ex.volumes("n1"); got != "…" {
		t.Errorf("while loading: %q", got)
	}
	ex.volsLoaded, ex.volCounts = true, map[string]int{"n1": 0, "n2": 4}
	for host, want := range map[string]string{"n1": "0", "n2": "4", "n3": "–"} {
		if got := ex.volumes(host); got != want {
			t.Errorf("volumes(%s) = %q, want %q", host, got, want)
		}
	}
}

// The node detail shows an agent error as its first cause and what to run,
// not gRPC's nested transport text over three wrapped lines.
func TestShortAgentError(t *testing.T) {
	dial := errors.New(`cannot reach agent on 192.168.188.22:9443 (mTLS/connection error: connection error: desc = "transport: Error while dialing: dial tcp 192.168.188.22:9443: connect: connection refused")`)
	want := "agent not reachable on 192.168.188.22:9443: connection refused — swarmexec doctor"
	if got := shortAgentError(dial); got != want {
		t.Errorf("shortAgentError(dial) = %q, want %q", got, want)
	}
	multi := errors.New("agent rejected the shared secret.\n  Most likely …")
	if got := shortAgentError(multi); got != "agent rejected the shared secret." {
		t.Errorf("shortAgentError(multi-line) = %q", got)
	}
	section := reclaimSection(nil, dial, false, time.Now())
	if !strings.Contains(section, want) || strings.Contains(section, "transport") {
		t.Errorf("reclaim section shows %q", section)
	}
}
