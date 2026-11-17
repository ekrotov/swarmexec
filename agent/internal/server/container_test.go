// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"google.golang.org/grpc/codes"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// swarmTaskFake is a fake daemon holding one swarm task container ("task1")
// and one plain container ("plain").
func swarmTaskFake() *fakeDocker {
	d := newFakeDocker()
	d.inspect["task1"] = container.InspectResponse{
		ID: "task1full",
		Config: &container.Config{
			Labels: map[string]string{swarmServiceLabel: "web", swarmTaskLabel: "t-1"},
			Env:    []string{"DB_PASSWORD=hunter2"},
		},
	}
	d.inspect["plain"] = container.InspectResponse{
		ID:     "plainfull",
		Config: &container.Config{Labels: map[string]string{"x": "y"}},
	}
	return d
}

func TestKillContainerKillsASwarmTaskAndAudits(t *testing.T) {
	d := swarmTaskFake()
	var seen []auth.Request
	srv, log := auditedTestServer(d, denyActions{seen: &seen})

	if _, err := srv.KillContainer(context.Background(), &pb.KillContainerRequest{ContainerId: "task1"}); err != nil {
		t.Fatalf("KillContainer: %v", err)
	}
	if len(d.killed) != 1 || d.killed[0] != "task1full" || d.killOpts[0].Signal != "KILL" {
		t.Errorf("killed = %v opts = %v, want task1full with SIGKILL", d.killed, d.killOpts)
	}
	if len(seen) != 1 || seen[0].Action != "kill" || seen[0].Service != "web" || seen[0].ContainerID != "task1" {
		t.Errorf("authorized = %+v", seen)
	}
	rec := findAuditEvent(t, log.String(), "container_kill")
	wantFields(t, rec, map[string]any{"identity": "test-user", "container_id": "task1", "service": "web", "ok": true})
}

func TestKillContainerRefusesNonSwarmContainer(t *testing.T) {
	d := swarmTaskFake()
	srv, log := auditedTestServer(d, auth.AllowAll{})

	_, err := srv.KillContainer(context.Background(), &pb.KillContainerRequest{ContainerId: "plain"})
	if statusCode(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "not a swarm task") {
		t.Fatalf("want FailedPrecondition not a swarm task, got %v", err)
	}
	if len(d.killed) != 0 {
		t.Error("a non-swarm container must never be killed")
	}
	rec := findAuditEvent(t, log.String(), "container_kill")
	if rec["ok"] != false || !strings.Contains(rec["error"].(string), "not a swarm task") {
		t.Errorf("audit = %v", rec)
	}
}

func TestKillContainerPolicyDenial(t *testing.T) {
	d := swarmTaskFake()
	srv, log := auditedTestServer(d, denyActions{deny: map[string]bool{"kill": true}})

	_, err := srv.KillContainer(context.Background(), &pb.KillContainerRequest{ContainerId: "task1"})
	if statusCode(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if len(d.killed) != 0 {
		t.Error("a refused kill must not reach Docker")
	}
	rec := findAuditEvent(t, log.String(), "auth_decision")
	if rec["allow"] != false {
		t.Errorf("audit = %v", rec)
	}
}

func TestKillContainerErrors(t *testing.T) {
	srv, _ := newTestServer(swarmTaskFake(), auth.AllowAll{}, Options{})
	if _, err := srv.KillContainer(context.Background(), &pb.KillContainerRequest{}); statusCode(err) != codes.InvalidArgument {
		t.Errorf("empty id: %v", err)
	}

	d := swarmTaskFake()
	d.inspectErr = map[string]error{"gone": cerrdefs.ErrNotFound.WithMessage("No such container: gone")}
	srv, _ = newTestServer(d, auth.AllowAll{}, Options{})
	if _, err := srv.KillContainer(context.Background(), &pb.KillContainerRequest{ContainerId: "gone"}); statusCode(err) != codes.NotFound {
		t.Errorf("missing container: %v", err)
	}

	d = swarmTaskFake()
	d.killErr = cerrdefs.ErrConflict.WithMessage("container is not running")
	srv, log := auditedTestServer(d, auth.AllowAll{})
	_, err := srv.KillContainer(context.Background(), &pb.KillContainerRequest{ContainerId: "task1"})
	if statusCode(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "kill container") {
		t.Errorf("stopped container: %v", err)
	}
	if rec := findAuditEvent(t, log.String(), "container_kill"); rec["ok"] != false {
		t.Errorf("audit = %v", rec)
	}
}

func TestInspectContainerReturnsTheDocument(t *testing.T) {
	d := swarmTaskFake()
	var seen []auth.Request
	srv, log := auditedTestServer(d, denyActions{seen: &seen})

	resp, err := srv.InspectContainer(context.Background(), &pb.InspectContainerRequest{ContainerId: "task1"})
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	var got container.InspectResponse
	if err := json.Unmarshal(resp.GetInspectJson(), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if got.ID != "task1full" || got.Config.Labels[swarmServiceLabel] != "web" {
		t.Errorf("document = %+v", got)
	}
	if len(seen) != 1 || seen[0].Action != "inspect" || seen[0].Service != "web" {
		t.Errorf("authorized = %+v", seen)
	}
	out := log.String()
	rec := findAuditEvent(t, out, "container_inspect")
	wantFields(t, rec, map[string]any{"container_id": "task1", "service": "web", "ok": true})
	if strings.Contains(out, "hunter2") {
		t.Error("the audit trail must never carry the inspected environment")
	}
}

func TestInspectContainerPassesTheEngineBytesThrough(t *testing.T) {
	d := swarmTaskFake()
	raw := []byte(`{"Id":"task1full","Config":{"Labels":{"com.docker.swarm.task.id":"t-1"}},"Extra":1}`)
	d.inspectRaw = map[string][]byte{"task1": raw}
	srv, _ := newTestServer(d, auth.AllowAll{}, Options{})
	resp, err := srv.InspectContainer(context.Background(), &pb.InspectContainerRequest{ContainerId: "task1"})
	if err != nil || string(resp.GetInspectJson()) != string(raw) {
		t.Errorf("got %s, %v; want the Engine's bytes verbatim", resp.GetInspectJson(), err)
	}
}

func TestInspectContainerRefusals(t *testing.T) {
	d := swarmTaskFake()
	srv, log := auditedTestServer(d, auth.AllowAll{})
	if _, err := srv.InspectContainer(context.Background(), &pb.InspectContainerRequest{}); statusCode(err) != codes.InvalidArgument {
		t.Errorf("empty id: %v", err)
	}
	_, err := srv.InspectContainer(context.Background(), &pb.InspectContainerRequest{ContainerId: "plain"})
	if statusCode(err) != codes.FailedPrecondition {
		t.Errorf("non-swarm container: %v", err)
	}
	if rec := findAuditEvent(t, log.String(), "container_inspect"); rec["ok"] != false {
		t.Errorf("audit = %v", rec)
	}

	srv, _ = newTestServer(swarmTaskFake(), denyActions{deny: map[string]bool{"inspect": true}}, Options{})
	if _, err := srv.InspectContainer(context.Background(), &pb.InspectContainerRequest{ContainerId: "task1"}); statusCode(err) != codes.PermissionDenied {
		t.Errorf("policy denial: %v", err)
	}
}
