// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/pb"
)

// endpointsAgent answers NetworkEndpoints with a fixed view.
type endpointsAgent struct {
	pb.UnimplementedAgentServer
	resp *pb.NetworkEndpointsResponse
}

func (a *endpointsAgent) NetworkEndpoints(context.Context, *pb.NetworkEndpointsRequest) (*pb.NetworkEndpointsResponse, error) {
	return a.resp, nil
}

const (
	webTask  = "web.1.0000000000000000000000001"
	goneTask = "web.2.0000000000000000000000002"
)

// endpointsUI is the operator's case on three nodes: web runs once on node-a
// (10.0.1.5); node-a and node-b still list a departed web task (10.0.1.30,
// owned by node-b) in their networkdb; node-c runs an agent too old to ask.
func endpointsUI(t *testing.T) (*ui, *listManager) {
	t.Helper()
	m := &listManager{networks: []network.Summary{subnetNet("dbnet", "10.0.1.0/24")}}
	m.nodes = []swarm.Node{readyNode("n1", "node-a"), readyNode("n2", "node-b"), readyNode("n3", "node-c")}
	var tk swarm.Task
	tk.NodeID, tk.Status.State = "n1", swarm.TaskStateRunning
	tk.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "c-web"}
	tk.NetworksAttachments = []swarm.NetworkAttachment{{Network: swarm.Network{ID: "id-dbnet"}, Addresses: []netip.Prefix{netip.MustParsePrefix("10.0.1.5/24")}}}
	m.tasks = []swarm.Task{tk}

	networkdb := []*pb.NetworkServiceEndpoints{
		{Name: "", Tasks: []*pb.NetworkTaskEndpoint{{Name: "dbnet-endpoint", EndpointId: "lb-a", EndpointIp: "10.0.1.3", HostIp: "node-a"}}},
		{Name: "web", Vip: "10.0.1.2/24", Tasks: []*pb.NetworkTaskEndpoint{
			{Name: webTask, EndpointId: "e-web", EndpointIp: "10.0.1.5", HostIp: "node-a"},
			{Name: goneTask, EndpointId: "e-gone", EndpointIp: "10.0.1.30", HostIp: "node-b"},
		}},
	}
	view := func(locals ...*pb.NetworkLocalEndpoint) *endpointsAgent {
		return &endpointsAgent{resp: &pb.NetworkEndpointsResponse{Networks: []*pb.NetworkEndpointsInfo{
			{Id: "id-dbnet", Name: "dbnet", LocalEndpoints: locals, Services: networkdb},
		}}}
	}
	cfg := startAgents(t, map[string]pb.AgentServer{
		"node-a": view(
			&pb.NetworkLocalEndpoint{ContainerId: "c-web", Name: webTask, EndpointId: "e-web", Ipv4Address: "10.0.1.5/24", MacAddress: "02:42:0a:00:01:05"},
			&pb.NetworkLocalEndpoint{ContainerId: "lb-dbnet", Name: "dbnet-endpoint", EndpointId: "lb-a", Ipv4Address: "10.0.1.3/24", MacAddress: "02:42:0a:00:01:03", LoadBalancer: true},
		),
		"node-b": view(),
		"node-c": &volumeAgent{},
	})
	u := listUI(t, m)
	onLoop(t, u, func() {
		u.r = resolve.New(m, resolve.AddrHostname)
		u.cfg = cfg
		u.agents = agentcall.New(cfg)
		u.f = &uiFlags{connectTimeout: 3 * time.Second}
	})
	return u, m
}

// Entering the tab fills CONTAINERS from the agents: running tasks, the
// addresses the nodes know, the stale one, and "+1 n/a" for the old agent.
func TestNetworksTabShowsEndpointCountsAndStale(t *testing.T) {
	u, _ := endpointsUI(t)
	nt := u.netTab()
	onLoop(t, u, func() { nt.enter() })
	waitForTable(t, u, nt.table, "1 · 2 addr ⚠1 stale +1 n/a")
	var fg tcell.Color
	onLoop(t, u, func() { fg, _, _ = nt.table.GetCell(1, 7).Style.Decompose() })
	if fg != tcell.ColorYellow {
		t.Errorf("CONTAINERS colour = %v, want yellow", fg)
	}

	var header, list string
	onLoop(t, u, func() {
		n, _ := nt.selected()
		nt.showMembers(n)
	})
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(list, "10.0.1.30") || !strings.Contains(header, "addresses") {
		if time.Now().After(deadline) {
			t.Fatalf("overlay never showed the endpoints:\nheader %q\nlist:\n%s", header, list)
		}
		time.Sleep(10 * time.Millisecond)
		onLoop(t, u, func() {
			v := nt.members
			header = v.header.GetText(true)
			var b strings.Builder
			for i := 0; i < v.list.GetItemCount(); i++ {
				main, _ := v.list.GetItemText(i)
				b.WriteString(main + "\n")
			}
			list = b.String()
		})
	}
	for _, want := range []string{"1 running · 2 addresses · 1 LB", "⚠ 1 stale — listed below", "? 1 node(s) unknown"} {
		if !strings.Contains(header, want) {
			t.Errorf("header lacks %q: %q", want, header)
		}
	}
	for _, want := range []string{
		"node-a  running 1 · local 1 + 1 LB · MACs 2 · networkdb 3",
		"node-b  running 0 · local 0 + 0 LB · MACs 0 · networkdb 3",
		"node-c  [gray]agent too old — update the agent image",
		"stale candidates (1)",
		"10.0.1.30", goneTask, "on node-b", "seen by node-a,node-b",
		"docker network disconnect -f dbnet <endpoint name>",
	} {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q:\n%s", want, list)
		}
	}
}

// What the cell says in every load state.
func TestEndpointNoteStates(t *testing.T) {
	u := listUI(t, &listManager{})
	nt := u.netTab()
	swarmNet := swarmops.Network{ID: "id", Scope: "swarm"}
	cases := []struct {
		name  string
		st    netEndpointsState
		n     swarmops.Network
		text  string
		color tcell.Color
	}{
		{"local network", netEndpointsState{loaded: true}, swarmops.Network{ID: "id", Scope: "local"}, "", 0},
		{"never loaded", netEndpointsState{}, swarmNet, "", 0},
		{"loading", netEndpointsState{loading: time.Now()}, swarmNet, "· …", 0},
		{"load failed", netEndpointsState{loaded: true, err: errors.New("x")}, swarmNet, "· n/a", tcell.ColorGray},
		{"unknown network, all answered", netEndpointsState{loaded: true}, swarmNet, "· 0 addr", 0},
		{"unknown network, some failed", netEndpointsState{loaded: true, failed: 1}, swarmNet, "· n/a", tcell.ColorGray},
		{"nobody answered", netEndpointsState{loaded: true, byNet: map[string]*swarmops.NetEndpoints{"id": {Unknown: 2}}}, swarmNet, "· n/a", tcell.ColorGray},
		{"clean", netEndpointsState{loaded: true, byNet: map[string]*swarmops.NetEndpoints{"id": {Answered: 2, Addrs: 4}}}, swarmNet, "· 4 addr", 0},
		{"some nodes unknown", netEndpointsState{loaded: true, byNet: map[string]*swarmops.NetEndpoints{"id": {Answered: 1, Unknown: 2, Addrs: 4}}}, swarmNet, "· 4 addr +2 n/a", 0},
		{"duplicates", netEndpointsState{loaded: true, byNet: map[string]*swarmops.NetEndpoints{"id": {Answered: 2, Addrs: 4, DupIPs: []swarmops.Duplicate{{}}, DupMACs: []swarmops.Duplicate{{}}}}}, swarmNet, "· 4 addr ⚠2 dup", tcell.ColorYellow},
	}
	for _, c := range cases {
		var text string
		var color tcell.Color
		onLoop(t, u, func() {
			*nt.endpoints() = c.st
			text, color = nt.endpointNote(c.n)
		})
		if text != c.text || color != c.color {
			t.Errorf("%s: note = %q/%v, want %q/%v", c.name, text, color, c.text, c.color)
		}
	}
}

func TestEndpointSummaryAndBlock(t *testing.T) {
	n := swarmops.Network{ID: "id", Scope: "swarm"}
	sum := func(st netEndpointsState) string { return endpointSummary(&st, n) }
	for _, c := range []struct {
		st   netEndpointsState
		want string
	}{
		{netEndpointsState{loading: time.Now()}, loadingText},
		{netEndpointsState{}, "not loaded"},
		{netEndpointsState{loaded: true, err: errors.New("no nodes")}, "? no nodes"},
		{netEndpointsState{loaded: true}, "no node has joined"},
		{netEndpointsState{loaded: true, failed: 1}, "some agents could not answer"},
		{netEndpointsState{loaded: true, byNet: map[string]*swarmops.NetEndpoints{"id": {Unknown: 1}}}, "no agent could answer"},
		{netEndpointsState{loaded: true, byNet: map[string]*swarmops.NetEndpoints{"id": {Answered: 1, DupIPs: []swarmops.Duplicate{{}}}}}, "⚠ 1 duplicate"},
	} {
		if got := sum(c.st); !strings.Contains(got, c.want) {
			t.Errorf("summary = %q, want %q in it", got, c.want)
		}
	}

	if endpointBlock(nil, "net", 90) != nil {
		t.Error("no analysis, no block")
	}
	e := &swarmops.NetEndpoints{
		Nodes: []swarmops.NodeEndpoints{
			{Node: "a", State: swarmops.NodeEndpointsOK, Expected: 2, Containers: 3, MACs: 3},
			{Node: "b", State: swarmops.NodeNotOnNetwork, Expected: 1},
			{Node: "idle", State: swarmops.NodeNotOnNetwork},
			{Node: "c", State: swarmops.NodeUnreachable, Err: "dial refused"},
			{Node: "d", State: swarmops.NodeNotPermitted},
		},
		Stale:   []swarmops.StaleEndpoint{{IP: "10.0.0.9", Name: "ep-x", Orphan: true, Local: true, Owner: "a", SeenBy: []string{"a"}}},
		DupIPs:  []swarmops.Duplicate{{Addr: "10.0.0.5", Endpoints: []string{"x", "y"}}},
		DupMACs: []swarmops.Duplicate{{Addr: "02:42:0a:00:00:05", Endpoints: []string{"x", "y"}}},
	}
	block := strings.Join(endpointBlock(e, "net", 90), "\n")
	for _, want := range []string{
		"(local ≠ running)", "running 1, but the node reports no such network", "agent unreachable: dial refused",
		"not permitted (agent policy: network.inspect)", "endpoint without container", "duplicate IPs", "duplicate MACs",
		"10.0.0.5", "fix:",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}
	if strings.Contains(block, "idle") {
		t.Errorf("a node off the network with nothing expected is listed:\n%s", block)
	}
	for _, l := range endpointBlock(e, "net", 90) {
		if w := tview.TaggedStringWidth(l); w > 120 {
			t.Errorf("row too wide (%d): %q", w, l)
		}
	}
}
