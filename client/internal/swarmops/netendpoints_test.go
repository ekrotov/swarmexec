// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/fanout"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmfake"
	"swarmexec/internal/pb"
)

// tid is a task id of the right shape (25 lowercase alphanumerics).
func tid(n int) string { return fmt.Sprintf("%025d", n) }

func tname(svc string, slot, n int) string { return fmt.Sprintf("%s.%d.%s", svc, slot, tid(n)) }

func local(name, ip, mac string) *pb.NetworkLocalEndpoint {
	return &pb.NetworkLocalEndpoint{ContainerId: "c-" + name, Name: name, EndpointId: "ep-id-" + name,
		Ipv4Address: ip + "/24", MacAddress: mac}
}

func lb(netName, ip, mac string) *pb.NetworkLocalEndpoint {
	return &pb.NetworkLocalEndpoint{ContainerId: "lb-" + netName, Name: netName + "-endpoint", EndpointId: "ep-lb-" + ip,
		Ipv4Address: ip + "/24", MacAddress: mac, LoadBalancer: true}
}

func dbTask(name, ip, host string) *pb.NetworkTaskEndpoint {
	return &pb.NetworkTaskEndpoint{Name: name, EndpointId: "ep-id-" + name, EndpointIp: ip, HostIp: host}
}

func lbTask(netName, ip, host string) *pb.NetworkTaskEndpoint {
	return &pb.NetworkTaskEndpoint{Name: netName + "-endpoint", EndpointId: "ep-lb-" + ip, EndpointIp: ip, HostIp: host}
}

func nodeAns(id, name, host string, nets ...*pb.NetworkEndpointsInfo) agentcall.NodeNetworkEndpoints {
	return agentcall.NodeNetworkEndpoints{Node: resolve.Node{ID: id, Name: name, DialHost: host},
		Resp: &pb.NetworkEndpointsResponse{Networks: nets}}
}

func nodeErr(id, name string, err error) agentcall.NodeNetworkEndpoints {
	return agentcall.NodeNetworkEndpoints{Node: resolve.Node{ID: id, Name: name}, Err: err}
}

func taddr(net, node, svc string, running bool, ips ...string) TaskAddr {
	return TaskAddr{NetworkID: net, NodeID: node, Service: svc, IPs: ips, Running: running}
}

// The operator's case: web runs two tasks (10.0.1.5 on a, 10.0.1.6 on b);
// node a still has a departed task in its networkdb and an orphaned local
// endpoint; the LB endpoints and the VIP are never stale.
func TestAnalyzeEndpoints(t *testing.T) {
	webA, webB := tname("web", 1, 1), tname("web", 2, 2)
	gone, orphanIP := tname("web", 3, 3), "10.0.1.40"
	services := []*pb.NetworkServiceEndpoints{
		{Name: "", Tasks: []*pb.NetworkTaskEndpoint{lbTask("app", "10.0.1.3", "192.168.0.1"), lbTask("app", "10.0.1.4", "192.168.0.2")}},
		{Name: "web", Vip: "10.0.1.2/24", Tasks: []*pb.NetworkTaskEndpoint{
			dbTask(webA, "10.0.1.5", "192.168.0.1"), dbTask(webB, "10.0.1.6", "192.168.0.2"), dbTask(gone, "10.0.1.30", "192.168.0.2"),
		}},
	}
	netA := &pb.NetworkEndpointsInfo{Id: "app", Name: "app", Services: services,
		LocalEndpoints: []*pb.NetworkLocalEndpoint{
			local(webA, "10.0.1.5", "02:42:0a:00:01:05"), lb("app", "10.0.1.3", "02:42:0a:00:01:03"),
			{ContainerId: "ep-0123456789ab", EndpointId: "ep-orphan", Ipv4Address: orphanIP + "/24", MacAddress: "02:42:0A:00:01:28"},
		},
		Peers: []*pb.NetworkPeer{{Name: "nodeA", Ip: "192.168.0.1"}, {Name: "nodeB", Ip: "192.168.0.2"}}}
	netB := &pb.NetworkEndpointsInfo{Id: "app", Name: "app",
		Services: services[:1], // b's networkdb already forgot web's entries? no — keeps only the LBs
		LocalEndpoints: []*pb.NetworkLocalEndpoint{
			local(webB, "10.0.1.6", "02:42:0a:00:01:06"), lb("app", "10.0.1.4", "02:42:0a:00:01:04"),
		}}
	tasks := []TaskAddr{taddr("app", "nodeA-id", "web", true, "10.0.1.5"), taddr("app", "nodeB-id", "web", true, "10.0.1.6")}
	nodes := []agentcall.NodeNetworkEndpoints{
		nodeAns("nodeA-id", "a", "192.168.0.1", netA),
		nodeAns("nodeB-id", "b", "10.9.9.9", netB),
	}
	got := AnalyzeEndpoints(tasks, nodes)["app"]
	if got == nil {
		t.Fatal("no analysis for app")
	}
	if got.Running != 2 || got.Answered != 2 || got.Unknown != 0 {
		t.Errorf("running=%d answered=%d unknown=%d", got.Running, got.Answered, got.Unknown)
	}
	// Task IPs: .5 .6 .30 .40 — LB .3 .4 apart.
	if got.Addrs != 4 || got.LB != 2 {
		t.Errorf("addrs=%d lb=%d, want 4 and 2", got.Addrs, got.LB)
	}
	a, b := got.Nodes[0], got.Nodes[1]
	if a.State != NodeEndpointsOK || a.Expected != 1 || a.Containers != 2 || a.LB != 1 || a.MACs != 3 || a.Known != 5 || a.Stale != 2 {
		t.Errorf("node a = %+v", a)
	}
	if b.Expected != 1 || b.Containers != 1 || b.LB != 1 || b.MACs != 2 || b.Known != 2 || b.Stale != 0 {
		t.Errorf("node b = %+v", b)
	}
	if len(got.Stale) != 2 {
		t.Fatalf("stale = %+v", got.Stale)
	}
	s30, s40 := got.Stale[0], got.Stale[1]
	if s30.IP != "10.0.1.30" || s30.Name != gone || s30.Service != "web" || s30.Owner != "b" || s30.OwnerIP != "192.168.0.2" ||
		s30.Local || !reflect.DeepEqual(s30.SeenBy, []string{"a"}) {
		t.Errorf("stale .30 = %+v (owner via peer name → node id prefix)", s30)
	}
	if s40.IP != orphanIP || !s40.Local || !s40.Orphan || s40.MAC != "02:42:0a:00:01:28" || s40.Owner != "a" {
		t.Errorf("stale .40 = %+v", s40)
	}
	if !got.Warn() {
		t.Error("stale entries must warn")
	}
	if len(got.DupIPs) != 0 || len(got.DupMACs) != 0 {
		t.Errorf("dups = %+v / %+v", got.DupIPs, got.DupMACs)
	}
}

func TestAnalyzeEndpointsCases(t *testing.T) {
	one := func(locals []*pb.NetworkLocalEndpoint, svcs ...*pb.NetworkServiceEndpoints) []agentcall.NodeNetworkEndpoints {
		return []agentcall.NodeNetworkEndpoints{nodeAns("n1", "a", "h1",
			&pb.NetworkEndpointsInfo{Id: "net", LocalEndpoints: locals, Services: svcs})}
	}
	webT := tname("web", 1, 1)
	cases := []struct {
		name      string
		tasks     []TaskAddr
		nodes     []agentcall.NodeNetworkEndpoints
		stale     []string // IPs
		dupIPs    []string
		dupMACs   []string
		addrs, lb int
		states    []NodeEndpointState
		unknown   int
		tooOld    int
	}{
		{
			name:   "all alive",
			tasks:  []TaskAddr{taddr("net", "n1", "web", true, "10.0.0.5")},
			nodes:  one([]*pb.NetworkLocalEndpoint{local(webT, "10.0.0.5", "m5")}, &pb.NetworkServiceEndpoints{Name: "web", Tasks: []*pb.NetworkTaskEndpoint{dbTask(webT, "10.0.0.5", "h1")}}),
			addrs:  1,
			states: []NodeEndpointState{NodeEndpointsOK},
		},
		{
			name:   "starting task's address is live, not stale",
			tasks:  []TaskAddr{taddr("net", "n1", "web", false, "10.0.0.5")},
			nodes:  one([]*pb.NetworkLocalEndpoint{local(webT, "10.0.0.5", "m5")}),
			addrs:  1,
			states: []NodeEndpointState{NodeEndpointsOK},
		},
		{
			name:   "standalone container on an attachable network is not stale",
			nodes:  one([]*pb.NetworkLocalEndpoint{local("debug-shell", "10.0.0.9", "m9")}),
			addrs:  1,
			states: []NodeEndpointState{NodeEndpointsOK},
		},
		{
			name:   "task-named local endpoint without a task is stale",
			nodes:  one([]*pb.NetworkLocalEndpoint{local(webT, "10.0.0.9", "m9")}),
			stale:  []string{"10.0.0.9"},
			addrs:  1,
			states: []NodeEndpointState{NodeEndpointsOK},
		},
		{
			name: "empty-service entries (LB, standalone) are never stale",
			nodes: one(nil, &pb.NetworkServiceEndpoints{Name: "", Tasks: []*pb.NetworkTaskEndpoint{
				lbTask("net", "10.0.0.2", "h1"), dbTask("debug-shell", "10.0.0.7", "h1")}}),
			lb:     1,
			states: []NodeEndpointState{NodeEndpointsOK},
		},
		{
			name: "VIP-addressed entry is not stale",
			nodes: one(nil, &pb.NetworkServiceEndpoints{Name: "web", Vip: "10.0.0.2/24", Tasks: []*pb.NetworkTaskEndpoint{
				dbTask(webT, "10.0.0.2", "h1")}}),
			addrs:  1,
			states: []NodeEndpointState{NodeEndpointsOK},
		},
		{
			name:  "same IP on two endpoints, same MAC on two",
			tasks: []TaskAddr{taddr("net", "n1", "web", true, "10.0.0.5")},
			nodes: []agentcall.NodeNetworkEndpoints{
				nodeAns("n1", "a", "h1", &pb.NetworkEndpointsInfo{Id: "net", LocalEndpoints: []*pb.NetworkLocalEndpoint{local(webT, "10.0.0.5", "02:42:0a:00:00:05")}}),
				nodeAns("n2", "b", "h2", &pb.NetworkEndpointsInfo{Id: "net", LocalEndpoints: []*pb.NetworkLocalEndpoint{local(tname("old", 1, 9), "10.0.0.5", "02:42:0A:00:00:05")}}),
			},
			dupIPs:  []string{"10.0.0.5"},
			dupMACs: []string{"02:42:0a:00:00:05"},
			addrs:   1,
			states:  []NodeEndpointState{NodeEndpointsOK, NodeEndpointsOK},
		},
		{
			name:  "one endpoint seen by every node is one stale entry, not a duplicate",
			tasks: nil,
			nodes: []agentcall.NodeNetworkEndpoints{
				nodeAns("n1", "a", "h1", &pb.NetworkEndpointsInfo{Id: "net", Services: []*pb.NetworkServiceEndpoints{{Name: "web", Tasks: []*pb.NetworkTaskEndpoint{dbTask(webT, "10.0.0.8", "h2")}}}}),
				nodeAns("n2", "b", "h2", &pb.NetworkEndpointsInfo{Id: "net", Services: []*pb.NetworkServiceEndpoints{{Name: "web", Tasks: []*pb.NetworkTaskEndpoint{dbTask(webT, "10.0.0.8", "h2")}}}}),
			},
			stale:  []string{"10.0.0.8"},
			addrs:  1,
			states: []NodeEndpointState{NodeEndpointsOK, NodeEndpointsOK},
		},
		{
			name:  "node states: not joined, network error, too old, denied, unreachable, not reached",
			tasks: []TaskAddr{taddr("net", "n1", "web", true, "10.0.0.5")},
			nodes: []agentcall.NodeNetworkEndpoints{
				nodeAns("n1", "a", "h1", &pb.NetworkEndpointsInfo{Id: "other"}),
				nodeAns("n2", "b", "h2", &pb.NetworkEndpointsInfo{Id: "net", Error: "inspect: boom"}),
				nodeErr("n3", "c", status.Error(codes.Unimplemented, "unknown method")),
				nodeErr("n4", "d", status.Error(codes.PermissionDenied, "denied")),
				nodeErr("n5", "e", errors.New("dial tcp: refused")),
				nodeErr("n6", "f", fanout.ErrSkipped),
			},
			states:  []NodeEndpointState{NodeNotOnNetwork, NodeNetworkError, NodeAgentTooOld, NodeNotPermitted, NodeUnreachable, NodeNotReached},
			unknown: 5,
			tooOld:  1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := AnalyzeEndpoints(c.tasks, c.nodes)["net"]
			if e == nil {
				t.Fatal("no analysis")
			}
			var stale, dupIP, dupMAC []string
			for _, s := range e.Stale {
				stale = append(stale, s.IP)
			}
			for _, d := range e.DupIPs {
				dupIP = append(dupIP, d.Addr)
				if len(d.Endpoints) != 2 {
					t.Errorf("dup %s endpoints %v", d.Addr, d.Endpoints)
				}
			}
			for _, d := range e.DupMACs {
				dupMAC = append(dupMAC, d.Addr)
			}
			if !reflect.DeepEqual(stale, c.stale) || !reflect.DeepEqual(dupIP, c.dupIPs) || !reflect.DeepEqual(dupMAC, c.dupMACs) {
				t.Errorf("stale=%v dupIP=%v dupMAC=%v, want %v %v %v", stale, dupIP, dupMAC, c.stale, c.dupIPs, c.dupMACs)
			}
			if e.Addrs != c.addrs || e.LB != c.lb {
				t.Errorf("addrs=%d lb=%d, want %d %d", e.Addrs, e.LB, c.addrs, c.lb)
			}
			var states []NodeEndpointState
			for _, n := range e.Nodes {
				states = append(states, n.State)
			}
			if !reflect.DeepEqual(states, c.states) {
				t.Errorf("states = %v, want %v", states, c.states)
			}
			if e.Unknown != c.unknown || e.TooOld != c.tooOld {
				t.Errorf("unknown=%d tooOld=%d, want %d %d", e.Unknown, e.TooOld, c.unknown, c.tooOld)
			}
			if e.Warn() != (len(c.stale)+len(c.dupIPs)+len(c.dupMACs) > 0) {
				t.Errorf("Warn() = %v", e.Warn())
			}
		})
	}
}

func TestNodeEndpointStateStrings(t *testing.T) {
	for s := NodeEndpointsOK; s <= NodeNotReached; s++ {
		if s.String() == "unknown" || s.String() == "" {
			t.Errorf("state %d has no text", s)
		}
	}
	if NodeEndpointState(99).String() != "unknown" {
		t.Error("an out-of-range state must say unknown")
	}
	if !strings.Contains(NodeAgentTooOld.String(), "update the agent image") {
		t.Errorf("too-old text = %q", NodeAgentTooOld)
	}
	if !NodeNotOnNetwork.Known() || NodeAgentTooOld.Known() {
		t.Error("Known(): answered states only")
	}
	var nilE *NetEndpoints
	if nilE.Warn() {
		t.Error("nil analysis must not warn")
	}
}

func TestTaskContainerName(t *testing.T) {
	cases := map[string]bool{
		tname("web", 1, 1):                      true,
		"/" + tname("shop_api", 2, 7):           true,
		"web.nodeid0123456789abcdefg." + tid(3): true,
		"web.1":                                 false,
		"debug-shell":                           false,
		"web." + tid(1):                         false, // no slot part
		"web.1.ABCDEFGHIJKLMNOPQRSTUVWXY":       false,
		"web.1.short":                           false,
	}
	for name, want := range cases {
		if got := taskContainerName(name); got != want {
			t.Errorf("taskContainerName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestTaskServiceName(t *testing.T) {
	for name, want := range map[string]string{
		tname("shop_web", 2, 1):        "shop_web",
		"/" + tname("a.b", 1, 1):       "a.b",
		"debug-shell":                  "",
		"shop_web.1.not-a-task-id-xyz": "",
	} {
		if got := TaskServiceName(name); got != want {
			t.Errorf("TaskServiceName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestLessIP(t *testing.T) {
	if !lessIP("10.0.1.9", "10.0.1.10", "", "") || lessIP("10.0.1.10", "10.0.1.9", "", "") {
		t.Error("numeric order")
	}
	if !lessIP("10.0.1.9", "10.0.1.9", "a", "b") || !lessIP("fe80::1", "fe80::2", "", "") {
		t.Error("ties on name / non-v4 as text")
	}
}

func TestOwnerIndex(t *testing.T) {
	o := newOwnerIndex([]agentcall.NodeNetworkEndpoints{
		{Node: resolve.Node{ID: "abcdef123", Name: "a", DialHost: "10.1.0.1"}},
		{Node: resolve.Node{ID: "zzz", Name: "b"}},
	})
	cases := []struct{ ip, peer, want string }{
		{"10.1.0.1", "", "a"},
		{"10.1.0.2", "abcdef", "a"},
		{"10.1.0.3", "b", "b"},
		{"10.1.0.4", "nobody", ""},
		{"", "abcdef", ""},
	}
	for _, c := range cases {
		if got := o.name(c.ip, c.peer); got != c.want {
			t.Errorf("name(%q,%q) = %q, want %q", c.ip, c.peer, got, c.want)
		}
	}
}

func TestTaskAddrs(t *testing.T) {
	mk := func(state swarm.TaskState, node string, addrs ...string) swarm.Task {
		var tk swarm.Task
		tk.ServiceID, tk.NodeID, tk.Status.State = "svc1", node, state
		att := swarm.NetworkAttachment{Network: swarm.Network{ID: "net"}}
		for _, a := range addrs {
			att.Addresses = append(att.Addresses, netip.MustParsePrefix(a))
		}
		tk.NetworksAttachments = []swarm.NetworkAttachment{att, {}}
		return tk
	}
	got := taskAddrs([]swarm.Task{
		mk(swarm.TaskStateRunning, "n1", "10.0.0.5/24"),
		mk(swarm.TaskStateStarting, "n2", "10.0.0.6/24", "fd00::6/64"),
		mk(swarm.TaskStateShutdown, "n1", "10.0.0.7/24"),
	}, map[string]string{"svc1": "web"})
	want := []TaskAddr{
		{NetworkID: "net", NodeID: "n1", Service: "web", IPs: []string{"10.0.0.5"}, Running: true},
		{NetworkID: "net", NodeID: "n2", Service: "web", IPs: []string{"10.0.0.6", "fd00::6"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("taskAddrs = %+v\nwant %+v", got, want)
	}
}

func TestNetworkTaskAddrsOverTheAPI(t *testing.T) {
	var tk swarm.Task
	tk.ServiceID, tk.NodeID, tk.DesiredState, tk.Status.State = "s1", "n1", swarm.TaskStateRunning, swarm.TaskStateRunning
	tk.NetworksAttachments = []swarm.NetworkAttachment{{Network: swarm.Network{ID: "net"}, Addresses: []netip.Prefix{netip.MustParsePrefix("10.0.0.5/24")}}}
	var svc swarm.Service
	svc.ID, svc.Spec.Name = "s1", "web"
	got, err := NetworkTaskAddrs(context.Background(), fakeDocker(t, &swarmfake.Swarm{Tasks: []swarm.Task{tk}, Services: []swarm.Service{svc}}))
	if err != nil || len(got) != 1 || got[0].Service != "web" || got[0].IPs[0] != "10.0.0.5" {
		t.Errorf("NetworkTaskAddrs = %+v, %v", got, err)
	}
}

// A task the manager has placed but whose container does not exist yet has no
// ContainerStatus; the members views must not dereference it.
func TestMembersToleratesTaskWithoutContainer(t *testing.T) {
	var tk swarm.Task
	tk.ServiceID, tk.DesiredState, tk.Status.State = "s1", swarm.TaskStateRunning, swarm.TaskStateRunning
	tk.NetworksAttachments = []swarm.NetworkAttachment{{Network: swarm.Network{ID: "net"}}}
	var svc swarm.Service
	svc.ID, svc.Spec.Name = "s1", "web"
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Secrets: []*swarm.SecretReference{{SecretID: "sec", SecretName: "pw"}}}
	dcli := fakeDocker(t, &swarmfake.Swarm{Tasks: []swarm.Task{tk}, Services: []swarm.Service{svc}})
	ms := NetworkMembers(context.Background(), dcli, Network{ID: "net", Name: "netname", Services: []string{"web"}})
	if len(ms) != 1 || len(ms[0].Containers) != 1 || ms[0].Containers[0].ID != "" {
		t.Errorf("NetworkMembers = %+v", ms)
	}
	ss := SecretMembers(context.Background(), dcli, Secret{ID: "sec", Name: "pw"})
	if len(ss) != 1 || len(ss[0].Containers) != 1 {
		t.Errorf("SecretMembers = %+v", ss)
	}
}

// scopeFixture is network app with the visible web (two tasks, a departed
// one still in a's networkdb), a leftover of the removed service old, and —
// with db — the hidden service db: a running task, a starting one, a stale
// entry named after it, a standalone-filed entry under its task name, an
// orphaned endpoint holding its starting task's address and a task-named
// endpoint on b that duplicates its running task's address.
func scopeFixture(db bool) ([]TaskAddr, []agentcall.NodeNetworkEndpoints) {
	webA, webB, gone, old := tname("web", 1, 1), tname("web", 2, 2), tname("web", 3, 3), tname("old", 1, 5)
	tasks := []TaskAddr{taddr("app", "A", "web", true, "10.0.1.5"), taddr("app", "B", "web", true, "10.0.1.6")}
	empty := &pb.NetworkServiceEndpoints{Name: "", Tasks: []*pb.NetworkTaskEndpoint{lbTask("app", "10.0.1.3", "h1")}}
	svcs := []*pb.NetworkServiceEndpoints{empty,
		{Name: "web", Vip: "10.0.1.2/24", Tasks: []*pb.NetworkTaskEndpoint{
			dbTask(webA, "10.0.1.5", "h1"), dbTask(webB, "10.0.1.6", "h2"), dbTask(gone, "10.0.1.30", "h2")}},
		{Name: "old", Tasks: []*pb.NetworkTaskEndpoint{dbTask(old, "10.0.1.33", "h2")}},
	}
	localsA := []*pb.NetworkLocalEndpoint{local(webA, "10.0.1.5", "m5"), lb("app", "10.0.1.3", "m3")}
	localsB := []*pb.NetworkLocalEndpoint{local(webB, "10.0.1.6", "m6")}
	if db {
		dbA := tname("db", 1, 7)
		tasks = append(tasks, taddr("app", "A", "db", true, "10.0.1.7"), taddr("app", "B", "db", false, "10.0.1.8"))
		empty.Tasks = append(empty.Tasks, dbTask(tname("db", 4, 4), "10.0.1.32", "h1"))
		svcs = append(svcs, &pb.NetworkServiceEndpoints{Name: "db", Vip: "10.0.1.9/24", Tasks: []*pb.NetworkTaskEndpoint{
			dbTask(dbA, "10.0.1.7", "h1"), dbTask(tname("db", 3, 6), "10.0.1.31", "h2")}})
		localsA = append(localsA, local(dbA, "10.0.1.7", "m7"),
			&pb.NetworkLocalEndpoint{ContainerId: "ep-0123456789ab", EndpointId: "ep-orphan", Ipv4Address: "10.0.1.8/24", MacAddress: "m8"})
		localsB = append(localsB, local(tname("web", 9, 9), "10.0.1.7", "m9"))
	}
	nodes := []agentcall.NodeNetworkEndpoints{
		nodeAns("A", "a", "h1", &pb.NetworkEndpointsInfo{Id: "app", Services: svcs, LocalEndpoints: localsA}),
		nodeAns("B", "b", "h2", &pb.NetworkEndpointsInfo{Id: "app", Services: svcs[:1], LocalEndpoints: localsB}),
		nodeErr("C", "c", errors.New("dial tcp: refused")),
	}
	return tasks, nodes
}

// A hidden service on a visible network contributes nothing: the scoped
// analysis is the one of a cluster where it never ran. With nothing hidden,
// scoping changes nothing.
func TestScopeEndpoints(t *testing.T) {
	restricted := EndpointScope{
		Visible: func(s string) bool { return s == "web" },
		Hidden:  func(s string) bool { return s == "db" },
	}
	tasks, nodes := scopeFixture(true)
	full := AnalyzeEndpoints(tasks, nodes)["app"]
	if full.Running != 3 || len(full.DupIPs) == 0 {
		t.Fatalf("fixture does not exercise db: running=%d dups=%v", full.Running, full.DupIPs)
	}

	st, sn := ScopeEndpoints(tasks, nodes, restricted)
	got := AnalyzeEndpoints(st, sn)["app"]
	wt, wn := scopeFixture(false)
	want := AnalyzeEndpoints(wt, wn)["app"]
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scoped analysis\n%+v\nwant (db never ran)\n%+v", got, want)
	}
	if fmt.Sprintf("%+v", got) == fmt.Sprintf("%+v", full) {
		t.Error("scoping changed nothing")
	}
	var stale []string
	for _, s := range got.Stale {
		stale = append(stale, s.Name)
	}
	if !reflect.DeepEqual(stale, []string{tname("web", 3, 3), tname("old", 1, 5)}) {
		t.Errorf("stale = %v: the visible and the removed service's leftovers stay", stale)
	}
	if n := len(nodes[0].Resp.GetNetworks()[0].GetLocalEndpoints()); n != 4 {
		t.Errorf("the input was changed: %d local endpoints on a, want 4", n)
	}

	all := EndpointScope{Visible: func(string) bool { return true }, Hidden: func(string) bool { return false }}
	at, an := ScopeEndpoints(tasks, nodes, all)
	if !reflect.DeepEqual(AnalyzeEndpoints(at, an), AnalyzeEndpoints(tasks, nodes)) {
		t.Error("with nothing hidden the analysis must not change")
	}
}
