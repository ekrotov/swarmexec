// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
)

// Networks and secrets are swarm-scoped (managed by the manager), unlike
// node-local volumes, so the UI reads them straight from the manager's Docker
// API — no per-node agent RPC. This file is the read-only data layer; modifying
// actions (remove/create) can hang off the same types later.

// swarmNetwork is a network plus the services attached to it. Service
// membership comes from service specs (one ServiceList call, cluster-wide);
// container membership is resolved lazily via networkContainers when the
// operator opens a network, because it needs a verbose inspect per network.
type swarmNetwork struct {
	ID         string
	Name       string
	Driver     string
	Scope      string
	Internal   bool
	Attachable bool
	Ingress    bool
	Created    time.Time
	Services   []string // service names attached, from service specs
}

// netContainer is a container (running task) attached to a network.
type netContainer struct {
	ID   string // short container id
	Node string // node hostname it runs on
	IPv4 string
}

// netService groups the containers attached to a network by the service they
// belong to — the shape the network detail view renders.
type netService struct {
	Name       string
	Containers []netContainer
}

// swarmSecret is a secret's metadata. Secret *values* are never retrievable via
// the Docker API, so only metadata is surfaced.
type swarmSecret struct {
	ID      string
	Name    string
	Created time.Time
	Updated time.Time
	Labels  map[string]string
}

// listNetworks returns every network on the manager with attached-service
// membership resolved from service specs.
func listNetworks(ctx context.Context, dcli *client.Client) ([]swarmNetwork, error) {
	nets, err := dcli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, err
	}
	members := networkServiceMembers(ctx, dcli)
	out := make([]swarmNetwork, 0, len(nets))
	for _, n := range nets {
		// A service references a network by either ID or name (Target), so pull
		// from both buckets and de-duplicate.
		set := map[string]bool{}
		var svcs []string
		for _, s := range append(members[n.ID], members[n.Name]...) {
			if !set[s] {
				set[s] = true
				svcs = append(svcs, s)
			}
		}
		sort.Strings(svcs)
		out = append(out, swarmNetwork{
			ID:         n.ID,
			Name:       n.Name,
			Driver:     n.Driver,
			Scope:      n.Scope,
			Internal:   n.Internal,
			Attachable: n.Attachable,
			Ingress:    n.Ingress,
			Created:    n.Created,
			Services:   svcs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// networkServiceMembers maps a network — keyed by both ID and name, since a
// service's attachment Target may be either — to the services attached to it.
// Best effort: a ServiceList failure yields an empty map so the network list
// still renders (just without membership).
func networkServiceMembers(ctx context.Context, dcli *client.Client) map[string][]string {
	svcs, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return map[string][]string{}
	}
	return serviceNetworkMembership(svcs)
}

// serviceNetworkMembership is the pure core of networkServiceMembers: it maps a
// network Target (ID or name) to the services attached to it.
func serviceNetworkMembership(svcs []swarm.Service) map[string][]string {
	m := map[string][]string{}
	for _, s := range svcs {
		seen := map[string]bool{}
		add := func(target string) {
			if target == "" || seen[target] {
				return
			}
			seen[target] = true
			m[target] = append(m[target], s.Spec.Name)
		}
		// TaskTemplate.Networks is current; Spec.Networks is the deprecated
		// pre-v1.44 location — read both so older services still show up.
		for _, a := range s.Spec.TaskTemplate.Networks {
			add(a.Target)
		}
		for _, a := range s.Spec.Networks {
			add(a.Target)
		}
	}
	return m
}

// networkMembers returns the services attached to a network, each carrying the
// running containers (swarm tasks) attached to that network. Grouping is driven
// by tasks — the reliable source that ties a container to both its service and
// its networks. A service with a spec attachment but no running task still
// appears (with zero containers). Best effort: failed API calls degrade to
// empty rather than failing the view.
func networkMembers(ctx context.Context, dcli *client.Client, net swarmNetwork) []netService {
	idName := serviceIDNames(ctx, dcli)
	nodeName := nodeHostnames(ctx, dcli)

	byService := map[string][]netContainer{}
	if tasks, err := dcli.TaskList(ctx, types.TaskListOptions{}); err == nil {
		for _, t := range tasks {
			if t.Status.State != swarm.TaskStateRunning || !taskOnNetwork(t, net.ID) {
				continue
			}
			name := idName[t.ServiceID]
			if name == "" {
				name = shortID(t.ServiceID)
			}
			byService[name] = append(byService[name], netContainer{
				ID:   shortID(t.Status.ContainerStatus.ContainerID),
				Node: nodeName[t.NodeID],
				IPv4: taskIPv4(t, net.ID),
			})
		}
	}

	// Union the services known from specs (may have zero running tasks) with the
	// services actually running tasks on the network.
	seen := map[string]bool{}
	var out []netService
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		cs := byService[name]
		sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
		out = append(out, netService{Name: name, Containers: cs})
	}
	for _, s := range net.Services {
		add(s)
	}
	taskOnly := make([]string, 0, len(byService))
	for s := range byService {
		taskOnly = append(taskOnly, s)
	}
	sort.Strings(taskOnly)
	for _, s := range taskOnly {
		add(s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// serviceIDNames maps service ID to name (for turning a task's ServiceID into a
// readable service name).
func serviceIDNames(ctx context.Context, dcli *client.Client) map[string]string {
	m := map[string]string{}
	svcs, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return m
	}
	for _, s := range svcs {
		m[s.ID] = s.Spec.Name
	}
	return m
}

// nodeHostnames maps node ID to hostname, so a task's NodeID shows as a name.
func nodeHostnames(ctx context.Context, dcli *client.Client) map[string]string {
	m := map[string]string{}
	nodes, err := dcli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return m
	}
	for _, n := range nodes {
		m[n.ID] = n.Description.Hostname
	}
	return m
}

// taskOnNetwork reports whether a task is attached to the given network. Task
// attachments carry the resolved network ID, so an ID match is reliable.
func taskOnNetwork(t swarm.Task, netID string) bool {
	for _, a := range t.NetworksAttachments {
		if a.Network.ID == netID {
			return true
		}
	}
	return false
}

// taskIPv4 returns the task's IPv4 address on the network, or "". Addresses are
// CIDR (e.g. "10.0.1.5/24"), so the mask is stripped.
func taskIPv4(t swarm.Task, netID string) string {
	for _, a := range t.NetworksAttachments {
		if a.Network.ID != netID {
			continue
		}
		for _, addr := range a.Addresses {
			ip := addr
			if i := strings.IndexByte(addr, '/'); i >= 0 {
				ip = addr[:i]
			}
			if strings.Contains(ip, ".") {
				return ip
			}
		}
	}
	return ""
}

// listSecrets returns secret metadata (never the value — the API does not
// expose it).
func listSecrets(ctx context.Context, dcli *client.Client) ([]swarmSecret, error) {
	secs, err := dcli.SecretList(ctx, types.SecretListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]swarmSecret, 0, len(secs))
	for _, s := range secs {
		out = append(out, swarmSecret{
			ID:      s.ID,
			Name:    s.Spec.Name,
			Created: s.Meta.CreatedAt,
			Updated: s.Meta.UpdatedAt,
			Labels:  s.Spec.Labels,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
