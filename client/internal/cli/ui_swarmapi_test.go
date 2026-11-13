// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/rivo/tview"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmapi"
	pb "swarmexec/internal/pb"
)

// TUI tests against a fake swarmapi.Client — the seam review Q-06 asked for.
// Before it, clusterState.dcli was a *client.Client, so a UI test could only
// fake the manager by standing up an HTTP server for the real client to talk
// to (mcp_fake_test.go, gatedAPI). Behind the interface the manager is a Go
// value the test shapes call by call.

// fakeManager is a swarmapi.Client. The embedded interface is nil: a manager
// call a test did not plan for panics with its method name, which is the
// honest failure for a fake.
type fakeManager struct {
	swarmapi.Client

	mu        sync.Mutex
	services  []swarm.Service
	nodes     []swarm.Node
	updateErr error
	updates   []client.ServiceUpdateOptions
}

func (f *fakeManager) ServiceList(_ context.Context, o client.ServiceListOptions) (client.ServiceListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []swarm.Service
	for _, s := range f.services {
		if matchService(s, o.Filters) {
			out = append(out, s)
		}
	}
	return client.ServiceListResult{Items: out}, nil
}

func (f *fakeManager) ServiceInspect(_ context.Context, id string, _ client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.services {
		if s.ID == id || s.Spec.Name == id {
			return client.ServiceInspectResult{Service: s}, nil
		}
	}
	return client.ServiceInspectResult{}, fmt.Errorf("no such service: %s", id)
}

func (f *fakeManager) ServiceUpdate(_ context.Context, _ string, o client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return client.ServiceUpdateResult{}, f.updateErr
	}
	f.updates = append(f.updates, o)
	return client.ServiceUpdateResult{}, nil
}

func (f *fakeManager) NodeList(context.Context, client.NodeListOptions) (client.NodeListResult, error) {
	return client.NodeListResult{Items: f.nodes}, nil
}

func (f *fakeManager) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	return client.SystemInfoResult{}, nil
}

func readyNode(id, host string) swarm.Node {
	var n swarm.Node
	n.ID = id
	n.Description.Hostname = host
	n.Status.State = swarm.NodeStateReady
	return n
}

// volumeAgent answers the two RPCs the volumes tab fans out; with fail it
// answers like an agent whose Docker daemon went away mid-listing.
type volumeAgent struct {
	pb.UnimplementedAgentServer
	vols []string
	fail bool
}

func (a *volumeAgent) ListVolumes(context.Context, *pb.ListVolumesRequest) (*pb.ListVolumesResponse, error) {
	if a.fail {
		return nil, status.Error(codes.Unavailable, "list volumes: Cannot connect to the Docker daemon")
	}
	out := &pb.ListVolumesResponse{}
	for _, v := range a.vols {
		out.Volumes = append(out.Volumes, &pb.VolumeInfo{Name: v, Driver: "local", SizeBytes: 1 << 20, SizeKnown: true})
	}
	return out, nil
}

func (a *volumeAgent) ListContainers(context.Context, *pb.ListRequest) (*pb.ListResponse, error) {
	if a.fail {
		return nil, status.Error(codes.Unavailable, "list containers: Cannot connect to the Docker daemon")
	}
	return &pb.ListResponse{Containers: []*pb.ContainerInfo{{Name: "web.1", Service: "web", Volumes: a.vols}}}, nil
}

// startVolumeAgents serves one fake agent per node host and returns a client
// config whose dialer reaches each host's own agent.
func startVolumeAgents(t *testing.T, byHost map[string]*volumeAgent) config.Config {
	t.Helper()
	addrs := map[string]string{}
	for host, a := range byHost {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{selfSignedPair(t)}})))
		pb.RegisterAgentServer(gs, a)
		go func() { _ = gs.Serve(lis) }()
		t.Cleanup(gs.Stop)
		addrs[net.JoinHostPort(host, "7000")] = lis.Addr().String()
	}
	return config.Config{
		Port:        7000,
		AddrMode:    config.AddrModeHostname,
		Insecure:    true,
		AgentSecret: "s3cret",
		ProxyDialer: func(ctx context.Context, addr string) (net.Conn, error) {
			real, ok := addrs[addr]
			if !ok {
				return nil, fmt.Errorf("dial %s: connection refused", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", real)
		},
	}
}

// volumesUI is runningUI wired for the volumes tab against mgr.
func volumesUI(t *testing.T, mgr *fakeManager, cfg config.Config) *ui {
	t.Helper()
	u := runningUI(t)
	onLoop(t, u, func() {
		u.dcli = mgr
		u.r = resolve.New(mgr, resolve.AddrHostname)
		u.cfg = cfg
		u.agents = agentcall.New(cfg)
		u.f = &uiFlags{connectTimeout: 3 * time.Second}
		u.vtable = tview.NewTable()
	})
	return u
}

// tableText is every cell of t, row by row, read on the UI loop.
func tableText(t *testing.T, u *ui, tbl *tview.Table) string {
	t.Helper()
	var b strings.Builder
	onLoop(t, u, func() {
		for r := 0; r < tbl.GetRowCount(); r++ {
			for c := 0; c < tbl.GetColumnCount(); c++ {
				if cell := tbl.GetCell(r, c); cell != nil {
					b.WriteString(cell.Text)
				}
				b.WriteString("|")
			}
			b.WriteString("\n")
		}
	})
	return b.String()
}

func waitForTable(t *testing.T, u *ui, tbl *tview.Table, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		text := tableText(t, u, tbl)
		all := true
		for _, w := range want {
			all = all && strings.Contains(text, w)
		}
		if all {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatalf("table never showed %q:\n%s", want, text)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitVolumesSettled waits for the load's second pass (sizes) to finish, so no
// fan-out goroutine of this test outlives it and logs into the next one.
func waitVolumesSettled(t *testing.T, u *ui) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		done := false
		onLoop(t, u, func() { done = !u.volSizesLoading && u.volSizes != nil })
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the volume sizes pass never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One node's agent fails in the middle of the fan-out. The tab must show what
// the other node returned — its volume, its consumer, its size — and say that
// one node was not reached, rather than failing the whole load.
func TestVolumesLoad_OneNodeFailsMidFanOut(t *testing.T) {
	mgr := &fakeManager{nodes: []swarm.Node{readyNode("n1", "node-a"), readyNode("n2", "node-b")}}
	cfg := startVolumeAgents(t, map[string]*volumeAgent{
		"node-a": {vols: []string{"shop_data"}},
		"node-b": {fail: true},
	})
	u := volumesUI(t, mgr, cfg)
	onLoop(t, u, func() { u.loadVolumes() })

	waitVolumesSettled(t, u)
	text := waitForTable(t, u, u.vtable, "shop_data", "1: node-a", "(1 node(s) unreachable)", "1.049MB")
	if strings.Contains(text, errNoAgent.Error()) {
		t.Errorf("one failed node must not read as \"no agent\":\n%s", text)
	}
	var errs map[string]error
	onLoop(t, u, func() { errs = u.volErrs })
	if err := errs["node-b"]; status.Code(err) != codes.Unavailable {
		t.Errorf("node-b's error = %v, want its Unavailable kept", err)
	}
	if _, ok := errs["node-a"]; ok {
		t.Error("node-a answered and must not be listed as failed")
	}
}

// Every node fails and the manager knows no agent service: the tab says to
// run init. The "is an agent deployed" question goes to the manager, which a
// test could not answer before the seam without a whole fake Engine API.
func TestVolumesLoad_AllNodesFailAndNoAgentDeployed(t *testing.T) {
	mgr := &fakeManager{nodes: []swarm.Node{readyNode("n1", "node-a"), readyNode("n2", "node-b")}}
	cfg := startVolumeAgents(t, map[string]*volumeAgent{"node-a": {fail: true}}) // node-b: refused
	u := volumesUI(t, mgr, cfg)
	onLoop(t, u, func() { u.loadVolumes() })
	waitForTable(t, u, u.vtable, errNoAgent.Error())

	// Same failure with the agent service deployed: no "run init" — the
	// nodes are merely unreachable.
	mgr.mu.Lock()
	agentSvc := swarm.Service{ID: "a1"}
	agentSvc.Spec.Name = defaultServiceNm
	agentSvc.Spec.Labels = map[string]string{agentRoleLabel: agentRoleValue}
	mgr.services = []swarm.Service{agentSvc}
	mgr.mu.Unlock()
	onLoop(t, u, func() { u.loadVolumes() })
	waitVolumesSettled(t, u)
	text := waitForTable(t, u, u.vtable, "(2 node(s) unreachable)")
	if strings.Contains(text, errNoAgent.Error()) {
		t.Errorf("a deployed agent must not be reported missing:\n%s", text)
	}
}

func scalableService(name string, replicas uint64) swarm.Service {
	s := swarm.Service{ID: "svc-" + name}
	s.Spec.Name = name
	s.Version.Index = 41
	s.Spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &replicas}
	return s
}

// waitForScalePrompt returns the scale prompt's input field once it is up.
func waitForScalePrompt(t *testing.T, u *ui) *tview.InputField {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var in *tview.InputField
		onLoop(t, u, func() {
			if name, _ := u.pages.GetFrontPage(); name == pageScalePrompt {
				in, _ = u.app.GetFocus().(*tview.InputField)
			}
		})
		if in != nil {
			return in
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scale prompt did not open (front page %q)", frontPage(t, u))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The scale editor end to end: load the current replica count from the
// manager, prefill the prompt with it, take the operator's Enter, and write
// back exactly one update — the new count, at the version that was read, so
// the manager can refuse it if someone else changed the service meanwhile.
func TestScaleEditor_LoadsAndApplies(t *testing.T) {
	r := captureLog(t)
	mgr := &fakeManager{services: []swarm.Service{scalableService("web", 2)}}
	u := runningUI(t)
	after := make(chan struct{}, 1)
	onLoop(t, u, func() {
		u.dcli = mgr
		u.openScalePrompt("web", tview.NewBox(), func() { after <- struct{}{} })
	})

	in := waitForScalePrompt(t, u)
	var prefill string
	onLoop(t, u, func() { prefill = in.GetText() })
	if prefill != "2" {
		t.Fatalf("prompt prefilled with %q, want the current 2", prefill)
	}
	onLoop(t, u, func() { in.SetText("5") })
	u.app.QueueEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	select {
	case <-after:
	case <-time.After(5 * time.Second):
		t.Fatal("the scale never completed")
	}
	waitForLog(t, r, "ui notice", `scaled \"web\" to 5`)
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.updates) != 1 {
		t.Fatalf("%d updates, want 1", len(mgr.updates))
	}
	up := mgr.updates[0]
	if got := *up.Spec.Mode.Replicated.Replicas; got != 5 {
		t.Errorf("replicas written = %d, want 5", got)
	}
	if up.Version.Index != 41 {
		t.Errorf("update at version %d, want the 41 that was read", up.Version.Index)
	}
}

// The manager refusing the write (a concurrent update moved the version) is
// shown to the operator, and the "after" refresh does not run as if it had
// worked.
func TestScaleEditor_ApplyFailureIsShown(t *testing.T) {
	r := captureLog(t)
	mgr := &fakeManager{
		services:  []swarm.Service{scalableService("web", 2)},
		updateErr: errors.New("update out of sequence"),
	}
	u := runningUI(t)
	after := make(chan struct{}, 1)
	onLoop(t, u, func() {
		u.dcli = mgr
		u.openScalePrompt("web", tview.NewBox(), func() { after <- struct{}{} })
	})
	in := waitForScalePrompt(t, u)
	onLoop(t, u, func() { in.SetText("3") })
	u.app.QueueEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	waitForLog(t, r, "ui notice", "scale failed: update out of sequence")
	select {
	case <-after:
		t.Fatal("after ran although the update failed")
	case <-time.After(50 * time.Millisecond):
	}
}
