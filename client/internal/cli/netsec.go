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

// netContainer is a container endpoint attached to a network.
type netContainer struct {
	Name string
	IPv4 string
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

// networkContainers returns the container endpoints attached to a network via a
// verbose inspect (the only call that reports cluster-wide attachments for an
// overlay). Best effort — an inspect error yields nil. The swarm load-balancer
// pseudo-endpoint is skipped: it is plumbing, not a container.
func networkContainers(ctx context.Context, dcli *client.Client, id string) []netContainer {
	n, err := dcli.NetworkInspect(ctx, id, network.InspectOptions{Verbose: true})
	if err != nil {
		return nil
	}
	var cs []netContainer
	for _, ep := range n.Containers {
		if ep.Name == "" || strings.HasPrefix(ep.Name, "lb-") {
			continue
		}
		cs = append(cs, netContainer{Name: ep.Name, IPv4: ep.IPv4Address})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs
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
