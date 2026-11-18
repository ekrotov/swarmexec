// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"swarmexec/client/internal/swarmfake"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func node(id, host, addr, mgrAddr string) swarm.Node {
	var n swarm.Node
	n.ID, n.Description.Hostname, n.Status.Addr = id, host, addr
	if mgrAddr != "" {
		n.ManagerStatus = &swarm.ManagerStatus{Addr: mgrAddr}
	}
	return n
}

// The operator's cluster: Hetzner network 10.0.0.0/16, overlays in 10.0.x.0/24,
// dbnet holding the hosts themselves.
func hetznerNodes() []NodeAddr {
	return NodeAddrs([]swarm.Node{
		node("n2", "docker2v2", "10.0.1.2", "10.0.1.2:2377"),
		node("n3", "docker3", "10.0.1.3", ""),
		node("n5", "docker5", "10.0.1.5", ""),
		node("n9", "", "0.0.0.0", "127.0.0.1:2377"), // unusable addresses
	})
}

func TestNodeAddrs(t *testing.T) {
	got := NodeAddrs([]swarm.Node{
		node("n1", "mgr", "10.0.1.2", "192.168.5.2:2377"),
		node("n2", "", "10.0.1.3", ""),
		node("n3", "lo", "0.0.0.0", "[::1]:2377"),
		node("n4", "dup", "10.0.1.9", "10.0.1.9:2377"),
		node("n5", "bad", "nonsense", ""),
	})
	var s []string
	for _, a := range got {
		s = append(s, a.Addr.String()+"="+a.Node)
	}
	want := "10.0.1.2=mgr,10.0.1.3=n2,10.0.1.9=dup,192.168.5.2=mgr"
	if strings.Join(s, ",") != want {
		t.Errorf("NodeAddrs = %v, want %s", s, want)
	}
}

func TestAnalyzeOverlaps(t *testing.T) {
	nets := []Network{
		{ID: "db", Name: "dbnet", Subnets: []netip.Prefix{pfx("10.0.1.0/24")}},
		{ID: "web", Name: "webnet", Subnets: []netip.Prefix{pfx("10.0.2.0/24")}},
		{ID: "ok", Name: "clean", Subnets: []netip.Prefix{pfx("10.100.1.0/24")}},
		{ID: "dup", Name: "twin", Subnets: []netip.Prefix{pfx("10.100.1.128/25")}},
		{ID: "host", Name: "host"}, // no IPAM subnet: skipped
	}
	AnalyzeOverlaps(nets, hetznerNodes(), []netip.Prefix{pfx("10.0.0.0/16")})
	by := map[string]Network{}
	for _, n := range nets {
		by[n.Name] = n
	}

	db := by["dbnet"]
	if db.OverlapSeverity() != OverlapHigh {
		t.Fatalf("dbnet severity = %v, want high (%+v)", db.OverlapSeverity(), db.Overlaps)
	}
	if got := db.Overlaps[0].Message; got != "10.0.1.0/24 contains host IPs: 10.0.1.2 (docker2v2), 10.0.1.3 (docker3), 10.0.1.5 (docker5)" {
		t.Errorf("dbnet host message = %q", got)
	}
	if db.Overlaps[1].Kind != OverlapUnderlay || !strings.Contains(db.Overlaps[1].Message, "underlay 10.0.0.0/16") {
		t.Errorf("dbnet second finding = %+v, want the underlay overlap", db.Overlaps[1])
	}

	web := by["webnet"]
	if web.OverlapSeverity() != OverlapWarn || len(web.Overlaps) != 1 || web.Overlaps[0].Kind != OverlapUnderlay {
		t.Errorf("webnet = %+v, want one underlay warning", web.Overlaps)
	}
	if c := by["clean"]; c.OverlapSeverity() != OverlapWarn || c.Overlaps[0].Kind != OverlapNetwork ||
		c.Overlaps[0].Message != "10.100.1.0/24 overlaps network twin (10.100.1.128/25)" {
		t.Errorf("clean = %+v, want a network overlap with twin", c.Overlaps)
	}
	if h := by["host"]; len(h.Overlaps) != 0 {
		t.Errorf("host (no subnet) has findings: %+v", h.Overlaps)
	}

	// Without an underlay only node IPs count.
	AnalyzeOverlaps(nets, hetznerNodes(), nil)
	if w := nets[1]; len(w.Overlaps) != 0 {
		t.Errorf("webnet without underlay = %+v, want clean", w.Overlaps)
	}
}

func TestCheckDefaultPool(t *testing.T) {
	nodes := hetznerNodes()
	fs := CheckDefaultPool(nil, 0, nodes, nil)
	if len(fs) != 1 || fs[0].Kind != OverlapDefaultPool || fs[0].Severity != OverlapWarn {
		t.Fatalf("default pool findings = %+v", fs)
	}
	for _, want := range []string{"/24", "10.0.0.0/8", "10.0.1.2 (docker2v2)", "explicit subnet"} {
		if !strings.Contains(fs[0].Message, want) {
			t.Errorf("message %q lacks %q", fs[0].Message, want)
		}
	}
	// A pool clear of the hosts and the underlay is fine.
	if fs := CheckDefaultPool([]netip.Prefix{pfx("10.200.0.0/16")}, 26, nodes, []netip.Prefix{pfx("10.0.0.0/16")}); len(fs) != 0 {
		t.Errorf("clean pool flagged: %+v", fs)
	}
	// The underlay alone inside the pool is enough.
	fs = CheckDefaultPool([]netip.Prefix{pfx("172.16.0.0/12")}, 26, nil, []netip.Prefix{pfx("172.20.0.0/16")})
	if len(fs) != 1 || !strings.Contains(fs[0].Message, "/26") || !strings.Contains(fs[0].Message, "underlay 172.20.0.0/16") {
		t.Errorf("underlay-in-pool = %+v", fs)
	}
}

func TestCheckNewNetwork(t *testing.T) {
	nodes := hetznerNodes()
	underlay := []netip.Prefix{pfx("10.0.0.0/16")}
	existing := []Network{{ID: "x", Name: "app", Subnets: []netip.Prefix{pfx("10.100.1.0/24")}}}

	if fs := CheckNewNetwork(NewNetworkOpts{Subnet: "10.0.1.0/24"}, nodes, underlay, existing, nil, 0); WorstSeverity(fs) != OverlapHigh {
		t.Errorf("subnet with hosts: %+v", fs)
	}
	if fs := CheckNewNetwork(NewNetworkOpts{Subnet: "10.100.1.0/24"}, nodes, underlay, existing, nil, 0); len(fs) != 1 || fs[0].Kind != OverlapNetwork {
		t.Errorf("subnet of an existing network: %+v", fs)
	}
	if fs := CheckNewNetwork(NewNetworkOpts{Subnet: "10.100.2.0/24"}, nodes, underlay, existing, nil, 0); len(fs) != 0 {
		t.Errorf("clean subnet flagged: %+v", fs)
	}
	if fs := CheckNewNetwork(NewNetworkOpts{Subnet: "not-a-cidr"}, nodes, underlay, existing, nil, 0); fs != nil {
		t.Errorf("bad subnet must be left to the create: %+v", fs)
	}
	if fs := CheckNewNetwork(NewNetworkOpts{Driver: "overlay"}, nodes, underlay, existing, nil, 0); len(fs) != 1 || fs[0].Kind != OverlapDefaultPool {
		t.Errorf("no subnet, default pool: %+v", fs)
	}
	if fs := CheckNewNetwork(NewNetworkOpts{Driver: "bridge"}, nodes, underlay, existing, nil, 0); fs != nil {
		t.Errorf("a local bridge does not draw from the swarm pool: %+v", fs)
	}
}

func TestSeverityString(t *testing.T) {
	for s, want := range map[OverlapSeverity]string{OverlapNone: "none", OverlapWarn: "warn", OverlapHigh: "high"} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", s, s.String(), want)
		}
	}
}

func ipamNet(id, name, subnet, gw string) network.Summary {
	var n network.Summary
	n.ID, n.Name, n.Driver, n.Scope = id, name, "overlay", "swarm"
	c := network.IPAMConfig{Subnet: pfx(subnet)}
	if gw != "" {
		c.Gateway = netip.MustParseAddr(gw)
	}
	n.IPAM.Config = []network.IPAMConfig{c}
	return n
}

func fakeDocker(t *testing.T, f *swarmfake.Swarm) *client.Client {
	t.Helper()
	api := swarmfake.Serve(t, f)
	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dcli.Close() })
	return dcli
}

func TestListNetworksSubnetsContainersAndOverlaps(t *testing.T) {
	running := func(id string, nets ...string) swarm.Task {
		var tk swarm.Task
		tk.ID = id
		tk.DesiredState, tk.Status.State = swarm.TaskStateRunning, swarm.TaskStateRunning
		for _, n := range nets {
			tk.NetworksAttachments = append(tk.NetworksAttachments, swarm.NetworkAttachment{Network: swarm.Network{ID: n}})
		}
		return tk
	}
	stopped := running("t-old", "db")
	stopped.DesiredState, stopped.Status.State = swarm.TaskStateShutdown, swarm.TaskStateShutdown
	f := &swarmfake.Swarm{
		Networks: []network.Summary{ipamNet("db", "dbnet", "10.0.1.0/24", "10.0.1.1"), ipamNet("app", "appnet", "10.100.1.0/24", "")},
		Nodes:    []swarm.Node{node("n2", "docker2v2", "10.0.1.2", "")},
		Tasks:    []swarm.Task{running("t1", "db", "app"), running("t2", "db", "db"), stopped},
	}
	nets, err := ListNetworks(context.Background(), fakeDocker(t, f), []netip.Prefix{pfx("10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 2 || nets[0].Name != "appnet" || nets[1].Name != "dbnet" {
		t.Fatalf("networks = %+v", nets)
	}
	app, db := nets[0], nets[1]
	if app.Containers != 1 || db.Containers != 2 {
		t.Errorf("containers app=%d db=%d, want 1 and 2 (running only, one per task)", app.Containers, db.Containers)
	}
	if len(db.Gateways) != 1 || db.Gateways[0].String() != "10.0.1.1" || db.Subnets[0].String() != "10.0.1.0/24" {
		t.Errorf("db IPAM = %v / %v", db.Subnets, db.Gateways)
	}
	if db.OverlapSeverity() != OverlapHigh || app.OverlapSeverity() != OverlapNone {
		t.Errorf("severities db=%v app=%v", db.OverlapSeverity(), app.OverlapSeverity())
	}
}

func TestPreflightNetworkCreate(t *testing.T) {
	f := &swarmfake.Swarm{
		Networks: []network.Summary{ipamNet("app", "appnet", "10.100.1.0/24", "")},
		Nodes:    []swarm.Node{node("n2", "docker2v2", "10.0.1.2", "")},
	}
	dcli := fakeDocker(t, f)
	ctx := context.Background()
	if fs := PreflightNetworkCreate(ctx, dcli, NewNetworkOpts{Name: "x", Subnet: "10.100.1.0/25"}, nil); len(fs) != 1 || fs[0].Kind != OverlapNetwork {
		t.Errorf("overlap with appnet: %+v", fs)
	}
	// No subnet: the swarm's pool (unset = 10.0.0.0/8) holds the node.
	if fs := PreflightNetworkCreate(ctx, dcli, NewNetworkOpts{Name: "x"}, nil); len(fs) != 1 || !strings.Contains(fs[0].Message, "10.0.0.0/8") {
		t.Errorf("default pool: %+v", fs)
	}
	// A pool set at swarm init that avoids the hosts is clean.
	f.SwarmInfo = &swarm.Swarm{ClusterInfo: swarm.ClusterInfo{DefaultAddrPool: []netip.Prefix{pfx("10.200.0.0/16")}, SubnetSize: 24}}
	if fs := PreflightNetworkCreate(ctx, dcli, NewNetworkOpts{Name: "x"}, nil); len(fs) != 0 {
		t.Errorf("clean pool flagged: %+v", fs)
	}
}
