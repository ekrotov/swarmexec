// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"
)

// fakeDocker is an in-memory DockerClient honoring the filters the resolver uses
// (name, service, id, desired-state).
type fakeDocker struct {
	services []swarm.Service
	tasks    []swarm.Task
	nodes    map[string]swarm.Node
	peers    []swarm.Peer // raft peer list returned by Info
	infoErr  error
	infoCals int // how many times Info was called
}

func (f *fakeDocker) Info(_ context.Context) (system.Info, error) {
	f.infoCals++
	if f.infoErr != nil {
		return system.Info{}, f.infoErr
	}
	return system.Info{Swarm: swarm.Info{RemoteManagers: f.peers}}, nil
}

func (f *fakeDocker) ServiceList(_ context.Context, opts types.ServiceListOptions) ([]swarm.Service, error) {
	name := first(opts.Filters.Get("name"))
	var out []swarm.Service
	for _, s := range f.services {
		if name == "" || contains(s.Spec.Name, name) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeDocker) TaskList(_ context.Context, opts types.TaskListOptions) ([]swarm.Task, error) {
	id := first(opts.Filters.Get("id"))
	service := first(opts.Filters.Get("service"))
	desired := first(opts.Filters.Get("desired-state"))
	var out []swarm.Task
	for _, t := range f.tasks {
		if id != "" && t.ID != id {
			continue
		}
		if service != "" && t.ServiceID != service {
			continue
		}
		if desired != "" && string(t.DesiredState) != desired {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeDocker) NodeInspectWithRaw(_ context.Context, id string) (swarm.Node, []byte, error) {
	n, ok := f.nodes[id]
	if !ok {
		return swarm.Node{}, nil, errors.New("node not found")
	}
	return n, nil, nil
}

func (f *fakeDocker) NodeList(_ context.Context, _ types.NodeListOptions) ([]swarm.Node, error) {
	var out []swarm.Node
	for _, n := range f.nodes {
		out = append(out, n)
	}
	return out, nil
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}
func contains(hay, needle string) bool {
	return len(needle) == 0 || (len(hay) >= len(needle) && indexOf(hay, needle) >= 0)
}
func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func node(id, host, addr string) swarm.Node {
	n := swarm.Node{ID: id}
	n.Description.Hostname = host
	n.Status.Addr = addr
	n.Status.State = swarm.NodeStateReady
	return n
}

func task(id, serviceID, nodeID string, slot int, containerID string) swarm.Task {
	t := swarm.Task{ID: id, ServiceID: serviceID, NodeID: nodeID, Slot: slot}
	t.DesiredState = swarm.TaskStateRunning
	t.Status.Timestamp = time.Now().Add(-5 * time.Minute)
	if containerID != "" {
		t.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: containerID}
	}
	return t
}

func svc(id, name string) swarm.Service {
	s := swarm.Service{ID: id}
	s.Spec.Name = name
	return s
}

func newFake() *fakeDocker {
	return &fakeDocker{
		services: []swarm.Service{svc("svc-web", "web"), svc("svc-db", "db")},
		nodes: map[string]swarm.Node{
			"node-a": node("node-a", "host-a", "10.0.0.1"),
			"node-b": node("node-b", "host-b", "10.0.0.2"),
		},
	}
}

func TestCandidatesPopulatesServiceAndNodeAddr(t *testing.T) {
	f := newFake()
	f.tasks = []swarm.Task{
		task("t1", "svc-web", "node-a", 1, "cWEB"),
		task("t2", "svc-db", "node-b", 1, "cDB"),
	}
	r := New(f, AddrHostname)

	cands, err := r.Candidates(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(cands))
	}
	by := map[string]Candidate{}
	for _, c := range cands {
		by[c.ContainerID] = c
	}
	if web := by["cWEB"]; web.Service != "web" || web.NodeName != "host-a" || web.NodeAddr != "10.0.0.1" {
		t.Errorf("web candidate wrong: service=%q node=%q addr=%q", web.Service, web.NodeName, web.NodeAddr)
	}
	if db := by["cDB"]; db.Service != "db" || db.NodeAddr != "10.0.0.2" {
		t.Errorf("db candidate wrong: service=%q addr=%q", db.Service, db.NodeAddr)
	}
}

// A node reporting "0.0.0.0" that is not in the raft peer list (or whose peer
// lookup failed) still degrades to the hostname — the last resort.
func TestDialHost_IPModeFallsBackWhenAddrUnusable(t *testing.T) {
	f := newFake()
	f.nodes["node-z"] = node("node-z", "host-z", "0.0.0.0") // leader: no observed remote addr
	f.tasks = []swarm.Task{task("t1", "svc-db", "node-z", 1, "cZ")}
	r := New(f, AddrIP)

	ep, err := r.Resolve(context.Background(), Request{Target: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.DialHost != "host-z" {
		t.Errorf("dial host = %q, want host-z (fallback from 0.0.0.0)", ep.DialHost)
	}
}

// The leader reports Status.Addr "0.0.0.0" because swarm has no remote agent
// session to observe for itself. Its real address is in the raft peer list, and
// -addr-mode ip must use it rather than degrade to a hostname the operator may
// not be able to resolve.
func TestDialHost_IPModeRecoversLeaderAddrFromRaftPeers(t *testing.T) {
	f := newFake()
	f.nodes["node-z"] = node("node-z", "host-z", "0.0.0.0")
	f.peers = []swarm.Peer{
		{NodeID: "node-a", Addr: "10.0.1.2:2377"},
		{NodeID: "node-z", Addr: "10.0.1.5:2377"},
	}
	f.tasks = []swarm.Task{task("t1", "svc-db", "node-z", 1, "cZ")}
	r := New(f, AddrIP)

	ep, err := r.Resolve(context.Background(), Request{Target: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.DialHost != "10.0.1.5" {
		t.Errorf("dial host = %q, want 10.0.1.5 (recovered from raft peers)", ep.DialHost)
	}
}

// A peer entry that is itself unusable must not win over the hostname.
func TestDialHost_IPModeIgnoresUnusablePeerAddr(t *testing.T) {
	f := newFake()
	f.nodes["node-z"] = node("node-z", "host-z", "0.0.0.0")
	f.peers = []swarm.Peer{{NodeID: "node-z", Addr: "0.0.0.0:2377"}}
	f.tasks = []swarm.Task{task("t1", "svc-db", "node-z", 1, "cZ")}
	r := New(f, AddrIP)

	ep, err := r.Resolve(context.Background(), Request{Target: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.DialHost != "host-z" {
		t.Errorf("dial host = %q, want host-z", ep.DialHost)
	}
}

// A failing Info must not break resolution — it just costs the recovery.
func TestDialHost_IPModeToleratesInfoError(t *testing.T) {
	f := newFake()
	f.nodes["node-z"] = node("node-z", "host-z", "0.0.0.0")
	f.infoErr = errors.New("manager unreachable")
	f.tasks = []swarm.Task{task("t1", "svc-db", "node-z", 1, "cZ")}
	r := New(f, AddrIP)

	ep, err := r.Resolve(context.Background(), Request{Target: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.DialHost != "host-z" {
		t.Errorf("dial host = %q, want host-z", ep.DialHost)
	}
}

// The peer list costs one Info call per Resolver, however many nodes need it.
func TestDialHost_PeerListFetchedOnce(t *testing.T) {
	f := newFake()
	f.nodes["node-y"] = node("node-y", "host-y", "0.0.0.0")
	f.nodes["node-z"] = node("node-z", "host-z", "0.0.0.0")
	f.peers = []swarm.Peer{
		{NodeID: "node-y", Addr: "10.0.1.4:2377"},
		{NodeID: "node-z", Addr: "10.0.1.5:2377"},
	}
	r := New(f, AddrIP)

	nodes, err := r.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("no nodes returned")
	}
	if f.infoCals != 1 {
		t.Errorf("Info called %d times, want 1", f.infoCals)
	}
}

func TestHostOnly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"10.0.1.5:2377", "10.0.1.5"},
		{"10.0.1.5", "10.0.1.5"},
		{"[fd00::5]:2377", "fd00::5"},
		{"  10.0.1.5:2377  ", "10.0.1.5"},
		{"", ""},
	} {
		if got := hostOnly(tc.in); got != tc.want {
			t.Errorf("hostOnly(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDialHost_IPModeUsesRealAddr(t *testing.T) {
	f := newFake()
	f.tasks = []swarm.Task{task("t1", "svc-db", "node-a", 1, "cA")}
	r := New(f, AddrIP)

	ep, err := r.Resolve(context.Background(), Request{Target: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.DialHost != "10.0.0.1" {
		t.Errorf("dial host = %q, want 10.0.0.1", ep.DialHost)
	}
}

func TestResolveSingleTask(t *testing.T) {
	f := newFake()
	f.tasks = []swarm.Task{task("t1", "svc-db", "node-a", 1, "containerDB")}
	r := New(f, AddrHostname)

	ep, err := r.Resolve(context.Background(), Request{Target: "db"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.ContainerID != "containerDB" {
		t.Errorf("container = %q, want containerDB", ep.ContainerID)
	}
	if ep.DialHost != "host-a" {
		t.Errorf("dial host = %q, want host-a", ep.DialHost)
	}
}

func TestResolveMultipleIsAmbiguous(t *testing.T) {
	f := newFake()
	f.tasks = []swarm.Task{
		task("t1", "svc-web", "node-a", 1, "c1"),
		task("t2", "svc-web", "node-b", 2, "c2"),
	}
	r := New(f, AddrHostname)

	_, err := r.Resolve(context.Background(), Request{Target: "web"})
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("want AmbiguousError, got %v", err)
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d", len(amb.Candidates))
	}
}

func TestResolveNoRunningTask(t *testing.T) {
	f := newFake()
	r := New(f, AddrHostname)
	_, err := r.Resolve(context.Background(), Request{Target: "web"})
	if err == nil {
		t.Fatal("want error for no running task")
	}
	if _, ok := err.(*AmbiguousError); ok {
		t.Fatal("should not be ambiguous when there are zero tasks")
	}
}

func TestResolveServiceSlot(t *testing.T) {
	f := newFake()
	f.tasks = []swarm.Task{
		task("t1", "svc-web", "node-a", 1, "c1"),
		task("t2", "svc-web", "node-b", 2, "c2"),
	}
	r := New(f, AddrHostname)

	ep, err := r.Resolve(context.Background(), Request{Target: "web.2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.ContainerID != "c2" || ep.DialHost != "host-b" {
		t.Errorf("got container=%q host=%q, want c2/host-b", ep.ContainerID, ep.DialHost)
	}
}

func TestResolveTaskID(t *testing.T) {
	f := newFake()
	f.tasks = []swarm.Task{task("task-xyz", "svc-web", "node-b", 3, "cxyz")}
	r := New(f, AddrHostname)

	ep, err := r.Resolve(context.Background(), Request{Target: "task-xyz"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.ContainerID != "cxyz" {
		t.Errorf("container = %q, want cxyz", ep.ContainerID)
	}
}

func TestResolveAddrModeIP(t *testing.T) {
	f := newFake()
	f.tasks = []swarm.Task{task("t1", "svc-db", "node-a", 1, "cdb")}
	r := New(f, AddrIP)

	ep, err := r.Resolve(context.Background(), Request{Target: "db"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.DialHost != "10.0.0.1" {
		t.Errorf("dial host = %q, want 10.0.0.1", ep.DialHost)
	}
}

func TestResolveContainerIDNeedsNode(t *testing.T) {
	f := newFake()
	r := New(f, AddrHostname)
	_, err := r.Resolve(context.Background(), Request{Target: "deadbeefcafe"})
	if err == nil {
		t.Fatal("want error asking for --node")
	}
}

func TestResolveContainerIDWithNodeHint(t *testing.T) {
	f := newFake()
	r := New(f, AddrHostname)
	ep, err := r.Resolve(context.Background(), Request{Target: "deadbeefcafe", NodeHint: "node-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.DialHost != "host-b" || ep.ContainerID != "deadbeefcafe" {
		t.Errorf("got host=%q container=%q, want host-b/deadbeefcafe", ep.DialHost, ep.ContainerID)
	}
}

// svcStatus builds a service carrying the manager's ServiceStatus shortcut.
func svcStatus(id, name string, running, desired uint64, global bool) swarm.Service {
	s := svc(id, name)
	s.ServiceStatus = &swarm.ServiceStatus{RunningTasks: running, DesiredTasks: desired}
	if global {
		s.Spec.Mode.Global = &swarm.GlobalService{}
	}
	return s
}

func TestServicesUsesServiceStatus(t *testing.T) {
	f := newFake()
	f.services = []swarm.Service{
		svcStatus("s-web", "web", 3, 3, false),     // healthy
		svcStatus("s-api", "api", 1, 3, false),     // partial
		svcStatus("s-db", "db", 0, 3, false),       // down
		svcStatus("s-cache", "cache", 0, 0, false), // scaled to zero
		svcStatus("s-agent", "agent", 2, 2, true),  // global
	}
	r := New(f, AddrHostname)
	got, err := r.Services(context.Background())
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	// Sorted by name.
	want := []Service{
		{Name: "agent", Running: 2, Desired: 2, Global: true, Mode: "global"},
		{Name: "api", Running: 1, Desired: 3, Mode: "replicated"},
		{Name: "cache", Running: 0, Desired: 0, Mode: "replicated"},
		{Name: "db", Running: 0, Desired: 3, Mode: "replicated"},
		{Name: "web", Running: 3, Desired: 3, Mode: "replicated"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d services, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if !reflect.DeepEqual(got[i], w) {
			t.Errorf("service[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestServiceMode(t *testing.T) {
	cases := []struct {
		m    swarm.ServiceMode
		want string
	}{
		{swarm.ServiceMode{Global: &swarm.GlobalService{}}, "global"},
		{swarm.ServiceMode{GlobalJob: &swarm.GlobalJob{}}, "global-job"},
		{swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{}}, "replicated-job"},
		{swarm.ServiceMode{}, "replicated"},
	}
	for _, c := range cases {
		if got := serviceMode(c.m); got != c.want {
			t.Errorf("serviceMode(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
}

func TestServiceImage(t *testing.T) {
	if got := serviceImage(nil); got != "" {
		t.Errorf("serviceImage(nil) = %q, want empty", got)
	}
	// The @sha256 digest is stripped for display.
	if got, want := serviceImage(&swarm.ContainerSpec{Image: "nginx:1.27@sha256:deadbeef"}), "nginx:1.27"; got != want {
		t.Errorf("serviceImage = %q, want %q", got, want)
	}
	if got, want := serviceImage(&swarm.ContainerSpec{Image: "postgres:15"}), "postgres:15"; got != want {
		t.Errorf("serviceImage = %q, want %q", got, want)
	}
}

func TestServicePorts(t *testing.T) {
	ports := []swarm.PortConfig{
		{PublishedPort: 8080, TargetPort: 80, Protocol: "tcp", PublishMode: swarm.PortConfigPublishModeIngress},
		{PublishedPort: 5353, TargetPort: 53, Protocol: "udp", PublishMode: swarm.PortConfigPublishModeHost},
		{PublishedPort: 0, TargetPort: 9000}, // unpublished → skipped
	}
	if got, want := servicePorts(ports), "*:8080->80/tcp, 5353->53/udp"; got != want {
		t.Errorf("servicePorts = %q, want %q", got, want)
	}
	if got := servicePorts(nil); got != "" {
		t.Errorf("servicePorts(nil) = %q, want empty", got)
	}
}

func TestServicesFallbackWithoutStatus(t *testing.T) {
	f := newFake()
	// No ServiceStatus: a replicated service with replicas=2, one task running.
	rep := svc("s-web", "web")
	two := uint64(2)
	rep.Spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &two}
	f.services = []swarm.Service{rep}
	f.tasks = []swarm.Task{
		task("t1", "s-web", "node-a", 1, "cWEB"), // running with a container
		task("t2", "s-web", "node-b", 2, ""),     // scheduled, no container yet
	}
	// task() sets state Running via helper? ensure Status.State is running.
	for i := range f.tasks {
		f.tasks[i].Status.State = swarm.TaskStateRunning
	}
	r := New(f, AddrHostname)
	got, err := r.Services(context.Background())
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 service, got %+v", got)
	}
	if got[0].Desired != 2 || got[0].Running != 1 {
		t.Errorf("fallback counts = %d/%d, want 1/2", got[0].Running, got[0].Desired)
	}
}
