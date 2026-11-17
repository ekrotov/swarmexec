// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"reflect"
	"testing"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
)

func TestStatsTargets(t *testing.T) {
	cands := []resolve.Candidate{
		{Service: "web", NodeID: "n1", ContainerID: "c1"},
		{Service: "hidden", NodeID: "n2", ContainerID: "c2"},
		{Service: "web", NodeID: "n1", ContainerID: "c3"},
	}
	nodes := []resolve.Node{{ID: "n1"}, {ID: "n2"}, {ID: "n3"}}
	mine, ask := StatsTargets(cands, nodes, func(svc string) bool { return svc == "web" })
	if len(mine) != 2 || mine[0].ContainerID != "c1" || mine[1].ContainerID != "c3" {
		t.Errorf("mine = %+v", mine)
	}
	if !reflect.DeepEqual(ask, []resolve.Node{{ID: "n1"}}) {
		t.Errorf("ask = %+v, want only the node running a wanted container", ask)
	}
	if mine, ask := StatsTargets(cands, nodes, func(string) bool { return false }); mine != nil || ask != nil {
		t.Errorf("nothing wanted = %+v / %+v", mine, ask)
	}
}

func TestStatsRows(t *testing.T) {
	full := agentcall.ContainerUsage{CPUReady: true, CPUPercent: 50, MemBytes: 1 << 20, Health: "healthy"}
	memOnly := agentcall.ContainerUsage{MemBytes: 1 << 20}
	mine := []resolve.Candidate{
		{Service: "web", Slot: 1, NodeName: "a", ContainerID: "full0123456789"},
		{Service: "web", Slot: 2, NodeName: "a", ContainerID: "memonly"},
		{Service: "web", Slot: 3, NodeName: "a", ContainerID: "cold"},
		{Service: "web", Slot: 4, NodeName: "", ContainerID: "down"},
	}
	usage := map[string]agentcall.ContainerUsage{"full0123456789": full, "memonly": memOnly}
	got := StatsRows(mine, usage, map[string]bool{"a": true})
	want := []StatsRow{
		{Service: "web", Slot: 1, Node: "a", Container: "full01234567", CPU: FormatCPUUsage(full),
			Memory: FormatMemUsage(full), Health: "healthy"},
		{Service: "web", Slot: 2, Node: "a", Container: "memonly", CPU: StatsWarmingUp,
			Memory: FormatMemUsage(memOnly), Note: StatsWarmingUpNote},
		{Service: "web", Slot: 3, Node: "a", Container: "cold", CPU: StatsWarmingUp, Memory: StatsWarmingUp, Note: StatsWarmingUpNote},
		{Service: "web", Slot: 4, Node: "-", Container: "down", CPU: StatsNoAgent, Memory: StatsNoAgent, Note: StatsNoAgentNote},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows =\n%+v\nwant\n%+v", got, want)
	}
	if rows := StatsRows(nil, nil, nil); rows == nil || len(rows) != 0 {
		t.Errorf("no containers = %#v, want an empty (not nil) list", rows)
	}
}
