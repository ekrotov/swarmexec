// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"context"
	"testing"

	"github.com/docker/cli/cli/compose/convert"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// deployedCluster is what a deploy leaves behind, read back through the
// manager API.
type deployedCluster struct {
	services []swarm.Service
	networks []network.Summary
	secrets  []swarm.Secret
	configs  []swarm.Config
}

func (d deployedCluster) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{Items: d.services}, nil
}
func (d deployedCluster) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	return client.NetworkListResult{Items: d.networks}, nil
}
func (d deployedCluster) SecretList(context.Context, client.SecretListOptions) (client.SecretListResult, error) {
	return client.SecretListResult{Items: d.secrets}, nil
}
func (d deployedCluster) ConfigList(context.Context, client.ConfigListOptions) (client.ConfigListResult, error) {
	return client.ConfigListResult{Items: d.configs}, nil
}

// deployWithConverter builds the deployed state the way a deploy does: with
// docker's own converter, which also writes the stack labels onto the
// services, networks, secrets, configs and volume mounts.
func deployWithConverter(t *testing.T, path, stack string) deployedCluster {
	t.Helper()
	cfg, err := loadComposeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ns := convert.NewNamespace(stack)
	var d deployedCluster
	secrets, err := convert.Secrets(ns, cfg.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range secrets {
		d.secrets = append(d.secrets, swarm.Secret{ID: "sec" + string(rune('a'+i)), Spec: s})
	}
	configs, err := convert.Configs(ns, cfg.Configs)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range configs {
		d.configs = append(d.configs, swarm.Config{ID: "cfg" + string(rune('a'+i)), Spec: c})
	}
	used := map[string]struct{}{}
	for _, s := range cfg.Services {
		for n := range s.Networks {
			used[n] = struct{}{}
		}
	}
	create, _ := convert.Networks(ns, cfg.Networks, used)
	for name, o := range create {
		d.networks = append(d.networks, network.Summary{Network: network.Network{
			Name: name, ID: "net-" + name, Driver: o.Driver, Labels: o.Labels,
		}})
	}
	specs, err := convert.Services(context.Background(), ns, cfg, d.withSecrets())
	if err != nil {
		t.Fatal(err)
	}
	for short, spec := range specs {
		d.services = append(d.services, swarm.Service{ID: "svc-" + short, Spec: spec})
	}
	return d
}

// withSecrets lets the converter resolve references against the objects the
// deploy has just created.
func (d deployedCluster) withSecrets() client.APIClient {
	return emptyCluster{secrets: d.secrets, configs: d.configs}
}

const ownedObjectsStack = `version: "3.8"
services:
  web:
    image: nginx:alpine
    volumes:
      - data:/usr/share/nginx/html
    networks: [front]
    secrets: [pass]
    configs:
      - source: greeting
        target: /etc/greeting.txt
networks:
  front:
    driver: overlay
volumes:
  data:
secrets:
  pass:
    file: ./pass.txt
configs:
  greeting:
    file: ./greeting.txt
`

// The question stack diff answers in a pipeline is "would deploying this file
// change anything?". Asked right after deploying that very file, the answer
// must be no — it used to report the stack's own volume, secret and config as
// renamed or external.
func TestFileDiffedAgainstItsOwnDeployIsEmpty(t *testing.T) {
	path := writeStack(t, ownedObjectsStack)
	cluster := deployWithConverter(t, path, "shop")

	deployed, err := FromSwarm(context.Background(), cluster, "shop")
	if err != nil {
		t.Fatal(err)
	}
	file, err := FromFile(context.Background(), cluster.withSecrets(), path, "shop")
	if err != nil {
		t.Fatal(err)
	}
	d, err := Compare(deployed, file, "stack.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Empty() {
		t.Errorf("a file differs from its own deploy:\n%s", d.Text())
	}
	// And an export keeps the real names, so deploying it again mounts the
	// same volume and references the same secret rather than new ones.
	if v := deployed.Volumes["data"]; v == nil || v.Name != "shop_data" {
		t.Errorf("volume exported as %+v, want name shop_data", v)
	}
	if s := deployed.Secrets["pass"]; s == nil || s.Name != "shop_pass" {
		t.Errorf("secret exported as %+v, want name shop_pass", s)
	}
}
