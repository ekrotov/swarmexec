// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package clusterread

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmfake"
)

func TestClassifyNode(t *testing.T) {
	cases := []struct {
		name   string
		r      agentcall.NodeVersion
		binary string
		status string
		ok     bool
	}{
		{"too old", agentcall.NodeVersion{Err: status.Error(codes.Unimplemented, "no Version")}, "v1.2.0", "too old (init --force)", false},
		{"unreachable", agentcall.NodeVersion{Err: errors.New("connection refused")}, "v1.2.0", "unreachable", false},
		{"proto mismatch", agentcall.NodeVersion{Version: "v1.2.0", Proto: "0"}, "v1.2.0", "PROTO MISMATCH", false},
		{"skew", agentcall.NodeVersion{Version: "v1.1.0", Proto: "7"}, "v1.2.0", "ok (version skew)", true},
		{"dev build never skews", agentcall.NodeVersion{Version: "v1.1.0", Proto: "7"}, "dev", "ok", true},
		{"same", agentcall.NodeVersion{Version: "v1.2.0", Proto: "7"}, "v1.2.0", "ok", true},
	}
	for _, c := range cases {
		st, _, _, ok := ClassifyNode(c.r, c.binary, "7")
		if st != c.status || ok != c.ok {
			t.Errorf("%s: got %q/%v, want %q/%v", c.name, st, ok, c.status, c.ok)
		}
	}
}

// A CPU reading with no known denominator must not invent one.
func TestFormatCPUUsage(t *testing.T) {
	if got := FormatCPUUsage(agentcall.ContainerUsage{}); got != "…" {
		t.Errorf("unready cpu = %q, want the ellipsis, not 0", got)
	}
	got := FormatCPUUsage(agentcall.ContainerUsage{CPUReady: true, CPUPercent: 250})
	if strings.Contains(got, "/") || strings.Contains(got, "%") || !strings.Contains(got, "2.50 cores") {
		t.Errorf("cpu = %q, want just the measured cores when nothing bounds them", got)
	}
	got = FormatCPUUsage(agentcall.ContainerUsage{CPUReady: true, CPUPercent: 50, CPULimitCores: 2, CPURatio: 0.25})
	if got != "0.50 / 2.00 cores (25%)" {
		t.Errorf("limited cpu = %q", got)
	}
}

func TestFormatMemUsage(t *testing.T) {
	if got := FormatMemUsage(agentcall.ContainerUsage{}); got != "0B" {
		t.Errorf("zero memory = %q, want 0B (a cell needs a value)", got)
	}
	got := FormatMemUsage(agentcall.ContainerUsage{MemBytes: 128 << 20, MemLimitBytes: 256 << 20, MemRatio: 0.5})
	if !strings.Contains(got, " / ") || !strings.HasSuffix(got, "(50%)") {
		t.Errorf("limited memory = %q, want used / limit (50%%)", got)
	}
}

func TestHealthOf(t *testing.T) {
	cands := []resolve.Candidate{
		{Service: "web", ContainerID: "c1"}, {Service: "web", ContainerID: "c2"},
		{Service: "web", ContainerID: "c3"}, {Service: "web", ContainerID: "c4"},
		{Service: "db", ContainerID: "c5"},
	}
	usage := map[string]agentcall.ContainerUsage{
		"c1": {Health: HealthHealthy}, "c2": {Health: HealthUnhealthy}, "c3": {Health: HealthStarting},
		"c5": {Health: HealthUnhealthy},
	}
	h := HealthOf(usage, cands, "web")
	if h != (ServiceHealth{Healthy: 1, Unhealthy: 1, Starting: 1, Unknown: 1}) {
		t.Errorf("web = %+v", h)
	}
	if h.Total() != 4 || h.Checked() != 3 {
		t.Errorf("total/checked = %d/%d, want 4/3 (no reading is not a verdict)", h.Total(), h.Checked())
	}
}

func TestFormatUptime(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                "-",
		45 * time.Second:                 "45s",
		12 * time.Minute:                 "12m",
		3*time.Hour + 5*time.Minute:      "3h5m",
		(3*24+4)*time.Hour + time.Minute: "3d4h",
	} {
		if got := FormatUptime(d); got != want {
			t.Errorf("FormatUptime(%v) = %q, want %q", d, got, want)
		}
	}
}

// The topology read lists every service — with no running container too —
// and every running container.
func TestServicesAndContainers(t *testing.T) {
	var svc swarm.Service
	svc.ID, svc.Spec.Name = "svc-web", "web"
	var idle swarm.Service
	idle.ID, idle.Spec.Name = "svc-idle", "idle"
	var node swarm.Node
	node.ID, node.Description.Hostname = "n1", "host-a"
	node.Status.Addr = "10.0.0.1"
	var task swarm.Task
	task.ID, task.ServiceID, task.NodeID, task.Slot = "t1", "svc-web", "n1", 1
	task.DesiredState, task.Status.State = swarm.TaskStateRunning, swarm.TaskStateRunning
	task.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "c1"}

	api := httptest.NewServer(&swarmfake.Swarm{Services: []swarm.Service{svc, idle}, Nodes: []swarm.Node{node}, Tasks: []swarm.Task{task}})
	defer api.Close()
	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	defer dcli.Close()

	svcs, cands, err := ServicesAndContainers(context.Background(), resolve.New(dcli, resolve.AddrHostname), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 2 {
		t.Errorf("got %d services, want both (the idle one too)", len(svcs))
	}
	if len(cands) != 1 || cands[0].ContainerID != "c1" || cands[0].Service != "web" {
		t.Errorf("containers = %+v, want web's c1", cands)
	}
}
