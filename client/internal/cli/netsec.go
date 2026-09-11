// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
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
	Encrypted  bool   // overlay data-plane encryption (--opt encrypted)
	MTU        string // com.docker.network.driver.mtu, "" if unset
	Labels     map[string]string
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
// belong to — the shape the network detail view renders. Aliases are the
// service's custom DNS aliases on this network (collapsible in the view).
type netService struct {
	Name       string
	Containers []netContainer
	Aliases    []string
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
			Encrypted:  networkEncrypted(n.Options),
			MTU:        n.Options["com.docker.network.driver.mtu"],
			Labels:     n.Labels,
			Created:    n.Created,
			Services:   svcs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// networkEncrypted reports whether an overlay network has data-plane encryption
// on, read from its driver options. The docker CLI's `--opt encrypted` form
// stores an empty value (presence = on); `--opt encrypted=false` turns it off.
func networkEncrypted(opts map[string]string) bool {
	v, ok := opts["encrypted"]
	if !ok {
		return false
	}
	return v != "false" && v != "0"
}

// newNetworkOpts is the create-network form's collected input. Empty optional
// fields are omitted. Driver defaults to overlay (the swarm-scoped default).
type newNetworkOpts struct {
	Name       string
	Driver     string
	Attachable bool
	Encrypted  bool
	Internal   bool
	IPv6       bool
	MTU        string // optional; validated as an integer
	Subnet     string // optional CIDR, e.g. 10.10.0.0/24
	Gateway    string // optional; requires Subnet
	Labels     map[string]string
}

// buildNetworkCreateOptions maps the form input onto docker's CreateOptions,
// validating the numeric/relational constraints. Split out from createNetwork so
// the mapping is unit-testable without a daemon.
func buildNetworkCreateOptions(o newNetworkOpts) (string, network.CreateOptions, error) {
	name := strings.TrimSpace(o.Name)
	if name == "" {
		return "", network.CreateOptions{}, fmt.Errorf("network name is required")
	}
	driver := strings.TrimSpace(o.Driver)
	if driver == "" {
		driver = "overlay"
	}
	opts := network.CreateOptions{
		Driver:     driver,
		Attachable: o.Attachable,
		Internal:   o.Internal,
	}
	driverOpts := map[string]string{}
	if o.Encrypted {
		// Send an explicit "true": unambiguous vs the CLI's empty-value form.
		driverOpts["encrypted"] = "true"
	}
	if mtu := strings.TrimSpace(o.MTU); mtu != "" {
		if _, err := strconv.Atoi(mtu); err != nil {
			return "", network.CreateOptions{}, fmt.Errorf("MTU must be a number, got %q", o.MTU)
		}
		driverOpts["com.docker.network.driver.mtu"] = mtu
	}
	if len(driverOpts) > 0 {
		opts.Options = driverOpts
	}
	if o.IPv6 {
		v6 := true
		opts.EnableIPv6 = &v6
	}
	subnet, gateway := strings.TrimSpace(o.Subnet), strings.TrimSpace(o.Gateway)
	if gateway != "" && subnet == "" {
		return "", network.CreateOptions{}, fmt.Errorf("a gateway requires a subnet")
	}
	if subnet != "" {
		cfg := network.IPAMConfig{Subnet: subnet}
		if gateway != "" {
			cfg.Gateway = gateway
		}
		opts.IPAM = &network.IPAM{Config: []network.IPAMConfig{cfg}}
	}
	if len(o.Labels) > 0 {
		opts.Labels = o.Labels
	}
	return name, opts, nil
}

// createNetwork creates a network from the form input (default driver overlay).
func createNetwork(ctx context.Context, dcli *client.Client, o newNetworkOpts) error {
	name, opts, err := buildNetworkCreateOptions(o)
	if err != nil {
		return err
	}
	_, err = dcli.NetworkCreate(ctx, name, opts)
	return err
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

// detachServiceFromNetwork removes a network from a service's spec via a
// read-modify-write ServiceUpdate (rolling update). Errors (a no-op) if the
// service is not attached, or does not exist.
func detachServiceFromNetwork(ctx context.Context, dcli *client.Client, serviceName, networkID, networkName string) error {
	svc, err := serviceByName(ctx, dcli, serviceName)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", serviceName)
	}
	matches := func(target string) bool { return target == networkID || target == networkName }
	spec := svc.Spec
	tt, n1 := dropNetwork(spec.TaskTemplate.Networks, matches)
	sn, n2 := dropNetwork(spec.Networks, matches)
	if n1+n2 == 0 {
		return fmt.Errorf("service %q is not attached to network %q", serviceName, networkName)
	}
	spec.TaskTemplate.Networks = tt
	spec.Networks = sn
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// dropNetwork returns the attachments that do not match, plus how many it
// removed.
func dropNetwork(nets []swarm.NetworkAttachmentConfig, matches func(string) bool) ([]swarm.NetworkAttachmentConfig, int) {
	var out []swarm.NetworkAttachmentConfig
	removed := 0
	for _, a := range nets {
		if matches(a.Target) {
			removed++
			continue
		}
		out = append(out, a)
	}
	return out, removed
}

// detachSecretFromService removes a secret reference from a service's container
// spec via a read-modify-write ServiceUpdate (rolling update). Errors (a no-op)
// if the service does not use the secret, or does not exist.
func detachSecretFromService(ctx context.Context, dcli *client.Client, serviceName, secretID, secretName string) error {
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
	var kept []*swarm.SecretReference
	removed := false
	for _, ref := range cs.Secrets {
		if ref.SecretID == secretID || ref.SecretName == secretName {
			removed = true
			continue
		}
		kept = append(kept, ref)
	}
	if !removed {
		return fmt.Errorf("service %q does not use secret %q", serviceName, secretName)
	}
	spec := svc.Spec
	spec.TaskTemplate.ContainerSpec.Secrets = kept
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
	nodeName := nodeHostnames(ctx, dcli)

	// One ServiceList yields both id→name (to label each task's service) and each
	// service's custom DNS aliases on this network — no extra per-service inspect.
	idName := map[string]string{}
	aliasesBySvc := map[string][]string{}
	if svcs, err := dcli.ServiceList(ctx, types.ServiceListOptions{}); err == nil {
		for _, s := range svcs {
			idName[s.ID] = s.Spec.Name
			aliasesBySvc[s.Spec.Name] = serviceAliasesOnNetwork(s, net)
		}
	}

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
		out = append(out, netService{Name: name, Containers: cs, Aliases: aliasesBySvc[name]})
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
		if a.Network.ID == netID {
			return stripMask(firstIPv4(a.Addresses))
		}
	}
	return ""
}

// serviceAliasesOnNetwork returns a service's custom DNS aliases on one network,
// read straight from its spec — the attachment whose Target matches the network
// by id or name. Empty when the service sets no aliases there.
func serviceAliasesOnNetwork(s swarm.Service, net swarmNetwork) []string {
	match := func(a swarm.NetworkAttachmentConfig) bool {
		return a.Target == net.ID || a.Target == net.Name
	}
	for _, a := range s.Spec.TaskTemplate.Networks {
		if match(a) && len(a.Aliases) > 0 {
			return append([]string{}, a.Aliases...)
		}
	}
	for _, a := range s.Spec.Networks {
		if match(a) && len(a.Aliases) > 0 {
			return append([]string{}, a.Aliases...)
		}
	}
	return nil
}

// listSecrets returns secret metadata (never the value — the API does not
// expose it) with the services that use each secret resolved from service specs.
// createSecret creates a new swarm secret with the given value and labels.
// Docker secrets are write-only and immutable, so a name clash cannot be
// "updated" — it is reported as an error the caller can surface. The name check
// is best-effort (racy), but Docker also rejects a duplicate name server-side,
// so correctness does not depend on it.
func createSecret(ctx context.Context, dcli *client.Client, name string, data []byte, labels map[string]string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(data) == 0 {
		return fmt.Errorf("value is required")
	}
	list, err := dcli.SecretList(ctx, types.SecretListOptions{})
	if err != nil {
		return err
	}
	for _, s := range list {
		if s.Spec.Name == name {
			return fmt.Errorf("secret %q already exists (secrets are immutable — remove it first to replace)", name)
		}
	}
	_, err = dcli.SecretCreate(ctx, swarm.SecretSpec{
		Annotations: swarm.Annotations{Name: name, Labels: labels},
		Data:        data,
	})
	return err
}

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

// secretRef identifies a secret by its id and name.
type secretRef struct{ ID, Name string }

// orphanSecrets returns the secrets referenced by target that NO other service
// in all references — i.e. the ones that become unused if target is removed.
// Pure, so it is unit-testable.
func orphanSecrets(target swarm.Service, all []swarm.Service) []secretRef {
	cs := target.Spec.TaskTemplate.ContainerSpec
	if cs == nil || len(cs.Secrets) == 0 {
		return nil
	}
	// Secrets referenced by any OTHER service (keyed by both id and name, since
	// a reference may match on either).
	usedElsewhere := map[string]bool{}
	for _, s := range all {
		if s.ID == target.ID && target.ID != "" || s.Spec.Name == target.Spec.Name {
			continue
		}
		ocs := s.Spec.TaskTemplate.ContainerSpec
		if ocs == nil {
			continue
		}
		for _, r := range ocs.Secrets {
			usedElsewhere[r.SecretID] = true
			usedElsewhere[r.SecretName] = true
		}
	}
	seen := map[string]bool{}
	var out []secretRef
	for _, r := range cs.Secrets {
		key := r.SecretID + "\x00" + r.SecretName
		if seen[key] {
			continue
		}
		seen[key] = true
		if !usedElsewhere[r.SecretID] && !usedElsewhere[r.SecretName] {
			out = append(out, secretRef{ID: r.SecretID, Name: r.SecretName})
		}
	}
	return out
}

// secretsOnlyUsedBy returns the secrets referenced only by the named service —
// the ones orphaned if it is removed. Best-effort (empty on error/not found).
func secretsOnlyUsedBy(ctx context.Context, dcli *client.Client, name string) ([]secretRef, error) {
	svcs, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	for i := range svcs {
		if svcs[i].Spec.Name == name {
			return orphanSecrets(svcs[i], svcs), nil
		}
	}
	return nil, nil
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

// swarmConfig is a swarm config with the services that mount it. Unlike a
// secret, a config's content is readable — Size is its length, and the detail
// view can show the content itself.
type swarmConfig struct {
	ID       string
	Name     string
	Created  time.Time
	Updated  time.Time
	Size     int
	Labels   map[string]string
	Services []string // service names that mount the config
}

// listConfigs returns every config on the manager with service membership
// resolved from service specs — the same derivation listSecrets does, since
// swarm has no reverse index either way.
func listConfigs(ctx context.Context, dcli *client.Client) ([]swarmConfig, error) {
	cfgs, err := dcli.ConfigList(ctx, types.ConfigListOptions{})
	if err != nil {
		return nil, err
	}
	members := configServiceMembers(ctx, dcli)
	out := make([]swarmConfig, 0, len(cfgs))
	for _, c := range cfgs {
		// A reference may name the config by ID or name, so merge both buckets.
		set := map[string]bool{}
		var svcs []string
		for _, name := range append(members[c.ID], members[c.Spec.Name]...) {
			if !set[name] {
				set[name] = true
				svcs = append(svcs, name)
			}
		}
		sort.Strings(svcs)
		out = append(out, swarmConfig{
			ID:       c.ID,
			Name:     c.Spec.Name,
			Created:  c.Meta.CreatedAt,
			Updated:  c.Meta.UpdatedAt,
			Size:     len(c.Spec.Data),
			Labels:   c.Spec.Labels,
			Services: svcs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// configServiceMembers maps config id/name -> service names that mount it.
func configServiceMembers(ctx context.Context, dcli *client.Client) map[string][]string {
	svcs, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return map[string][]string{}
	}
	return serviceConfigMembership(svcs)
}

// serviceConfigMembership derives config -> services from service specs, keyed
// by both config id and name (a reference may match on either). Pure, so it is
// unit-testable without a daemon.
func serviceConfigMembership(svcs []swarm.Service) map[string][]string {
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
		for _, ref := range cs.Configs {
			if ref == nil {
				continue
			}
			add(ref.ConfigID)
			add(ref.ConfigName)
		}
	}
	return m
}

// configContent fetches a config's payload. Configs are readable (secrets are
// not), so the detail view can show what a service actually receives. Fetched on
// demand rather than with the list: a config can be a whole nginx.conf.
func configContent(ctx context.Context, dcli *client.Client, id string) ([]byte, error) {
	c, _, err := dcli.ConfigInspectWithRaw(ctx, id)
	if err != nil {
		return nil, err
	}
	return c.Spec.Data, nil
}
