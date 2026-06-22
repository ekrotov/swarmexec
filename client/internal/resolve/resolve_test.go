package resolve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
)

// fakeDocker is an in-memory DockerClient honoring the filters the resolver uses
// (name, service, id, desired-state).
type fakeDocker struct {
	services []swarm.Service
	tasks    []swarm.Task
	nodes    map[string]swarm.Node
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
