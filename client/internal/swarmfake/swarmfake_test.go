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
	named, err := c.ServiceList(ctx, client.ServiceListOptions{Filters: make(client.Filters).Add("name", "db")})
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
	if secs, err := c.SecretList(ctx, client.SecretListOptions{Filters: make(client.Filters).Add("name", "db")}); err != nil || len(secs.Items) != 1 {
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

func TestSelfSignedPair(t *testing.T) {
	pair := SelfSignedPair(t)
	if len(pair.Certificate) != 1 || pair.PrivateKey == nil {
		t.Errorf("pair = %d certs, key %v", len(pair.Certificate), pair.PrivateKey != nil)
	}
}
