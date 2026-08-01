// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
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

// swarmSecret is a secret's metadata plus the services that use it. Secret
// *values* are never retrievable via the Docker API, so only metadata is
// surfaced.
type swarmSecret struct {
	ID       string
	Name     string
	Created  time.Time
	Updated  time.Time
	Labels   map[string]string
	Services []string // service names that reference the secret
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

// servicesExcluding returns the names in all that are not in exclude, sorted —
// the eligible attach targets (every service not already attached).
func servicesExcluding(all, exclude []string) []string {
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[e] = true
	}
	out := make([]string, 0, len(all))
	for _, a := range all {
		if !skip[a] {
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// attachServiceToNetwork adds a network to a service's spec via a
// read-modify-write ServiceUpdate. This triggers a rolling update of the
// service. It errors (a no-op) if the service is already attached, or if the
// service does not exist.
func attachServiceToNetwork(ctx context.Context, dcli *client.Client, serviceName, networkID, networkName string) error {
	svc, err := serviceByName(ctx, dcli, serviceName)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", serviceName)
	}
	for _, a := range svc.Spec.TaskTemplate.Networks {
		if a.Target == networkID || a.Target == networkName {
			return fmt.Errorf("service %q is already attached to network %q", serviceName, networkName)
		}
	}
	for _, a := range svc.Spec.Networks {
		if a.Target == networkID || a.Target == networkName {
			return fmt.Errorf("service %q is already attached to network %q", serviceName, networkName)
		}
	}
	spec := svc.Spec
	// TaskTemplate.Networks is the current location (Spec.Networks is deprecated).
	spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: networkID})
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// attachSecretToService adds a secret reference to a service's container spec
// (read-modify-write ServiceUpdate), mounted at /run/secrets/<name> like
// `docker service update --secret-add`. Triggers a rolling update. Errors (a
// no-op) if the service already uses the secret, or does not exist.
func attachSecretToService(ctx context.Context, dcli *client.Client, serviceName, secretID, secretName string) error {
	svc, err := serviceByName(ctx, dcli, serviceName)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", serviceName)
	}
	cs := svc.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return fmt.Errorf("service %q has no container spec", serviceName)
	}
	for _, ref := range cs.Secrets {
		if ref.SecretID == secretID || ref.SecretName == secretName {
			return fmt.Errorf("service %q already uses secret %q", serviceName, secretName)
		}
	}
	spec := svc.Spec
	spec.TaskTemplate.ContainerSpec.Secrets = append(spec.TaskTemplate.ContainerSpec.Secrets, &swarm.SecretReference{
		SecretID:   secretID,
		SecretName: secretName,
		File: &swarm.SecretReferenceFileTarget{
			Name: secretName,
			UID:  "0",
			GID:  "0",
			Mode: 0o444,
		},
	})
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
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
// expose it) with the services that use each secret resolved from service specs.
func listSecrets(ctx context.Context, dcli *client.Client) ([]swarmSecret, error) {
	secs, err := dcli.SecretList(ctx, types.SecretListOptions{})
	if err != nil {
		return nil, err
	}
	members := secretServiceMembers(ctx, dcli)
	out := make([]swarmSecret, 0, len(secs))
	for _, s := range secs {
		// A reference may name the secret by ID or name, so merge both buckets.
		set := map[string]bool{}
		var svcs []string
		for _, name := range append(members[s.ID], members[s.Spec.Name]...) {
			if !set[name] {
				set[name] = true
				svcs = append(svcs, name)
			}
		}
		sort.Strings(svcs)
		out = append(out, swarmSecret{
			ID:       s.ID,
			Name:     s.Spec.Name,
			Created:  s.Meta.CreatedAt,
			Updated:  s.Meta.UpdatedAt,
			Labels:   s.Spec.Labels,
			Services: svcs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// secretServiceMembers maps a secret — keyed by both ID and name — to the
// services that reference it. Best effort: a ServiceList failure yields an empty
// map so the secret list still renders.
func secretServiceMembers(ctx context.Context, dcli *client.Client) map[string][]string {
	svcs, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return map[string][]string{}
	}
	return serviceSecretMembership(svcs)
}

// serviceSecretMembership is the pure core of secretServiceMembers.
func serviceSecretMembership(svcs []swarm.Service) map[string][]string {
	m := map[string][]string{}
	for _, s := range svcs {
		cs := s.Spec.TaskTemplate.ContainerSpec
		if cs == nil {
			continue
		}
		seen := map[string]bool{}
		add := func(key string) {
			if key == "" || seen[key] {
				return
			}
			seen[key] = true
			m[key] = append(m[key], s.Spec.Name)
		}
		for _, ref := range cs.Secrets {
			if ref == nil {
				continue
			}
			add(ref.SecretID)
			add(ref.SecretName)
		}
	}
	return m
}

// secretMembers returns the services that use a secret, each carrying the
// running containers (swarm tasks) of that service. Structure mirrors
// networkMembers so the detail views render the same way. Best effort: failed
// API calls degrade to empty.
func secretMembers(ctx context.Context, dcli *client.Client, secret swarmSecret) []netService {
	usingIDs := map[string]bool{}
	usingNames := map[string]bool{}
	idName := map[string]string{}
	if svcs, err := dcli.ServiceList(ctx, types.ServiceListOptions{}); err == nil {
		for _, s := range svcs {
			idName[s.ID] = s.Spec.Name
			if serviceUsesSecret(s, secret.ID, secret.Name) {
				usingIDs[s.ID] = true
				usingNames[s.Spec.Name] = true
			}
		}
	}
	nodeName := nodeHostnames(ctx, dcli)

	byService := map[string][]netContainer{}
	if tasks, err := dcli.TaskList(ctx, types.TaskListOptions{}); err == nil {
		for _, t := range tasks {
			if t.Status.State != swarm.TaskStateRunning || !usingIDs[t.ServiceID] {
				continue
			}
			name := idName[t.ServiceID]
			if name == "" {
				name = shortID(t.ServiceID)
			}
			byService[name] = append(byService[name], netContainer{
				ID:   shortID(t.Status.ContainerStatus.ContainerID),
				Node: nodeName[t.NodeID],
			})
		}
	}

	names := make([]string, 0, len(usingNames))
	for n := range usingNames {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]netService, 0, len(names))
	for _, name := range names {
		cs := byService[name]
		sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
		out = append(out, netService{Name: name, Containers: cs})
	}
	return out
}

// serviceUsesSecret reports whether a service references the secret by ID or
// name in its container spec.
func serviceUsesSecret(s swarm.Service, id, name string) bool {
	cs := s.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return false
	}
	for _, ref := range cs.Secrets {
		if ref != nil && (ref.SecretID == id || ref.SecretName == name) {
			return true
		}
	}
	return false
}

// serviceVolumeNames returns the set of named volumes referenced by any service
// spec (its TaskTemplate mounts). Used to spare service-declared volumes from a
// prune even when no task currently runs. Best effort: a ServiceList failure
// yields an empty set (prune then falls back to the running-container view).
func serviceVolumeNames(ctx context.Context, dcli *client.Client) map[string]bool {
	svcs, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return map[string]bool{}
	}
	return serviceVolumeMounts(svcs)
}

// serviceVolumeMounts is the pure core of serviceVolumeNames.
func serviceVolumeMounts(svcs []swarm.Service) map[string]bool {
	m := map[string]bool{}
	for _, s := range svcs {
		cs := s.Spec.TaskTemplate.ContainerSpec
		if cs == nil {
			continue
		}
		for _, mt := range cs.Mounts {
			if mt.Type == mount.TypeVolume && mt.Source != "" {
				m[mt.Source] = true
			}
		}
	}
	return m
}
