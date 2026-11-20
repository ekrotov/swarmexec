// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/container"
	"google.golang.org/grpc/codes"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
	"swarmexec/internal/policy"
)

// The example policy of site/docs/authentication.html, loaded into a real
// Server through auth.LoadPolicy — not a hand-built policy.Request, which is
// how a rule that no handler can ever trigger stayed green (SEC-01).
const docsPolicy = `
default: allow
rules:
  - deny: [image.prune.all]
    reason: no sweeping prune on production
  - deny: [portforward]
    ports: [5432, 3306]
    reason: no forwards to the databases
  - deny: [exec]
    users: [root, "0"]
    reason: no root shell
  - deny: [volume.remove, volume.create]
    volumes: ["prod_*"]
    reason: production volumes stay
`

func docsPolicyServer(t *testing.T, d *fakeDocker) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(docsPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := auth.LoadPolicy(path, false)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := auditedTestServer(d, p)
	return srv
}

func taskInspect(id, user string) container.InspectResponse {
	return container.InspectResponse{ID: id, Config: &container.Config{
		User:   user,
		Labels: map[string]string{swarmServiceLabel: "prod_db", swarmTaskLabel: "t1"},
	}}
}

func TestDocsPolicyProtectsVolumesByName(t *testing.T) {
	d := newFakeDocker()
	srv := docsPolicyServer(t, d)
	ctx := context.Background()

	if _, err := srv.RemoveVolume(ctx, &pb.RemoveVolumeRequest{Name: "prod_db_data"}); statusCode(err) != codes.PermissionDenied {
		t.Errorf("RemoveVolume(prod_db_data) = %v, want PermissionDenied", err)
	}
	if _, err := srv.CreateVolume(ctx, &pb.CreateVolumeRequest{Name: "prod_new"}); statusCode(err) != codes.PermissionDenied {
		t.Errorf("CreateVolume(prod_new) = %v, want PermissionDenied", err)
	}
	if len(d.removedVols) != 0 {
		t.Errorf("a refused removal reached the daemon: %v", d.removedVols)
	}
	if _, err := srv.RemoveVolume(ctx, &pb.RemoveVolumeRequest{Name: "staging_data"}); err != nil {
		t.Errorf("RemoveVolume(staging_data): %v", err)
	}
	if !slices.Equal(d.removedVols, []string{"staging_data"}) {
		t.Errorf("removed = %v, want [staging_data]", d.removedVols)
	}
}

// users: [root, "0"] must stop a root shell however it is spelled — and without
// -u, which is the normal case, by what the image runs as.
func TestDocsPolicyDeniesEveryRootSpelling(t *testing.T) {
	cases := []struct {
		imageUser, asked string
		denied           bool
	}{
		{"", "root", true},
		{"", "0", true},
		{"", "0:0", true},
		{"", "", true}, // no -u on an image without USER: root
		{"", "root:root", true},
		{"", "0:1", true},
		{"", "00", true},
		{"", "1000", false},
		{"", "app:app", false},
		{"app", "", false}, // the image drops privileges
		{"1000:1000", "", false},
		{"0", "", true},
		{"root:wheel", "", true},
		{"app", "root", true}, // an explicit -u beats the image's user
		{"root", "app", false},
	}
	d := newFakeDocker()
	d.inspect["c-root"] = taskInspect("c-root", "")
	d.inspect["c-app"] = taskInspect("c-app", "app")
	d.inspect["c-0"] = taskInspect("c-0", "0")
	d.inspect["c-rootwheel"] = taskInspect("c-rootwheel", "root:wheel")
	d.inspect["c-1000"] = taskInspect("c-1000", "1000:1000")
	d.inspect["c-r"] = taskInspect("c-r", "root")
	byImageUser := map[string]string{"": "c-root", "app": "c-app", "0": "c-0", "root:wheel": "c-rootwheel", "1000:1000": "c-1000", "root": "c-r"}
	srv := docsPolicyServer(t, d)
	for _, c := range cases {
		_, err := srv.authorize(context.Background(), auth.Request{Action: "exec", ContainerID: byImageUser[c.imageUser], User: c.asked})
		if got := err != nil; got != c.denied {
			t.Errorf("exec -u %q on an image running as %q: denied=%v, want %v (%v)", c.asked, c.imageUser, got, c.denied, err)
		}
	}
}

// recordingAuthz records every Request and refuses it, so a handler stops right
// after the gate.
type recordingAuthz struct {
	mu   sync.Mutex
	reqs []auth.Request
}

func (r *recordingAuthz) Authorize(_ context.Context, q auth.Request) auth.Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, q)
	return auth.Decision{Reason: "recorded"}
}

// The condition table in internal/policy says which Request fields an action
// carries. This drives every handler with every input it could pass on and
// checks that the Request it builds fills exactly those fields — so the table,
// which Parse trusts to reject dead rules, cannot drift from the handlers.
func TestRequestFieldsMatchPolicyTable(t *testing.T) {
	d := newFakeDocker()
	d.inspect["c1"] = taskInspect("c1", "")
	rec := &recordingAuthz{}
	srv, _ := auditedTestServer(d, rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, _ = srv.ListContainers(ctx, &pb.ListRequest{})
	_, _ = srv.Stats(ctx, &pb.StatsRequest{})
	_, _ = srv.ListImages(ctx, &pb.ListImagesRequest{})
	_, _ = srv.PruneImages(ctx, &pb.PruneImagesRequest{})
	_, _ = srv.PruneImages(ctx, &pb.PruneImagesRequest{All: true})
	_, _ = srv.ListVolumes(ctx, &pb.ListVolumesRequest{})
	_, _ = srv.CreateVolume(ctx, &pb.CreateVolumeRequest{Name: "v"})
	_, _ = srv.RemoveVolume(ctx, &pb.RemoveVolumeRequest{Name: "v"})
	_, _ = srv.NetworkEndpoints(ctx, &pb.NetworkEndpointsRequest{})
	_, _ = srv.ReclaimStats(ctx, &pb.ReclaimStatsRequest{})
	_, _ = srv.Prune(ctx, &pb.PruneRequest{Categories: everyCategory})
	_, _ = srv.KillContainer(ctx, &pb.KillContainerRequest{ContainerId: "c1"})
	_, _ = srv.InspectContainer(ctx, &pb.InspectContainerRequest{ContainerId: "c1"})
	_, _ = srv.StatPath(ctx, &pb.StatPathRequest{ContainerId: "c1", Path: "/tmp"})
	_ = srv.CopyFrom(&pb.CopyFromRequest{ContainerId: "c1", Path: "/tmp/x"}, &fakeCopyFromStream{ctx: ctx})
	_ = srv.CopyTo(&fakeCopyToStream{ctx: ctx, msgs: copyToMsgs("/tmp", 0, "x")})
	_ = srv.Logs(&pb.LogsRequest{ContainerId: "c1"}, &fakeLogsStream{ctx: ctx})
	_ = srv.WatchContainerEvents(&pb.WatchContainerEventsRequest{ContainerId: "c1"}, &fakeEventStream{ctx: ctx})
	fs := newFakeStream(ctx)
	fs.queueStart(&pb.StartExec{ContainerId: "c1", Cmd: []string{"sh"}, User: "1000"})
	_ = srv.Exec(fs)
	ff := newFakeForwardStream(ctx)
	ff.queueStart("c1", 5432)
	_ = srv.PortForward(ff)

	seen := map[string]bool{}
	for _, q := range rec.reqs {
		seen[q.Action] = true
		want := policy.Carries(q.Action)
		got := policy.Conditions{
			Services: q.Service != "",
			Ports:    q.Port != 0,
			Users:    q.User != "" || q.EffectiveUser != "",
			Volumes:  q.Volume != "",
		}
		if got != want {
			t.Errorf("%s fills %+v, but the policy table says it carries %+v", q.Action, got, want)
		}
	}
	for _, a := range policy.Actions {
		if !seen[a] {
			t.Errorf("action %s was never authorized by any handler driven here", a)
		}
	}
}
