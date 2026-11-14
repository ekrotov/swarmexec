// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/mcpaccess"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmfake"
	"swarmexec/internal/authmeta"
	pb "swarmexec/internal/pb"
)

// A fake swarm for the MCP tests: a Docker Engine API that answers the calls
// swarmexec makes, and a node agent that answers over real gRPC and TLS.
//
// Most tests in this package fake a narrow interface instead. The MCP server
// cannot be tested that way honestly: what has to be proven is that NOTHING it
// renders — through resolve, stackfile, secscan and the listing helpers, all of
// which talk to a *client.Client — lets a hidden service or an access label
// through. So the fake sits underneath the real client, and every layer above
// it runs as it does against a cluster.

// --- the fixture cluster ------------------------------------------------------

const (
	lEnable = "swarmexec.mcp.enable"
	lLogs   = "swarmexec.mcp.logs"
	lExec   = "swarmexec.mcp.exec"
)

// Names the assistant must never learn, from any tool.
var hiddenNames = []string{"shop_hidden", "shop_off", "ghost_x", "hidden_secret", "hidden_net", "hidden_vol", "hidden_cfg", "c-hidden", "c-off"}

func fixtureService(id, name, stack string, labels map[string]string, replicas uint64) swarm.Service {
	l := map[string]string{}
	for k, v := range labels {
		l[k] = v
	}
	if stack != "" {
		l["com.docker.stack.namespace"] = stack
	}
	var s swarm.Service
	s.ID = id
	s.Spec.Name = name
	s.Spec.Labels = l
	s.Spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &replicas}
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{
		Image:  "registry.example/" + name + ":1",
		Env:    []string{"PORT=8080", "DB_PASSWORD=hunter2", "LOG_LEVEL=info"},
		Labels: map[string]string{"app": name, lExec: "true"}, // a container label: grants nothing, must not show
	}
	s.ServiceStatus = &swarm.ServiceStatus{RunningTasks: replicas, DesiredTasks: replicas}
	s.Meta.CreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return s
}

func fixtureTask(id, svcID, node, container string, slot int) swarm.Task {
	var t swarm.Task
	t.ID, t.ServiceID, t.NodeID, t.Slot = id, svcID, node, slot
	t.DesiredState = swarm.TaskStateRunning
	t.Status.State = swarm.TaskStateRunning
	t.Status.Timestamp = time.Now().Add(-time.Hour)
	t.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: container}
	return t
}

func newFixtureSwarm() *swarmfake.Swarm {
	web := fixtureService("svc-web", "shop_web", "shop", map[string]string{lEnable: "true", lLogs: "true", lExec: "true"}, 2)
	web.Spec.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{SecretID: "sec-shop", SecretName: "shop_db_pw"}}
	web.Spec.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{{ConfigID: "cfg-shop", ConfigName: "shop_cfg"}}
	web.Spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "shop_data", Target: "/data"}}
	web.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "net-shop"}}

	api := fixtureService("svc-api", "shop_api", "shop", map[string]string{lEnable: "true", "swarmexec.mcp.exce": "true"}, 1)
	api.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "net-shop"}}

	hidden := fixtureService("svc-hidden", "shop_hidden", "shop", nil, 1)
	hidden.Spec.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{SecretID: "sec-hidden", SecretName: "hidden_secret"}}
	hidden.Spec.TaskTemplate.ContainerSpec.Configs = []*swarm.ConfigReference{{ConfigID: "cfg-hidden", ConfigName: "hidden_cfg"}}
	hidden.Spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "hidden_vol", Target: "/v"}}
	hidden.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "net-shop"}, {Target: "net-hidden"}}

	off := fixtureService("svc-off", "shop_off", "shop", map[string]string{lEnable: "false", lLogs: "true", lExec: "true"}, 1)
	off.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "net-hidden"}}

	sock := fixtureService("svc-sock", "sock", "", map[string]string{lEnable: "true", lLogs: "true", lExec: "true"}, 1)
	sock.Spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}}

	ghost := fixtureService("svc-ghost", "ghost_x", "ghost", nil, 1)

	node := func(id, host string, leader bool) swarm.Node {
		var n swarm.Node
		n.ID = id
		n.Description.Hostname = host
		n.Status.State = swarm.NodeStateReady
		n.Status.Addr = "10.0.0." + id[len(id)-1:]
		n.Spec.Availability = swarm.NodeAvailabilityActive
		if leader {
			n.Spec.Role = swarm.NodeRoleManager
			n.ManagerStatus = &swarm.ManagerStatus{Leader: true}
		} else {
			n.Spec.Role = swarm.NodeRoleWorker
		}
		return n
	}
	netw := func(id, name string, labels map[string]string) network.Summary {
		var n network.Summary
		n.ID, n.Name, n.Driver, n.Scope, n.Labels = id, name, "overlay", "swarm", labels
		return n
	}
	secret := func(id, name string) swarm.Secret {
		var s swarm.Secret
		s.ID, s.Spec.Name = id, name
		s.Spec.Labels = map[string]string{lEnable: "true"}
		return s
	}
	cfg := func(id, name string) swarm.Config {
		var c swarm.Config
		c.ID, c.Spec.Name = id, name
		return c
	}

	return &swarmfake.Swarm{
		Services: []swarm.Service{web, api, hidden, off, sock, ghost},
		Tasks: []swarm.Task{
			fixtureTask("t-web1", "svc-web", "node1", "c-web1", 1),
			fixtureTask("t-web2", "svc-web", "node2", "c-web2", 2),
			fixtureTask("t-api1", "svc-api", "node1", "c-api1", 1),
			fixtureTask("t-hidden1", "svc-hidden", "node2", "c-hidden", 1),
			fixtureTask("t-off1", "svc-off", "node1", "c-off", 1),
			fixtureTask("t-sock1", "svc-sock", "node1", "c-sock", 1),
			fixtureTask("t-ghost1", "svc-ghost", "node2", "c-ghost", 1),
		},
		Nodes: []swarm.Node{node("node1", "node-a", true), node("node2", "node-b", false)},
		Networks: []network.Summary{
			netw("net-shop", "shop_net", map[string]string{"com.docker.stack.namespace": "shop", lEnable: "true"}),
			netw("net-hidden", "hidden_net", nil),
		},
		Secrets: []swarm.Secret{secret("sec-shop", "shop_db_pw"), secret("sec-hidden", "hidden_secret")},
		Configs: []swarm.Config{cfg("cfg-shop", "shop_cfg"), cfg("cfg-hidden", "hidden_cfg")},
	}
}

// --- the fake agent -----------------------------------------------------------

type fakeAgent struct {
	pb.UnimplementedAgentServer

	mu        sync.Mutex
	execs     []string // container ids exec'd into
	logs      []string // container ids whose logs were read
	operators []string // operator identities the agent was told

	// coldCalls makes Stats behave like a real agent whose sampler was asleep:
	// the first coldCalls requests get nothing, the next coldCalls memory
	// without CPU, the rest everything. Counted, not timed, so a slow (-race)
	// run cannot skip a stage.
	coldCalls  int
	statsCalls int
}

func (a *fakeAgent) note(ctx context.Context, list *[]string, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	*list = append(*list, id)
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		a.operators = append(a.operators, md.Get(authmeta.OperatorKey)...)
	}
}

func (a *fakeAgent) Version(context.Context, *pb.VersionRequest) (*pb.VersionResponse, error) {
	return &pb.VersionResponse{Version: "test", ProtoVersion: pb.ProtocolVersion}, nil
}

func (a *fakeAgent) Logs(req *pb.LogsRequest, stream pb.Agent_LogsServer) error {
	a.note(stream.Context(), &a.logs, req.GetContainerId())
	for _, l := range []string{
		"2026-01-01T00:00:00Z INFO hello from " + req.GetContainerId(),
		"2026-01-01T00:00:01Z WARN disk at 80%",
		// An application that logs its own labels: the scrub has to catch it.
		"2026-01-01T00:00:02Z INFO my labels: swarmexec.mcp.enable=true",
		"2026-01-01T00:00:03Z ERROR boom",
	} {
		if err := stream.Send(&pb.LogChunk{Payload: &pb.LogChunk_Stdout{Stdout: []byte(l + "\n")}}); err != nil {
			return err
		}
	}
	return nil
}

func (a *fakeAgent) Stats(context.Context, *pb.StatsRequest) (*pb.StatsResponse, error) {
	a.mu.Lock()
	a.statsCalls++
	n, cold := a.statsCalls, a.coldCalls
	a.mu.Unlock()

	cpuReady := true
	switch {
	case n <= cold:
		return &pb.StatsResponse{NodeCpus: 4, NodeMemoryTotalBytes: 8 << 30}, nil
	case n <= 2*cold:
		cpuReady = false
	}
	var out []*pb.ContainerStats
	for _, id := range []string{"c-web1", "c-web2", "c-api1", "c-hidden", "c-off", "c-sock", "c-ghost"} {
		cs := &pb.ContainerStats{ContainerId: id, MemoryBytes: 64 << 20, MemoryLimitBytes: 256 << 20, MemoryLimited: true, Health: "healthy"}
		if cpuReady {
			cs.CpuPercent = 12
		}
		out = append(out, cs)
	}
	return &pb.StatsResponse{Stats: out, CpuReady: cpuReady, NodeCpus: 4, NodeMemoryTotalBytes: 8 << 30}, nil
}

func (a *fakeAgent) Exec(stream pb.Agent_ExecServer) error {
	msg, err := stream.Recv()
	if err != nil {
		return err
	}
	start := msg.GetStart()
	a.note(stream.Context(), &a.execs, start.GetContainerId())
	if err := stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_Stdout{Stdout: []byte("ran: " + strings.Join(start.GetCmd(), " ") + "\n")}}); err != nil {
		return err
	}
	if err := stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_Stderr{Stderr: []byte(strings.Repeat("e", 40<<10))}}); err != nil {
		return err
	}
	return stream.Send(&pb.ServerMessage{Payload: &pb.ServerMessage_ExitCode{ExitCode: 3}})
}

func (a *fakeAgent) snapshot() (execs, logs, operators []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.execs...), append([]string(nil), a.logs...), append([]string(nil), a.operators...)
}

// --- wiring -------------------------------------------------------------------

type mcpFixture struct {
	swarm *swarmfake.Swarm
	agent *fakeAgent
	srv   *mcpServer

	// connected records the context names the server tried to connect to.
	connected []string
	// downMu guards down: agent addresses ("node-b:7000") that do not answer.
	downMu sync.Mutex
	down   map[string]bool
}

// newMCPFixture starts the fake manager and agent and returns an MCP server
// whose default context is that fake cluster.
func newMCPFixture(t *testing.T, policyDefault string, allowReport bool) *mcpFixture {
	t.Helper()
	fs := newFixtureSwarm()
	api := httptest.NewServer(fs)
	t.Cleanup(api.Close)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{swarmfake.SelfSignedPair(t)}})))
	agent := &fakeAgent{}
	pb.RegisterAgentServer(gs, agent)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dcli.Close() })

	agentAddr := lis.Addr().String()
	fx := &mcpFixture{swarm: fs, agent: agent, down: map[string]bool{}}
	cfg := config.Config{
		Port:        7000,
		AddrMode:    config.AddrModeHostname,
		Insecure:    true,
		AgentSecret: "s3cret", // so the operator identity travels in metadata
		Operator:    mcpOperator("alice"),
		ProxyDialer: func(ctx context.Context, addr string) (net.Conn, error) {
			fx.downMu.Lock()
			down := fx.down[addr]
			fx.downMu.Unlock()
			if down {
				return nil, fmt.Errorf("dial %s: connection refused", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", agentAddr)
		},
	}

	def, err := mcpaccess.ParseDefault(policyDefault)
	if err != nil {
		t.Fatal(err)
	}
	g := &globalFlags{version: Version{Binary: "test", Proto: pb.ProtocolVersion}}
	s := newMCPServer(g, nil, mcpaccess.Policy{Default: def}, allowReport, 3*time.Second)
	// The real waits are seconds; the fake agent warms up in milliseconds.
	s.statsWait, s.statsPoll = 2*time.Second, 20*time.Millisecond
	fake := &mcpCluster{
		label: "fake", dcli: dcli, r: resolve.New(dcli, resolve.AddrHostname),
		agent: agentcall.New(cfg), cfgErr: cfg.Validate(),
	}
	// Every context the server agrees to connect to is the fake cluster; which
	// names it agrees to is the server's business (mcpServer.cluster), and is
	// what the context tests check.
	s.connect = func(_ context.Context, name string) (*mcpCluster, error) {
		fx.connected = append(fx.connected, name)
		return fake, nil
	}
	fx.srv = s
	return fx
}
