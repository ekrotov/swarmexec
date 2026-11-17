// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmfake

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// The fake answers the real client: lists with their filters, single objects
// by id or name, and a recorded 404 for anything it does not serve.
func TestSwarmAnswersTheRealClient(t *testing.T) {
	svc := func(id, name string, labels map[string]string) swarm.Service {
		var s swarm.Service
		s.ID, s.Spec.Name, s.Spec.Labels = id, name, labels
		return s
	}
	var task swarm.Task
	task.ID, task.ServiceID, task.DesiredState = "t1", "s1", swarm.TaskStateRunning
	var node swarm.Node
	node.ID = "n1"
	var sec swarm.Secret
	sec.ID, sec.Spec.Name = "sec1", "db_pw"
	var cfg swarm.Config
	cfg.ID, cfg.Spec.Name = "cfg1", "app_cfg"
	f := &Swarm{
		Services: []swarm.Service{svc("s1", "shop_web", map[string]string{"tier": "front"}), svc("s2", "shop_db", nil)},
		Tasks:    []swarm.Task{task},
		Nodes:    []swarm.Node{node},
		Networks: []network.Summary{{Network: network.Network{ID: "net1", Name: "shop_net"}}},
		Secrets:  []swarm.Secret{sec},
		Configs:  []swarm.Config{cfg},
	}
	api := httptest.NewServer(f)
	defer api.Close()
	c, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	if _, err := c.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := c.Info(ctx, client.InfoOptions{}); err != nil {
		t.Fatalf("info: %v", err)
	}
	if _, err := c.SwarmInspect(ctx, client.SwarmInspectOptions{}); err != nil {
		t.Fatalf("swarm: %v", err)
	}
	front, err := c.ServiceList(ctx, client.ServiceListOptions{Filters: make(client.Filters).Add("label", "tier=front")})
	if err != nil || len(front.Items) != 1 || front.Items[0].ID != "s1" {
		t.Errorf("label filter = %+v, %v", front.Items, err)
	}
	named, err := c.ServiceList(ctx, client.ServiceListOptions{Filters: make(client.Filters).Add("name", "shop_d")})
	if err != nil || len(named.Items) != 1 || named.Items[0].ID != "s2" {
		t.Errorf("name filter = %+v, %v", named.Items, err)
	}
	if got, err := c.ServiceInspect(ctx, "shop_web", client.ServiceInspectOptions{}); err != nil || got.Service.ID != "s1" {
		t.Errorf("inspect by name = %+v, %v", got.Service, err)
	}
	if _, err := c.ServiceInspect(ctx, "nope", client.ServiceInspectOptions{}); err == nil {
		t.Error("inspect of a missing service succeeded")
	}
	tasks, err := c.TaskList(ctx, client.TaskListOptions{Filters: make(client.Filters).Add("service", "s1").Add("desired-state", "running")})
	if err != nil || len(tasks.Items) != 1 {
		t.Errorf("tasks = %+v, %v", tasks.Items, err)
	}
	if none, _ := c.TaskList(ctx, client.TaskListOptions{Filters: make(client.Filters).Add("service", "s2")}); len(none.Items) != 0 {
		t.Errorf("service filter let other tasks through: %+v", none.Items)
	}
	if nodes, err := c.NodeList(ctx, client.NodeListOptions{}); err != nil || len(nodes.Items) != 1 {
		t.Errorf("nodes = %+v, %v", nodes.Items, err)
	}
	if n, err := c.NodeInspect(ctx, "n1", client.NodeInspectOptions{}); err != nil || n.Node.ID != "n1" {
		t.Errorf("node inspect = %+v, %v", n.Node, err)
	}
	if _, err := c.NodeInspect(ctx, "n9", client.NodeInspectOptions{}); err == nil {
		t.Error("inspect of a missing node succeeded")
	}
	if nets, err := c.NetworkList(ctx, client.NetworkListOptions{}); err != nil || len(nets.Items) != 1 {
		t.Errorf("networks = %+v, %v", nets.Items, err)
	}
	if secs, err := c.SecretList(ctx, client.SecretListOptions{Filters: make(client.Filters).Add("name", "db_")}); err != nil || len(secs.Items) != 1 {
		t.Errorf("secrets = %+v, %v", secs.Items, err)
	}
	if cfgs, err := c.ConfigList(ctx, client.ConfigListOptions{Filters: make(client.Filters).Add("name", "nope")}); err != nil || len(cfgs.Items) != 0 {
		t.Errorf("config name filter = %+v, %v", cfgs.Items, err)
	}
	if _, err := c.VolumeList(ctx, client.VolumeListOptions{}); err == nil {
		t.Error("an unserved path answered")
	}
	if len(f.Unknown) != 1 || !strings.Contains(f.Unknown[0], "/volumes") {
		t.Errorf("unknown = %v, want the volumes call recorded", f.Unknown)
	}
}

// The writes land in the lists the reads answer from, and the fake refuses
// what a manager refuses: a taken name, and an update from a stale version.
func TestSwarmTakesWrites(t *testing.T) {
	f := &Swarm{Fail: map[string]bool{"POST /networks/create": true}}
	api := httptest.NewServer(f)
	defer api.Close()
	c, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	sec, err := c.SecretCreate(ctx, client.SecretCreateOptions{Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "pw"}, Data: []byte("x")}})
	if err != nil || sec.ID != "secret-pw" {
		t.Fatalf("secret create = %+v, %v", sec, err)
	}
	if _, err := c.SecretCreate(ctx, client.SecretCreateOptions{Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "pw"}}}); err == nil {
		t.Error("a second secret of the same name was created")
	}
	if cfg, err := c.ConfigCreate(ctx, client.ConfigCreateOptions{Spec: swarm.ConfigSpec{Annotations: swarm.Annotations{Name: "pol"}}}); err != nil || cfg.ID != "config-pol" {
		t.Errorf("config create = %+v, %v", cfg, err)
	}
	if list, _ := c.ConfigList(ctx, client.ConfigListOptions{}); len(list.Items) != 1 {
		t.Errorf("created config not listed: %+v", list.Items)
	}

	spec := swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "web"}, TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "nginx:1"}}}
	svc, err := c.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: spec, EncodedRegistryAuth: "auth1"})
	if err != nil || svc.ID != "service-web" {
		t.Fatalf("service create = %+v, %v", svc, err)
	}
	got, err := c.ServiceInspect(ctx, "web", client.ServiceInspectOptions{})
	if err != nil || got.Service.Spec.TaskTemplate.ContainerSpec.Image != "nginx:1" || got.Service.Version.Index != 1 {
		t.Fatalf("created service = %+v, %v", got.Service, err)
	}

	spec.TaskTemplate.ContainerSpec.Image = "nginx:2"
	if _, err := c.ServiceUpdate(ctx, svc.ID, client.ServiceUpdateOptions{Version: swarm.Version{Index: 7}, Spec: spec}); err == nil {
		t.Error("an update from a stale version was taken")
	}
	if _, err := c.ServiceUpdate(ctx, svc.ID, client.ServiceUpdateOptions{Version: got.Service.Version, Spec: spec}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = c.ServiceInspect(ctx, "web", client.ServiceInspectOptions{})
	if got.Service.Spec.TaskTemplate.ContainerSpec.Image != "nginx:2" || got.Service.Version.Index != 2 {
		t.Errorf("updated service = %+v", got.Service)
	}
	if len(f.RegistryAuth) != 2 || f.RegistryAuth[0] != "auth1" || f.RegistryAuth[1] != "" {
		t.Errorf("registry auth = %q, want the create's header and none on the update", f.RegistryAuth)
	}

	if _, err := c.NetworkCreate(ctx, "n", client.NetworkCreateOptions{}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Errorf("an injected failure must fail the call: %v", err)
	}
}

func TestSelfSignedPair(t *testing.T) {
	pair := SelfSignedPair(t)
	if len(pair.Certificate) != 1 || pair.PrivateKey == nil {
		t.Errorf("pair = %d certs, key %v", len(pair.Certificate), pair.PrivateKey != nil)
	}
}
