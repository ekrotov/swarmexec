// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package clusterread

import (
	"errors"
	"reflect"
	"testing"

	"swarmexec/client/internal/agentcall"
	"swarmexec/internal/pb"
)

// Focused tests for the steps analyzeNetwork is built from; the end-to-end
// behaviour stays pinned by TestAnalyzeEndpoints / TestAnalyzeEndpointsCases.

func TestCollectLive(t *testing.T) {
	tasks := []TaskAddr{
		taddr("net", "n1", "web", true, "10.0.0.5", "10.0.0.6"),
		taddr("net", "n1", "web", true, "10.0.0.7"),
		taddr("net", "n2", "web", false, "10.0.0.8"), // starting: live address, not expected
		taddr("other", "n1", "db", true, "10.9.0.5"), // another network
	}
	l := collectLive("net", tasks)
	if l.running != 2 {
		t.Errorf("running = %d, want 2", l.running)
	}
	if !reflect.DeepEqual(l.expected, map[string]int{"n1": 2}) {
		t.Errorf("expected = %v", l.expected)
	}
	want := map[string]bool{"10.0.0.5": true, "10.0.0.6": true, "10.0.0.7": true, "10.0.0.8": true}
	if !reflect.DeepEqual(l.ips, want) {
		t.Errorf("ips = %v", l.ips)
	}
}

func TestCollectVIPs(t *testing.T) {
	svc := func(vip string) *pb.NetworkServiceEndpoints { return &pb.NetworkServiceEndpoints{Name: "s", Vip: vip} }
	nodes := []agentcall.NodeNetworkEndpoints{
		nodeAns("n1", "a", "h1", &pb.NetworkEndpointsInfo{Id: "net", Services: []*pb.NetworkServiceEndpoints{svc("10.0.0.2/24"), svc("")}}),
		nodeAns("n2", "b", "h2", &pb.NetworkEndpointsInfo{Id: "other", Services: []*pb.NetworkServiceEndpoints{svc("10.9.0.2/24")}}),
		nodeErr("n3", "c", errors.New("down")),
		nodeAns("n4", "d", "h4", &pb.NetworkEndpointsInfo{Id: "net", Services: []*pb.NetworkServiceEndpoints{svc("10.0.0.3")}}),
	}
	got := collectVIPs("net", nodes)
	if !reflect.DeepEqual(got, map[string]bool{"10.0.0.2": true, "10.0.0.3": true}) {
		t.Errorf("vips = %v", got)
	}
}

func TestTallyStates(t *testing.T) {
	var nodes []NodeEndpoints
	for _, s := range []NodeEndpointState{NodeEndpointsOK, NodeNotOnNetwork, NodeNetworkError, NodeAgentTooOld,
		NodeAgentTooOld, NodeNotPermitted, NodeUnreachable, NodeNotReached} {
		nodes = append(nodes, NodeEndpoints{State: s})
	}
	a, u, old := tallyStates(nodes)
	if a != 2 || u != 6 || old != 2 {
		t.Errorf("answered=%d unknown=%d tooOld=%d, want 2 6 2", a, u, old)
	}
	if a, u, old := tallyStates(nil); a+u+old != 0 {
		t.Error("no nodes must tally zero")
	}
}

func TestAddrBookNote(t *testing.T) {
	b := addrBook{}
	b.note("", "k0", "ignored")
	b.note("10.0.0.5", "ep1", "web.1")
	b.note("10.0.0.5", "0123456789abcdef", "") // unnamed: short key
	b.note("10.0.0.5", "ep1", "web.1")         // same endpoint twice: one entry
	want := addrBook{"10.0.0.5": {"ep1": "web.1", "0123456789abcdef": "0123456789ab"}}
	if !reflect.DeepEqual(b, want) {
		t.Errorf("book = %v", b)
	}
	if d := duplicates(b); len(d) != 1 || !reflect.DeepEqual(d[0].Endpoints, []string{"0123456789ab", "web.1"}) {
		t.Errorf("duplicates = %+v", d)
	}
}

func TestStaleSetMerge(t *testing.T) {
	m := staleSet{}
	// networkdb sighting from b: owner known by host IP only.
	m.add("k", StaleEndpoint{IP: "10.0.0.30", Name: "", Service: "web", EndpointID: "ep", OwnerIP: "192.168.0.2"}, "b")
	// local sighting on a: fills name, MAC, owner, local/orphan flags.
	m.add("k", StaleEndpoint{IP: "10.0.0.30", Name: "web.3.x", MAC: "02:42", Owner: "a", Local: true, Orphan: true,
		OwnerIP: "192.168.9.9"}, "a")
	m.add("k", StaleEndpoint{IP: "10.0.0.30"}, "a") // same node again: SeenBy unchanged
	// owner never matched: falls back to the host IP.
	m.add("k2", StaleEndpoint{IP: "10.0.0.9", Name: "z", OwnerIP: "192.168.0.7"}, "c")

	got := m.sorted()
	want := []StaleEndpoint{
		{IP: "10.0.0.9", Name: "z", Owner: "192.168.0.7", OwnerIP: "192.168.0.7", SeenBy: []string{"c"}},
		{IP: "10.0.0.30", Name: "web.3.x", Service: "web", EndpointID: "ep", MAC: "02:42", Owner: "a",
			OwnerIP: "192.168.0.2", SeenBy: []string{"a", "b"}, Local: true, Orphan: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sorted =\n%+v\nwant\n%+v", got, want)
	}
	if (staleSet{}).sorted() != nil {
		t.Error("an empty set must sort to nil")
	}
}

func TestNetScanAddrCounts(t *testing.T) {
	s := newNetScan("net", liveTasks{}, nil, ownerIndex{})
	s.taskIPs["10.0.0.5"], s.taskIPs["10.0.0.6"], s.taskIPs["10.0.0.3"] = true, true, true
	s.addLB("10.0.0.3") // an LB address also filed as a task: counted as LB only
	s.addLB("10.0.0.4")
	s.addLB("") // ignored
	if tasks, lbs := s.addrCounts(); tasks != 2 || lbs != 2 {
		t.Errorf("addrCounts = %d, %d; want 2, 2", tasks, lbs)
	}
}

func TestNetScanDBTask(t *testing.T) {
	live := liveTasks{ips: map[string]bool{"10.0.0.5": true}}
	owners := ownerIndex{byAddr: map[string]string{"192.168.0.1": "a"}, ids: map[string]string{"nodeB-id": "b"}}
	s := newNetScan("net", live, map[string]bool{"10.0.0.2": true}, owners)
	peers := map[string]string{"192.168.0.2": "nodeB"}
	gone := tname("web", 3, 3)
	cases := []struct {
		name, service string
		task          *pb.NetworkTaskEndpoint
		stale         bool
	}{
		{"live task", "web", dbTask(tname("web", 1, 1), "10.0.0.5", "192.168.0.1"), false},
		{"vip", "web", dbTask("vip", "10.0.0.2", "192.168.0.1"), false},
		{"lb under empty service", "", lbTask("net", "10.0.0.3", "192.168.0.1"), false},
		{"standalone under empty service", "", dbTask("loner", "10.0.0.50", "192.168.0.1"), false},
		{"no address", "web", dbTask(tname("web", 4, 4), "", "192.168.0.1"), false},
		{"departed task", "web", dbTask(gone, "10.0.0.30", "192.168.0.2"), true},
	}
	for _, c := range cases {
		if _, stale := s.dbTask(c.service, c.task, peers, "a"); stale != c.stale {
			t.Errorf("%s: stale = %v, want %v", c.name, stale, c.stale)
		}
	}
	if !reflect.DeepEqual(s.lbIPs, map[string]bool{"10.0.0.3": true}) {
		t.Errorf("lbIPs = %v", s.lbIPs)
	}
	if !reflect.DeepEqual(s.taskIPs, map[string]bool{"10.0.0.5": true, "10.0.0.2": true, "10.0.0.30": true}) {
		t.Errorf("taskIPs = %v", s.taskIPs)
	}
	st := s.stale.sorted()
	if len(st) != 1 || st[0].Name != gone || st[0].Owner != "b" || st[0].Service != "web" || !reflect.DeepEqual(st[0].SeenBy, []string{"a"}) {
		t.Errorf("stale = %+v", st)
	}
}

func TestNetScanNodeStates(t *testing.T) {
	s := newNetScan("net", liveTasks{expected: map[string]int{"n1": 3}}, nil, ownerIndex{})
	if ne := s.scanNode(nodeErr("n1", "a", errors.New("dial"))); ne.State != NodeUnreachable || ne.Err != "dial" || ne.Expected != 3 {
		t.Errorf("error node = %+v", ne)
	}
	if ne := s.scanNode(nodeAns("n2", "b", "h", &pb.NetworkEndpointsInfo{Id: "other"})); ne.State != NodeNotOnNetwork {
		t.Errorf("absent network = %+v", ne)
	}
	if ne := s.scanNode(nodeAns("n3", "c", "h", &pb.NetworkEndpointsInfo{Id: "net", Error: "boom"})); ne.State != NodeNetworkError || ne.Err != "boom" {
		t.Errorf("network error = %+v", ne)
	}
}
