// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"google.golang.org/grpc/codes"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

func mustMAC(t *testing.T, s string) network.HardwareAddr {
	t.Helper()
	hw, err := net.ParseMAC(s)
	if err != nil {
		t.Fatal(err)
	}
	return network.HardwareAddr(hw)
}

// appNet is a joined overlay as a worker's daemon reports it in verbose mode:
// one task here, its load balancer, and the networkdb view of two nodes.
func appNet(t *testing.T) network.Inspect {
	return network.Inspect{
		Network: network.Network{
			ID: "net-app", Name: "app", Scope: "swarm", Driver: "overlay",
			Peers: []network.PeerInfo{
				{Name: "node-b", IP: netip.MustParseAddr("10.0.0.12")},
				{Name: "node-a", IP: netip.MustParseAddr("10.0.0.11")},
			},
		},
		Containers: map[string]network.EndpointResource{
			"c0ffee": {
				Name: "web.1.abc", EndpointID: "ep-web1",
				IPv4Address: netip.MustParsePrefix("10.0.1.5/24"),
				MacAddress:  mustMAC(t, "02:42:0a:00:01:05"),
			},
			"lb-app": {
				Name: "app-endpoint", EndpointID: "ep-lb",
				IPv4Address: netip.MustParsePrefix("10.0.1.3/24"),
				MacAddress:  mustMAC(t, "02:42:0a:00:01:03"),
			},
		},
		Services: map[string]network.ServiceInfo{
			"web": {VIP: netip.MustParseAddr("10.0.1.2"), Tasks: []network.Task{
				{Name: "web.2.def", EndpointID: "ep-web2", EndpointIP: netip.MustParseAddr("10.0.1.6"), Info: map[string]string{"Host IP": "10.0.0.12"}},
				{Name: "web.1.abc", EndpointID: "ep-web1", EndpointIP: netip.MustParseAddr("10.0.1.5"), Info: map[string]string{"Host IP": "10.0.0.11"}},
			}},
			"": {Tasks: []network.Task{
				{Name: "app-endpoint", EndpointID: "ep-lb", EndpointIP: netip.MustParseAddr("10.0.1.3")},
			}},
		},
	}
}

func networkTestServer(d *fakeDocker) *Server {
	s, _ := newTestServer(d, auth.AllowAll{}, Options{})
	return s
}

// The default answer is every swarm network this node has joined, shaped and
// sorted for diffing: local endpoints with IP and MAC, the load balancer
// flagged, the networkdb tasks with their owning host, and the peers.
func TestNetworkEndpointsReportsJoinedSwarmNetworks(t *testing.T) {
	d := newFakeDocker()
	d.networks = []network.Summary{
		{Network: network.Network{ID: "net-app", Name: "app", Scope: "swarm", Driver: "overlay"}},
		// A manager lists swarm networks it never instantiated: inspected, they
		// carry no peers, endpoints or services, and are left out.
		{Network: network.Network{ID: "net-elsewhere", Name: "elsewhere", Scope: "swarm", Driver: "overlay"}},
		// The scope filter is not applied by the fake; the handler must hold.
		{Network: network.Network{ID: "net-bridge", Name: "bridge", Scope: "local", Driver: "bridge"}},
		// Removed between the listing and the inspect.
		{Network: network.Network{ID: "net-gone", Name: "gone", Scope: "swarm", Driver: "overlay"}},
	}
	d.netInspect = map[string]network.Inspect{"net-app": appNet(t)}
	d.netInspectErr = map[string]error{"net-gone": cerrdefs.ErrNotFound}

	resp, err := networkTestServer(d).NetworkEndpoints(context.Background(), &pb.NetworkEndpointsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetNetworks()) != 1 {
		t.Fatalf("networks = %v, want only the joined swarm network", resp.GetNetworks())
	}
	n := resp.GetNetworks()[0]
	if n.GetId() != "net-app" || n.GetName() != "app" || n.GetDriver() != "overlay" || n.GetIngress() || n.GetError() != "" {
		t.Errorf("network = %+v", n)
	}

	eps := n.GetLocalEndpoints()
	if len(eps) != 2 {
		t.Fatalf("local endpoints = %v", eps)
	}
	lb, web := eps[0], eps[1] // sorted by name: "app-endpoint" < "web.1.abc"
	if lb.GetContainerId() != "lb-app" || !lb.GetLoadBalancer() || lb.GetIpv4Address() != "10.0.1.3/24" || lb.GetMacAddress() != "02:42:0a:00:01:03" {
		t.Errorf("lb endpoint = %+v", lb)
	}
	if web.GetContainerId() != "c0ffee" || web.GetLoadBalancer() || web.GetEndpointId() != "ep-web1" ||
		web.GetIpv4Address() != "10.0.1.5/24" || web.GetIpv6Address() != "" || web.GetMacAddress() != "02:42:0a:00:01:05" {
		t.Errorf("task endpoint = %+v", web)
	}

	svcs := n.GetServices()
	if len(svcs) != 2 || svcs[0].GetName() != "" || svcs[1].GetName() != "web" {
		t.Fatalf("services = %v, want the load balancers' \"\" entry and web, sorted", svcs)
	}
	if svcs[0].GetVip() != "" || len(svcs[0].GetTasks()) != 1 || svcs[0].GetTasks()[0].GetHostIp() != "" {
		t.Errorf("lb service = %+v", svcs[0])
	}
	if svcs[1].GetVip() != "10.0.1.2" {
		t.Errorf("vip = %q", svcs[1].GetVip())
	}
	tasks := svcs[1].GetTasks()
	if len(tasks) != 2 || tasks[0].GetName() != "web.1.abc" || tasks[0].GetEndpointIp() != "10.0.1.5" ||
		tasks[0].GetHostIp() != "10.0.0.11" || tasks[1].GetHostIp() != "10.0.0.12" || tasks[1].GetEndpointId() != "ep-web2" {
		t.Errorf("tasks = %v", tasks)
	}

	peers := n.GetPeers()
	if len(peers) != 2 || peers[0].GetIp() != "10.0.0.11" || peers[0].GetName() != "node-a" || peers[1].GetIp() != "10.0.0.12" {
		t.Errorf("peers = %v, want sorted by IP", peers)
	}

	// The listing asked for swarm scope, every inspect was verbose and swarm-scoped.
	if len(d.networkListOpts) != 1 || len(filterValues(d.networkListOpts[0].Filters, "scope")) != 1 ||
		filterValues(d.networkListOpts[0].Filters, "scope")[0] != "swarm" {
		t.Errorf("list opts = %+v", d.networkListOpts)
	}
	for _, o := range d.netInspectOpts {
		if !o.Verbose || o.Scope != "swarm" {
			t.Errorf("inspect opts = %+v, want verbose, swarm scope", o)
		}
	}
}

// Requested networks are reported even when not joined, deduplicated; one that
// is missing, not swarm-scope or fails comes back with its error instead of
// failing the RPC.
func TestNetworkEndpointsExplicitNetworks(t *testing.T) {
	d := newFakeDocker()
	d.networks = []network.Summary{
		{Network: network.Network{ID: "net-bridge", Name: "bridge", Scope: "local", Driver: "bridge"}},
	}
	app := appNet(t)
	ingress := network.Inspect{Network: network.Network{ID: "net-ingress", Name: "ingress", Scope: "swarm", Driver: "overlay", Ingress: true}}
	ingress.Containers = map[string]network.EndpointResource{"ingress-sbox": {Name: "ingress-endpoint"}}
	d.netInspect = map[string]network.Inspect{"net-app": app, "app": app, "net-ingress": ingress}
	d.netInspectErr = map[string]error{"missing": cerrdefs.ErrNotFound, "broken": errors.New("daemon hiccup")}

	resp, err := networkTestServer(d).NetworkEndpoints(context.Background(), &pb.NetworkEndpointsRequest{
		NetworkIds: []string{"net-app", "app", " net-app ", "missing", "net-bridge", "broken", "net-ingress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*pb.NetworkEndpointsInfo{}
	for _, n := range resp.GetNetworks() {
		got[n.GetId()] = n
	}
	if len(resp.GetNetworks()) != 5 {
		t.Fatalf("networks = %v, want app once, ingress, and three errors", resp.GetNetworks())
	}
	if got["net-app"].GetError() != "" || len(got["net-app"].GetLocalEndpoints()) != 2 {
		t.Errorf("app = %+v", got["net-app"])
	}
	if g := got["net-ingress"]; !g.GetIngress() || len(g.GetLocalEndpoints()) != 1 || !g.GetLocalEndpoints()[0].GetLoadBalancer() {
		t.Errorf("ingress = %+v, want its ingress-sbox flagged as the load balancer", g)
	}
	if got["missing"].GetError() != "network not found on this node" {
		t.Errorf("missing = %+v", got["missing"])
	}
	if got["net-bridge"].GetError() != "not a swarm-scope network" {
		t.Errorf("bridge = %+v", got["net-bridge"])
	}
	if got["broken"].GetError() != "inspect network: daemon hiccup" {
		t.Errorf("broken = %+v", got["broken"])
	}
}

func TestNetworkEndpointsRejectsBadRequests(t *testing.T) {
	s := networkTestServer(newFakeDocker())
	if _, err := s.NetworkEndpoints(context.Background(), &pb.NetworkEndpointsRequest{NetworkIds: []string{"a", " "}}); statusCode(err) != codes.InvalidArgument {
		t.Errorf("empty id: err = %v, want InvalidArgument", err)
	}
	many := make([]string, maxNetworkIDs+1)
	for i := range many {
		many[i] = "n"
	}
	if _, err := s.NetworkEndpoints(context.Background(), &pb.NetworkEndpointsRequest{NetworkIds: many}); statusCode(err) != codes.InvalidArgument {
		t.Errorf("too many ids: err = %v, want InvalidArgument", err)
	}
}

// A cancelled caller ends the RPC with its context's status, not with a list
// of per-network errors.
func TestNetworkEndpointsCancelled(t *testing.T) {
	d := newFakeDocker()
	d.netInspect = map[string]network.Inspect{"net-app": appNet(t)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := networkTestServer(d).NetworkEndpoints(ctx, &pb.NetworkEndpointsRequest{NetworkIds: []string{"net-app"}})
	if statusCode(err) != codes.Canceled {
		t.Errorf("err = %v, want Canceled", err)
	}
}
