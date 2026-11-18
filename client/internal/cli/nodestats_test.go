// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
	"swarmexec/client/internal/swarmops"
	pb "swarmexec/internal/pb"
)

// opsAgent is a node agent for the node/stats/status commands: warm stats for
// its containers, two volumes, reclaim figures, and a record of each prune.
type opsAgent struct {
	pb.UnimplementedAgentServer

	mu     sync.Mutex
	pruned []*pb.PruneRequest
}

func (a *opsAgent) Stats(context.Context, *pb.StatsRequest) (*pb.StatsResponse, error) {
	return &pb.StatsResponse{CpuReady: true, NodeCpus: 4, NodeMemoryTotalBytes: 8 << 30, Stats: []*pb.ContainerStats{
		{ContainerId: "c-web1", CpuPercent: 12, MemoryBytes: 64 << 20, MemoryLimitBytes: 256 << 20, MemoryLimited: true, Health: "healthy"},
	}}, nil
}

func (a *opsAgent) ListVolumes(context.Context, *pb.ListVolumesRequest) (*pb.ListVolumesResponse, error) {
	return &pb.ListVolumesResponse{Volumes: []*pb.VolumeInfo{{Name: "data"}, {Name: "cache"}}}, nil
}

func (a *opsAgent) ReclaimStats(context.Context, *pb.ReclaimStatsRequest) (*pb.ReclaimStatsResponse, error) {
	return &pb.ReclaimStatsResponse{MeasuredAt: "2026-11-17T10:00:00Z", Categories: []*pb.ReclaimCategory{
		{Category: pb.PruneCategory_PRUNE_CATEGORY_CONTAINERS, Count: 3, Bytes: 1 << 20, BytesKnown: true},
	}}, nil
}

func (a *opsAgent) Prune(_ context.Context, req *pb.PruneRequest) (*pb.PruneResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruned = append(a.pruned, req)
	out := &pb.PruneResponse{}
	for _, c := range req.GetCategories() {
		r := &pb.PruneCategoryResult{Category: c, Count: 1, FreedBytes: 1 << 20}
		if c == pb.PruneCategory_PRUNE_CATEGORY_VOLUMES {
			r.Error = "volume [x] in use"
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

func (a *opsAgent) prunes() []*pb.PruneRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pruned
}

// nodeOpsSwarm: node n1 answers as an agent, n2 (127.0.0.2) does not. web runs
// on n1, db on n2.
func nodeOpsSwarm() *swarmfake.Swarm {
	n1 := readyNode("n1", agentNodeHost)
	n1.Spec.Role, n1.Spec.Availability = swarm.NodeRoleManager, swarm.NodeAvailabilityActive
	n1.ManagerStatus = &swarm.ManagerStatus{Leader: true}
	n1.Spec.Labels = map[string]string{"zone": "a", "old": "x"}
	n1.Version.Index = 1
	n2 := readyNode("n2", "127.0.0.2")
	n2.Spec.Role, n2.Spec.Availability = swarm.NodeRoleWorker, swarm.NodeAvailabilityActive
	return &swarmfake.Swarm{
		Services: []swarm.Service{stackedService("s1", "web", "shop", 1), stackedService("s2", "db", "shop", 1)},
		Tasks:    []swarm.Task{runningTask("s1", "c-web1", "n1", 1), runningTask("s2", "c-db1", "n2", 1)},
		Nodes:    []swarm.Node{n1, n2},
	}
}

// --- stats -------------------------------------------------------------------

func TestStatsTableAndJSON(t *testing.T) {
	g := cliCluster(t, nodeOpsSwarm(), &opsAgent{})
	out, errOut, err := runCmd(t, newStatsCmd(g), "--wait", "0")
	if err != nil {
		t.Fatal(err, errOut)
	}
	if !strings.Contains(out, "SERVICE") || !strings.Contains(out, "64MiB / 256MiB") || !strings.Contains(out, "healthy") {
		t.Errorf("table:\n%s", out)
	}
	if !strings.Contains(out, swarmops.StatsNoAgent) || !strings.Contains(errOut, "note: "+swarmops.StatsNoAgentNote) {
		t.Errorf("the dead node's container should say unavailable, once, with a note: %q / %q", out, errOut)
	}

	out, _, err = runCmd(t, newStatsCmd(g), "--json", "--wait", "0", "web")
	var rows []swarmops.StatsRow
	if jerr := json.Unmarshal([]byte(out), &rows); err != nil || jerr != nil || len(rows) != 1 || rows[0].Service != "web" || rows[0].Node != agentNodeHost {
		t.Errorf("json web: %q %v %v", out, err, jerr)
	}
}

func TestStatsUnknownServiceAndEmpty(t *testing.T) {
	g := cliCluster(t, nodeOpsSwarm(), &opsAgent{})
	_, _, err := runCmd(t, newStatsCmd(g), "--wait", "0", "nope")
	if exitCode(err) != 125 || !strings.Contains(err.Error(), `no service "nope"`) {
		t.Errorf("unknown: %v", err)
	}

	f := nodeOpsSwarm()
	f.Tasks = nil
	g = cliCluster(t, f, &opsAgent{})
	out, errOut, err := runCmd(t, newStatsCmd(g), "--wait", "0")
	if err != nil || out != "" || errOut != "no running containers\n" {
		t.Errorf("empty: %q %q %v", out, errOut, err)
	}
	out, _, err = runCmd(t, newStatsCmd(g), "--json", "--wait", "0")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("empty json: %q %v", out, err)
	}
}

// --- status ------------------------------------------------------------------

func TestStatusTablesAndJSON(t *testing.T) {
	g := cliCluster(t, nodeOpsSwarm(), &opsAgent{})
	out, errOut, err := runCmd(t, newStatusCmd(g))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"manager manager-1", agentNodeHost + " (leader)", "127.0.0.2", "web", "1 healthy", "2 node(s), 2 service(s) visible, 0 degraded"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if errOut != "" {
		t.Errorf("stderr: %q", errOut)
	}

	out, _, err = runCmd(t, newStatusCmd(g), "--json")
	var ov swarmops.Overview
	if jerr := json.Unmarshal([]byte(out), &ov); err != nil || jerr != nil || len(ov.Nodes) != 2 || len(ov.Services) != 2 || ov.Services[0].Health == nil {
		t.Errorf("json: %q %v %v", out, err, jerr)
	}
}

// Without agents the services are still listed, without health, and a note
// says why.
func TestStatusWithoutAgentsAndEmpty(t *testing.T) {
	f := nodeOpsSwarm()
	f.Services, f.Tasks = nil, nil
	g := cliCluster(t, f, nil)
	out, errOut, err := runCmd(t, newStatusCmd(g))
	if err != nil || !strings.Contains(out, "0 service(s)") || !strings.Contains(errOut, "no services") ||
		!strings.Contains(errOut, "note: "+swarmops.OverviewNoHealth) {
		t.Errorf("%q %q %v", out, errOut, err)
	}
}

func TestHealthTallyText(t *testing.T) {
	for _, c := range []struct {
		h    *swarmops.HealthTally
		want string
	}{
		{nil, "-"},
		{&swarmops.HealthTally{}, "-"},
		{&swarmops.HealthTally{Healthy: 2, Unhealthy: 1, NoCheck: 3}, "2 healthy, 1 unhealthy, 3 no check"},
		{&swarmops.HealthTally{Starting: 1}, "1 starting"},
	} {
		if got := healthTallyText(c.h); got != c.want {
			t.Errorf("%+v = %q, want %q", c.h, got, c.want)
		}
	}
}

// --- node ls -----------------------------------------------------------------

func TestNodeLs(t *testing.T) {
	g := cliCluster(t, nodeOpsSwarm(), &opsAgent{})
	out, _, err := runCmd(t, newNodeLsCmd(g))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NODE") {
		t.Fatalf("table:\n%s", out)
	}
	// Managers first; the answering node counts its two volumes, the dead
	// one is unknown, not zero.
	if f := strings.Fields(lines[1]); f[0] != agentNodeHost || f[len(f)-2] != "2" || f[len(f)-1] != "2" {
		t.Errorf("n1 row %q", lines[1])
	}
	if f := strings.Fields(lines[2]); f[0] != "127.0.0.2" || f[len(f)-2] != "-" {
		t.Errorf("n2 row %q", lines[2])
	}

	out, _, err = runCmd(t, newNodeLsCmd(g), "--json")
	var rows []nodeRow
	if jerr := json.Unmarshal([]byte(out), &rows); err != nil || jerr != nil || len(rows) != 2 {
		t.Fatalf("json: %q %v %v", out, err, jerr)
	}
	if rows[0].Volumes == nil || *rows[0].Volumes != 2 || rows[1].Volumes != nil || rows[0].Labels["zone"] != "a" || !rows[0].Leader {
		t.Errorf("json rows %+v", rows)
	}
	if !strings.Contains(out, `"volumes": null`) {
		t.Errorf("an unknown count should be null:\n%s", out)
	}

	out, _, err = runCmd(t, newNodeLsCmd(g), "--json", "--skip-agents")
	if err != nil || strings.Contains(out, `"volumes": 2`) {
		t.Errorf("--skip-agents asked the agents: %q", out)
	}
}

func TestNodeLsEmpty(t *testing.T) {
	g := cliCluster(t, &swarmfake.Swarm{}, nil)
	out, errOut, err := runCmd(t, newNodeLsCmd(g))
	if err != nil || out != "" || errOut != "no nodes\n" {
		t.Errorf("%q %q %v", out, errOut, err)
	}
	out, _, err = runCmd(t, newNodeLsCmd(g), "--json")
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("json: %q %v", out, err)
	}
}

// --- node prune --------------------------------------------------------------

func TestNodePrune(t *testing.T) {
	agent := &opsAgent{}
	g := cliCluster(t, nodeOpsSwarm(), agent)

	// No terminal, no --yes: it shows what would go, prunes nothing.
	out, errOut, err := runCmd(t, newNodePruneCmd(g), agentNodeHost)
	if exitCode(err) != 125 || !strings.Contains(errOut, "aborted") || len(agent.prunes()) != 0 {
		t.Errorf("unconfirmed: %q %q %v", out, errOut, err)
	}
	if !strings.Contains(out, "stopped containers (stopped > 24h) — 3 · 1MiB") || !strings.Contains(out, "untagged images — no figure") || strings.Contains(out, "[red]") {
		t.Errorf("confirm text:\n%s", out)
	}

	// By id prefix, with --yes: the defaults, the safe sweeps.
	out, _, err = runCmd(t, newNodePruneCmd(g), "n1", "--yes", "--containers-until", "2h")
	if err != nil || !strings.Contains(out, "Pruned on \""+agentNodeHost+"\"") || strings.Contains(out, "[green]") {
		t.Fatalf("pruned: %q %v", out, err)
	}
	reqs := agent.prunes()
	if len(reqs) != 1 || len(reqs[0].GetCategories()) != 4 || reqs[0].GetContainersUntil() != "2h" {
		t.Fatalf("requests %v", reqs)
	}
	for _, c := range reqs[0].GetCategories() {
		if c == pb.PruneCategory_PRUNE_CATEGORY_IMAGES || c == pb.PruneCategory_PRUNE_CATEGORY_VOLUMES {
			t.Errorf("default categories include %v", c)
		}
	}

	// A category that fails is the exit code, and its error is printed as is.
	out, _, err = runCmd(t, newNodePruneCmd(g), agentNodeHost, "--yes", "--categories", "volumes")
	if exitCode(err) != 125 || !strings.Contains(out, "volume [x] in use") {
		t.Errorf("failed category: %q %v", out, err)
	}
}

func TestNodePruneRefusals(t *testing.T) {
	g := cliCluster(t, nodeOpsSwarm(), &opsAgent{})
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"nope", "--yes"}, 125, `no node "nope"`},
		{[]string{"n", "--yes"}, exitUsage, "ambiguous"},
		{[]string{"n1", "--categories", "images,dangling-images"}, exitUsage, "name one of them"},
		{[]string{"n1", "--categories", "bogus"}, exitUsage, "not a category"},
		{[]string{"n1", "--containers-until", "soon"}, exitUsage, "duration"},
	} {
		_, _, err := runCmd(t, newNodePruneCmd(g), c.args...)
		if exitCode(err) != c.code || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%v: %v", c.args, err)
		}
	}
}

func TestPlainText(t *testing.T) {
	if got := plainText("[red]x[white] [[y] [-]z[gray]"); got != "x [y] z" {
		t.Errorf("%q", got)
	}
}

// --- node update -------------------------------------------------------------

func TestNodeUpdate(t *testing.T) {
	f := nodeOpsSwarm()
	g := cliCluster(t, f, nil)
	out, _, err := runCmd(t, newNodeUpdateCmd(g), agentNodeHost, "--label-add", "zone=b", "--label-add", "rack=1", "--label-rm", "old")
	if err != nil || !strings.Contains(out, "labels rack=1, zone=b") {
		t.Fatalf("labels: %q %v", out, err)
	}
	if l := f.Nodes[0].Spec.Labels; len(l) != 2 || l["zone"] != "b" || l["rack"] != "1" {
		t.Errorf("labels on the node: %v", l)
	}

	// Drain asks; without a terminal and --yes nothing changes.
	out, errOut, err := runCmd(t, newNodeUpdateCmd(g), "127.0.0.2", "--availability", "drain")
	if exitCode(err) != 125 || !strings.Contains(out, "Draining") || !strings.Contains(errOut, "aborted") || f.Nodes[1].Spec.Availability != swarm.NodeAvailabilityActive {
		t.Errorf("unconfirmed drain: %q %q %v", out, errOut, err)
	}
	out, _, err = runCmd(t, newNodeUpdateCmd(g), "127.0.0.2", "--availability", "DRAIN", "--yes")
	if err != nil || f.Nodes[1].Spec.Availability != swarm.NodeAvailabilityDrain || !strings.Contains(out, "availability drain") {
		t.Errorf("drain: %q %v", out, err)
	}
	// pause needs no confirm.
	if _, _, err = runCmd(t, newNodeUpdateCmd(g), "n2", "--availability", "pause"); err != nil || f.Nodes[1].Spec.Availability != swarm.NodeAvailabilityPause {
		t.Errorf("pause: %v", err)
	}
}

func TestNodeUpdateRefusals(t *testing.T) {
	g := cliCluster(t, nodeOpsSwarm(), nil)
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"n1"}, exitUsage, "nothing to change"},
		{[]string{"n1", "--availability", "off"}, exitUsage, "active, pause or drain"},
		{[]string{"n1", "--label-add", "novalue"}, exitUsage, "key=value"},
		{[]string{"nope", "--availability", "pause"}, 125, `no node "nope"`},
	} {
		_, _, err := runCmd(t, newNodeUpdateCmd(g), c.args...)
		if exitCode(err) != c.code || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%v: %v", c.args, err)
		}
	}
}

// --- the command tree ----------------------------------------------------------

// prune-schedule moved under node; the old top-level name still runs, out of
// the help. The new commands are in the root's help, the security group lists
// check next to report.
func TestNodeGroupAndAliases(t *testing.T) {
	root := newRootCmd(&globalFlags{}, Version{Binary: "dev", Proto: "1"})
	old, _, err := root.Find([]string{"prune-schedule", "show"})
	if err != nil || old.Parent().Name() != "prune-schedule" || !old.Parent().Hidden {
		t.Errorf("top-level prune-schedule: %v %v", old, err)
	}
	set, _, err := root.Find([]string{"node", "prune-schedule", "set"})
	if err != nil || !strings.Contains(set.Example, "swarmexec node prune-schedule set --disable") {
		t.Errorf("node prune-schedule set: %v", err)
	}
	if c, _, err := root.Find([]string{"nodes", "ls"}); err != nil || c.Name() != "ls" {
		t.Errorf("nodes alias: %v", err)
	}
	for _, path := range [][]string{{"security", "check"}, {"stack", "check"}, {"stack", "rm"}, {"stats"}, {"status"}, {"node", "prune"}, {"node", "update"}} {
		if c, _, err := root.Find(path); err != nil || c.Name() != path[len(path)-1] {
			t.Errorf("%v: %v", path, err)
		}
	}

	var help strings.Builder
	root.SetOut(&help)
	if err := groupedUsage(root); err != nil {
		t.Fatal(err)
	}
	h := help.String()
	for _, want := range []string{"\n  node ", "\n  stats ", "\n  status "} {
		if !strings.Contains(h, want) {
			t.Errorf("root help lacks %q", want)
		}
	}
	if strings.Contains(h, "prune-schedule") {
		t.Error("root help still lists prune-schedule")
	}
	sec, _, _ := root.Find([]string{"security"})
	if strings.Contains(sec.Short, "scan") || len(sec.Commands()) != 2 {
		t.Errorf("security group: %q, %d commands", sec.Short, len(sec.Commands()))
	}
}
