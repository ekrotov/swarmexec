// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	units "github.com/docker/go-units"
)

// Service edits (scale, ports, labels) are manager-API ServiceUpdate operations
// — read the current spec, mutate it, update. Each triggers a rolling update /
// reconciliation of the service's tasks.

// scaleService sets the replica count of a replicated service.
func scaleService(ctx context.Context, dcli *client.Client, name string, replicas uint64) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	if svc.Spec.Mode.Replicated == nil {
		return fmt.Errorf("service %q is not replicated and cannot be scaled", name)
	}
	spec := svc.Spec
	r := replicas
	spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &r}
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// setServicePorts replaces a service's published ports.
func setServicePorts(ctx context.Context, dcli *client.Client, name string, ports []swarm.PortConfig) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	spec := svc.Spec
	if spec.EndpointSpec == nil {
		spec.EndpointSpec = &swarm.EndpointSpec{}
	}
	spec.EndpointSpec.Ports = ports
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// setServiceLabels replaces a service's labels.
func setServiceLabels(ctx context.Context, dcli *client.Client, name string, labels map[string]string) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	spec := svc.Spec
	spec.Labels = labels
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// currentServicePorts returns a service's published ports as editable strings.
func currentServicePorts(ctx context.Context, dcli *client.Client, name string) ([]string, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %q", name)
	}
	var out []string
	if svc.Spec.EndpointSpec != nil {
		for _, p := range svc.Spec.EndpointSpec.Ports {
			out = append(out, formatServicePort(p))
		}
	}
	return out, nil
}

// currentServiceLabels returns a service's labels as editable "key=value" strings.
func currentServiceLabels(ctx context.Context, dcli *client.Client, name string) ([]string, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %q", name)
	}
	return kvPairs(svc.Spec.Labels), nil
}

// currentServiceReplicas returns the replica count and whether the service is
// replicated (and therefore scalable).
func currentServiceReplicas(ctx context.Context, dcli *client.Client, name string) (replicas uint64, replicated bool, err error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return 0, false, err
	}
	if svc == nil {
		return 0, false, fmt.Errorf("no service named %q", name)
	}
	r := svc.Spec.Mode.Replicated
	if r == nil {
		return 0, false, nil
	}
	if r.Replicas != nil {
		replicas = *r.Replicas
	}
	return replicas, true, nil
}

// setServiceNetworks replaces the networks a service is attached to. targetIDs
// are network IDs (or names the daemon can resolve). It writes the current
// TaskTemplate.Networks and clears the deprecated Spec.Networks so it cannot
// override.
func setServiceNetworks(ctx context.Context, dcli *client.Client, name string, targetIDs []string) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	spec := svc.Spec
	var nets []swarm.NetworkAttachmentConfig
	for _, id := range targetIDs {
		nets = append(nets, swarm.NetworkAttachmentConfig{Target: id})
	}
	spec.TaskTemplate.Networks = nets
	spec.Networks = nil
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// listNetworkRefs returns name->ID and ID->name maps plus the sorted network
// names, for the networks editor (autocomplete + name/ID resolution).
func listNetworkRefs(ctx context.Context, dcli *client.Client) (idByName, idToName map[string]string, names []string) {
	idByName, idToName = map[string]string{}, map[string]string{}
	nets, err := dcli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return idByName, idToName, names
	}
	for _, n := range nets {
		idByName[n.Name] = n.ID
		idToName[n.ID] = n.Name
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return idByName, idToName, names
}

// attachedNet is one of a service's network attachments, with the network's
// resolved name and its custom aliases — for the alias editor.
type attachedNet struct {
	Target  string
	Name    string
	Aliases []string
}

// serviceAttachedNetworks lists a service's network attachments with their
// current aliases.
func serviceAttachedNetworks(ctx context.Context, dcli *client.Client, name string) ([]attachedNet, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %q", name)
	}
	_, idToName, _ := listNetworkRefs(ctx, dcli)
	var out []attachedNet
	seen := map[string]bool{}
	add := func(a swarm.NetworkAttachmentConfig) {
		if a.Target == "" || seen[a.Target] {
			return
		}
		seen[a.Target] = true
		n := idToName[a.Target]
		if n == "" {
			n = a.Target
		}
		out = append(out, attachedNet{Target: a.Target, Name: n, Aliases: append([]string{}, a.Aliases...)})
	}
	for _, a := range svc.Spec.TaskTemplate.Networks {
		add(a)
	}
	for _, a := range svc.Spec.Networks {
		add(a)
	}
	return out, nil
}

// setNetworkAliases replaces the aliases of a service's attachment to one
// network (read-modify-write ServiceUpdate, rolling update).
func setNetworkAliases(ctx context.Context, dcli *client.Client, name, target string, aliases []string) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	spec := svc.Spec
	found := false
	for i := range spec.TaskTemplate.Networks {
		if spec.TaskTemplate.Networks[i].Target == target {
			spec.TaskTemplate.Networks[i].Aliases = aliases
			found = true
		}
	}
	for i := range spec.Networks {
		if spec.Networks[i].Target == target {
			spec.Networks[i].Aliases = aliases
			found = true
		}
	}
	if !found {
		return fmt.Errorf("service %q is not attached to network %q", name, target)
	}
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// currentServiceNetworks returns the network names a service is attached to.
func currentServiceNetworks(ctx context.Context, dcli *client.Client, name string, idToName map[string]string) ([]string, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %q", name)
	}
	seen := map[string]bool{}
	var out []string
	add := func(target string) {
		if target == "" || seen[target] {
			return
		}
		seen[target] = true
		n := idToName[target]
		if n == "" {
			n = target
		}
		out = append(out, n)
	}
	for _, a := range svc.Spec.TaskTemplate.Networks {
		add(a.Target)
	}
	for _, a := range svc.Spec.Networks {
		add(a.Target)
	}
	return out, nil
}

// setServiceEnv replaces a service's environment variables ("KEY=VALUE").
// Read-modify-write ServiceUpdate (rolling update).
func setServiceEnv(ctx context.Context, dcli *client.Client, name string, env []string) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	if svc.Spec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("service %q has no container spec", name)
	}
	spec := svc.Spec
	spec.TaskTemplate.ContainerSpec.Env = env
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// forceUpdateService redeploys a service without changing its spec — the
// equivalent of `docker service update --force`. Bumping TaskTemplate.ForceUpdate
// makes Swarm reconcile every task (restart/reschedule), which is how you unstick
// a service sitting in an incomplete state (e.g. 1/2 replicas). Read-modify-write
// ServiceUpdate; nothing else in the spec changes.
func forceUpdateService(ctx context.Context, dcli *client.Client, name string) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	spec := svc.Spec
	spec.TaskTemplate.ForceUpdate++
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// updateServiceImage sets a service's image (read-modify-write ServiceUpdate,
// rolling update). Used to move a :latest service onto the registry's current
// digest — the caller passes the fully-qualified ref (repo:latest@sha256:…).
func updateServiceImage(ctx context.Context, dcli *client.Client, name, image string) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	if svc.Spec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("service %q has no container spec", name)
	}
	spec := svc.Spec
	spec.TaskTemplate.ContainerSpec.Image = image
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// currentServiceEnv returns a service's environment variables as "KEY=VALUE".
func currentServiceEnv(ctx context.Context, dcli *client.Client, name string) ([]string, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %q", name)
	}
	var out []string
	if cs := svc.Spec.TaskTemplate.ContainerSpec; cs != nil {
		out = append(out, cs.Env...)
	}
	return out, nil
}

// parseEnv splits "KEY=VALUE"; the key is trimmed and required, the value is
// kept verbatim (env values may contain '=' and meaningful whitespace).
func parseEnv(s string) (key, value string, err error) {
	i := strings.Index(s, "=")
	if i <= 0 {
		return "", "", fmt.Errorf("environment variable must be KEY=VALUE")
	}
	key = strings.TrimSpace(s[:i])
	if key == "" {
		return "", "", fmt.Errorf("environment variable key must not be empty")
	}
	return key, s[i+1:], nil
}

// envFromStrings validates edited "KEY=VALUE" entries and rejects a duplicate key.
func envFromStrings(items []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, it := range items {
		k, v, err := parseEnv(it)
		if err != nil {
			return nil, err
		}
		if seen[k] {
			return nil, fmt.Errorf("duplicate environment variable %q", k)
		}
		seen[k] = true
		out = append(out, k+"="+v)
	}
	return out, nil
}

// setServiceSecrets replaces the secrets a service references. secretNames are
// resolved to their IDs; each is mounted at /run/secrets/<name>. Read-modify-
// write ServiceUpdate (rolling update).
func setServiceSecrets(ctx context.Context, dcli *client.Client, name string, secretNames []string) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	cs := svc.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return fmt.Errorf("service %q has no container spec", name)
	}
	idByName := map[string]string{}
	if secs, e := dcli.SecretList(ctx, types.SecretListOptions{}); e == nil {
		for _, s := range secs {
			idByName[s.Spec.Name] = s.ID
		}
	}
	var refs []*swarm.SecretReference
	for _, sn := range secretNames {
		id := idByName[sn]
		if id == "" {
			return fmt.Errorf("unknown secret %q", sn)
		}
		refs = append(refs, &swarm.SecretReference{
			SecretID:   id,
			SecretName: sn,
			File:       &swarm.SecretReferenceFileTarget{Name: sn, UID: "0", GID: "0", Mode: 0o444},
		})
	}
	spec := svc.Spec
	spec.TaskTemplate.ContainerSpec.Secrets = refs
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// currentServiceSecrets returns the secret names a service references.
func currentServiceSecrets(ctx context.Context, dcli *client.Client, name string) ([]string, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %q", name)
	}
	var out []string
	if cs := svc.Spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, r := range cs.Secrets {
			out = append(out, r.SecretName)
		}
	}
	return out, nil
}

// secretNames lists all secret names in the swarm, for the secrets editor's
// autocomplete. Best effort.
func secretNames(ctx context.Context, dcli *client.Client) []string {
	var out []string
	if secs, e := dcli.SecretList(ctx, types.SecretListOptions{}); e == nil {
		for _, s := range secs {
			out = append(out, s.Spec.Name)
		}
	}
	sort.Strings(out)
	return out
}

// setServiceMounts replaces a service's mounts (volumes/binds). Read-modify-
// write ServiceUpdate (rolling update).
func setServiceMounts(ctx context.Context, dcli *client.Client, name string, mounts []mount.Mount) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	if svc.Spec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("service %q has no container spec", name)
	}
	spec := svc.Spec
	spec.TaskTemplate.ContainerSpec.Mounts = mounts
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// addServiceMount appends a single mount to a service (read-modify-write
// ServiceUpdate). Errors if a mount already exists at the same target.
func addServiceMount(ctx context.Context, dcli *client.Client, name string, m mount.Mount) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	if svc.Spec.TaskTemplate.ContainerSpec == nil {
		return fmt.Errorf("service %q has no container spec", name)
	}
	for _, ex := range svc.Spec.TaskTemplate.ContainerSpec.Mounts {
		if ex.Target == m.Target {
			return fmt.Errorf("service %q already has a mount at %q", name, m.Target)
		}
	}
	spec := svc.Spec
	spec.TaskTemplate.ContainerSpec.Mounts = append(spec.TaskTemplate.ContainerSpec.Mounts, m)
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// currentServiceMountSpecs returns a service's mounts as editable strings.
func currentServiceMountSpecs(ctx context.Context, dcli *client.Client, name string) ([]string, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %q", name)
	}
	var out []string
	if cs := svc.Spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, m := range cs.Mounts {
			out = append(out, formatServiceMount(m))
		}
	}
	return out, nil
}

// formatServiceMount renders a mount as "type:source:target[:ro]".
func formatServiceMount(m mount.Mount) string {
	s := fmt.Sprintf("%s:%s:%s", m.Type, m.Source, m.Target)
	if m.ReadOnly {
		s += ":ro"
	}
	return s
}

// parseServiceMount parses "TYPE:SOURCE:TARGET[:ro]" (TYPE volume|bind) into a
// Mount. Bind sources must be absolute host paths; targets must be absolute.
func parseServiceMount(s string) (mount.Mount, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	ro := false
	if len(parts) > 0 && parts[len(parts)-1] == "ro" {
		ro = true
		parts = parts[:len(parts)-1]
	}
	if len(parts) != 3 {
		return mount.Mount{}, fmt.Errorf("format: volume:NAME:TARGET[:ro] or bind:/host/path:TARGET[:ro]")
	}
	typ, src, tgt := parts[0], parts[1], parts[2]
	if src == "" || tgt == "" {
		return mount.Mount{}, fmt.Errorf("source and target are required")
	}
	if !strings.HasPrefix(tgt, "/") {
		return mount.Mount{}, fmt.Errorf("target %q must be an absolute path", tgt)
	}
	var mt mount.Type
	switch typ {
	case "volume":
		mt = mount.TypeVolume
	case "bind":
		mt = mount.TypeBind
		if !strings.HasPrefix(src, "/") {
			return mount.Mount{}, fmt.Errorf("bind source %q must be an absolute host path", src)
		}
	default:
		return mount.Mount{}, fmt.Errorf("mount type must be 'volume' or 'bind' (got %q)", typ)
	}
	return mount.Mount{Type: mt, Source: src, Target: tgt, ReadOnly: ro}, nil
}

// mountsFromStrings converts edited strings back to mounts, rejecting a
// duplicate target.
func mountsFromStrings(items []string) ([]mount.Mount, error) {
	var out []mount.Mount
	seen := map[string]bool{}
	for _, it := range items {
		m, err := parseServiceMount(it)
		if err != nil {
			return nil, err
		}
		if seen[m.Target] {
			return nil, fmt.Errorf("duplicate mount target %q", m.Target)
		}
		seen[m.Target] = true
		out = append(out, m)
	}
	return out, nil
}

// candidateNodesForService computes, client-side, the hostnames of nodes a
// service could be scheduled on (ready + active, satisfying every placement
// constraint we can evaluate). It also returns any constraints it could not
// evaluate, so the caller can qualify the result. Used for the bind-mount guard
// warning — swarmexec cannot check host paths, so it at least tells the operator
// which nodes must carry the path.
func candidateNodesForService(ctx context.Context, dcli *client.Client, name string) (nodes []string, unevaluated []string, err error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, nil, err
	}
	if svc == nil {
		return nil, nil, fmt.Errorf("no service named %q", name)
	}
	var constraints []string
	if p := svc.Spec.TaskTemplate.Placement; p != nil {
		constraints = p.Constraints
	}
	nl, err := dcli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, nil, err
	}
	unevalSet := map[string]bool{}
	for _, n := range nl {
		if n.Status.State != swarm.NodeStateReady || n.Spec.Availability != swarm.NodeAvailabilityActive {
			continue
		}
		ok := true
		for _, c := range constraints {
			matches, known := nodeMatchesConstraint(n, c)
			if !known {
				unevalSet[c] = true
				continue
			}
			if !matches {
				ok = false
				break
			}
		}
		if ok {
			nodes = append(nodes, n.Description.Hostname)
		}
	}
	sort.Strings(nodes)
	for c := range unevalSet {
		unevaluated = append(unevaluated, c)
	}
	sort.Strings(unevaluated)
	return nodes, unevaluated, nil
}

// nodeMatchesConstraint evaluates a single Swarm placement constraint against a
// node. known is false for constraint keys it doesn't understand (caller treats
// those conservatively). Supports == and != on node.role / node.hostname /
// node.id / node.platform.os|arch / node.labels.* / engine.labels.*.
func nodeMatchesConstraint(n swarm.Node, c string) (matches, known bool) {
	c = strings.TrimSpace(c)
	var key, val string
	neg := false
	if i := strings.Index(c, "!="); i >= 0 {
		key, val, neg = strings.TrimSpace(c[:i]), strings.TrimSpace(c[i+2:]), true
	} else if i := strings.Index(c, "=="); i >= 0 {
		key, val = strings.TrimSpace(c[:i]), strings.TrimSpace(c[i+2:])
	} else {
		return false, false
	}
	var actual string
	switch {
	case key == "node.role":
		actual = string(n.Spec.Role)
	case key == "node.hostname":
		actual = n.Description.Hostname
	case key == "node.id":
		actual = n.ID
	case key == "node.platform.os":
		actual = n.Description.Platform.OS
	case key == "node.platform.arch":
		actual = n.Description.Platform.Architecture
	case strings.HasPrefix(key, "node.labels."):
		actual = n.Spec.Labels[strings.TrimPrefix(key, "node.labels.")]
	case strings.HasPrefix(key, "engine.labels."):
		actual = n.Description.Engine.Labels[strings.TrimPrefix(key, "engine.labels.")]
	default:
		return false, false
	}
	eq := actual == val
	if neg {
		return !eq, true
	}
	return eq, true
}

// currentServicePlacement returns the service's placement constraints (the
// `node.*` / `engine.*` == / != expressions).
func currentServicePlacement(ctx context.Context, dcli *client.Client, name string) ([]string, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %q", name)
	}
	if p := svc.Spec.TaskTemplate.Placement; p != nil {
		return append([]string{}, p.Constraints...), nil
	}
	return nil, nil
}

// setServicePlacement replaces the service's placement constraints in one
// ServiceUpdate (read-modify-write; preferences and other placement fields are
// preserved).
func setServicePlacement(ctx context.Context, dcli *client.Client, name string, constraints []string) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	spec := svc.Spec
	if spec.TaskTemplate.Placement == nil {
		spec.TaskTemplate.Placement = &swarm.Placement{}
	}
	spec.TaskTemplate.Placement.Constraints = constraints
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// validatePlacementConstraint checks a single constraint and returns it in the
// canonical spaceless "key==value" / "key!=value" form. Keys are limited to the
// ones Swarm understands (and that nodeMatchesConstraint can evaluate).
func validatePlacementConstraint(s string) (string, error) {
	s = strings.TrimSpace(s)
	op, at := "", -1
	if i := strings.Index(s, "=="); i >= 0 {
		op, at = "==", i
	}
	if i := strings.Index(s, "!="); i >= 0 && (at < 0 || i < at) {
		op, at = "!=", i
	}
	if at < 0 {
		return "", fmt.Errorf("constraint must contain == or !=, e.g. node.labels.zone==eu")
	}
	key := strings.TrimSpace(s[:at])
	val := strings.TrimSpace(s[at+2:])
	known := key == "node.role" || key == "node.hostname" || key == "node.id" ||
		key == "node.platform.os" || key == "node.platform.arch" ||
		(strings.HasPrefix(key, "node.labels.") && len(key) > len("node.labels.")) ||
		(strings.HasPrefix(key, "engine.labels.") && len(key) > len("engine.labels."))
	if !known {
		return "", fmt.Errorf("unknown constraint key %q (use node.role / node.hostname / node.id / node.platform.os|arch / node.labels.<k> / engine.labels.<k>)", key)
	}
	if val == "" {
		return "", fmt.Errorf("constraint value must not be empty")
	}
	if strings.ContainsAny(val, " \t") {
		return "", fmt.Errorf("constraint value %q must not contain spaces", val)
	}
	return key + op + val, nil
}

// placementSuggestions builds fully-formed candidate constraints from the
// current cluster (node roles, hostnames, platforms and labels) for the
// placement editor's autocomplete. The operator offered is ==; the user can
// still type an != constraint by hand.
func placementSuggestions(ctx context.Context, dcli *client.Client) ([]string, error) {
	nl, err := dcli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	set := map[string]bool{"node.role==manager": true, "node.role==worker": true}
	for _, n := range nl {
		if h := n.Description.Hostname; h != "" {
			set["node.hostname=="+h] = true
		}
		if os := n.Description.Platform.OS; os != "" {
			set["node.platform.os=="+os] = true
		}
		if a := n.Description.Platform.Architecture; a != "" {
			set["node.platform.arch=="+a] = true
		}
		for k, v := range n.Spec.Labels {
			set["node.labels."+k+"=="+v] = true
		}
		for k, v := range n.Description.Engine.Labels {
			set["engine.labels."+k+"=="+v] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// currentServiceSpread returns the descriptors of the service's spread
// placement preferences (Placement.Preferences[].Spread.SpreadDescriptor).
func currentServiceSpread(ctx context.Context, dcli *client.Client, name string) ([]string, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, fmt.Errorf("no service named %q", name)
	}
	var out []string
	if p := svc.Spec.TaskTemplate.Placement; p != nil {
		for _, pref := range p.Preferences {
			if pref.Spread != nil {
				out = append(out, pref.Spread.SpreadDescriptor)
			}
		}
	}
	return out, nil
}

// setServiceSpread replaces the service's spread placement preferences (in the
// given order) in one ServiceUpdate. Constraints and other placement fields are
// preserved.
func setServiceSpread(ctx context.Context, dcli *client.Client, name string, descriptors []string) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	spec := svc.Spec
	if spec.TaskTemplate.Placement == nil {
		spec.TaskTemplate.Placement = &swarm.Placement{}
	}
	prefs := make([]swarm.PlacementPreference, 0, len(descriptors))
	for _, d := range descriptors {
		prefs = append(prefs, swarm.PlacementPreference{Spread: &swarm.SpreadOver{SpreadDescriptor: d}})
	}
	spec.TaskTemplate.Placement.Preferences = prefs
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// validateSpreadDescriptor checks a spread descriptor — a bare node attribute
// (no operator/value) that Swarm spreads tasks evenly over.
func validateSpreadDescriptor(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("spread descriptor must not be empty, e.g. node.labels.zone")
	}
	if strings.ContainsAny(s, " \t=!") {
		return "", fmt.Errorf("spread descriptor %q is just a node attribute — no operator or value", s)
	}
	known := s == "node.hostname" || s == "node.id" || s == "node.role" ||
		s == "node.platform.os" || s == "node.platform.arch" ||
		(strings.HasPrefix(s, "node.labels.") && len(s) > len("node.labels.")) ||
		(strings.HasPrefix(s, "engine.labels.") && len(s) > len("engine.labels."))
	if !known {
		return "", fmt.Errorf("unknown spread descriptor %q (use node.hostname / node.id / node.role / node.platform.os|arch / node.labels.<k> / engine.labels.<k>)", s)
	}
	return s, nil
}

// spreadSuggestions builds candidate spread descriptors (bare node attributes)
// from the current cluster for the spread editor's autocomplete.
func spreadSuggestions(ctx context.Context, dcli *client.Client) ([]string, error) {
	nl, err := dcli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	set := map[string]bool{
		"node.hostname": true, "node.role": true,
		"node.platform.os": true, "node.platform.arch": true,
	}
	for _, n := range nl {
		for k := range n.Spec.Labels {
			set["node.labels."+k] = true
		}
		for k := range n.Description.Engine.Labels {
			set["engine.labels."+k] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// serviceResources holds a service's CPU/memory limits and reservations as
// display strings (empty = unset). CPU is in cores (e.g. "0.5"); memory is a
// human size (e.g. "512MiB").
type serviceResources struct {
	CPULimit, MemLimit, CPUReservation, MemReservation string
}

// currentServiceResources returns the service's current limits/reservations as
// display strings for the resources editor.
func currentServiceResources(ctx context.Context, dcli *client.Client, name string) (serviceResources, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return serviceResources{}, err
	}
	if svc == nil {
		return serviceResources{}, fmt.Errorf("no service named %q", name)
	}
	var r serviceResources
	if res := svc.Spec.TaskTemplate.Resources; res != nil {
		if l := res.Limits; l != nil {
			r.CPULimit = formatCPUCores(l.NanoCPUs)
			r.MemLimit = formatMemBytes(l.MemoryBytes)
		}
		if rv := res.Reservations; rv != nil {
			r.CPUReservation = formatCPUCores(rv.NanoCPUs)
			r.MemReservation = formatMemBytes(rv.MemoryBytes)
		}
	}
	return r, nil
}

// setServiceResources sets the service's CPU/memory limits and reservations in
// one ServiceUpdate. A zero value clears that field (Swarm treats 0 as
// unlimited). Pids limits and generic (device) reservations are preserved.
func setServiceResources(ctx context.Context, dcli *client.Client, name string, cpuLimit, memLimit, cpuReservation, memReservation int64) error {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no service named %q", name)
	}
	spec := svc.Spec
	if spec.TaskTemplate.Resources == nil {
		spec.TaskTemplate.Resources = &swarm.ResourceRequirements{}
	}
	res := spec.TaskTemplate.Resources
	if res.Limits == nil {
		res.Limits = &swarm.Limit{}
	}
	res.Limits.NanoCPUs = cpuLimit
	res.Limits.MemoryBytes = memLimit
	if res.Reservations == nil {
		res.Reservations = &swarm.Resources{}
	}
	res.Reservations.NanoCPUs = cpuReservation
	res.Reservations.MemoryBytes = memReservation
	_, err = dcli.ServiceUpdate(ctx, svc.ID, svc.Version, spec, types.ServiceUpdateOptions{})
	return err
}

// parseCPUCores parses a CPU amount in cores ("0.5", "2") into NanoCPUs. An
// empty string means "unset" (0).
func parseCPUCores(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	cores, err := strconv.ParseFloat(s, 64)
	if err != nil || cores < 0 {
		return 0, fmt.Errorf("invalid CPU value %q (use cores, e.g. 0.5 or 2)", s)
	}
	return int64(cores * 1e9), nil
}

// parseMemBytes parses a human memory size ("512m", "2g", "1.5GiB") into bytes.
// An empty string means "unset" (0).
func parseMemBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	b, err := units.RAMInBytes(s)
	if err != nil || b < 0 {
		return 0, fmt.Errorf("invalid memory value %q (use e.g. 512m, 2g, 1.5GiB)", s)
	}
	return b, nil
}

// formatCPUCores renders NanoCPUs as a compact cores string ("" when unset).
func formatCPUCores(nano int64) string {
	if nano <= 0 {
		return ""
	}
	return strconv.FormatFloat(float64(nano)/1e9, 'f', -1, 64)
}

// formatMemBytes renders bytes as a 1024-based size ("" when unset).
func formatMemBytes(b int64) string {
	if b <= 0 {
		return ""
	}
	return units.BytesSize(float64(b))
}

// formatServicePort renders a port config as "published:target/proto".
func formatServicePort(p swarm.PortConfig) string {
	return fmt.Sprintf("%d:%d/%s", p.PublishedPort, p.TargetPort, p.Protocol)
}

// parseServicePort parses "PUBLISHED:TARGET[/proto]" (proto tcp|udp|sctp,
// default tcp) into a PortConfig published in ingress mode.
func parseServicePort(s string) (swarm.PortConfig, error) {
	s = strings.TrimSpace(s)
	proto := "tcp"
	if i := strings.LastIndex(s, "/"); i >= 0 {
		proto = strings.ToLower(strings.TrimSpace(s[i+1:]))
		s = s[:i]
	}
	pt := strings.SplitN(s, ":", 2)
	if len(pt) != 2 {
		return swarm.PortConfig{}, fmt.Errorf("port must be PUBLISHED:TARGET[/proto], e.g. 8080:80/tcp")
	}
	pub, err := strconv.ParseUint(strings.TrimSpace(pt[0]), 10, 32)
	if err != nil || pub == 0 {
		return swarm.PortConfig{}, fmt.Errorf("invalid published port %q", pt[0])
	}
	tgt, err := strconv.ParseUint(strings.TrimSpace(pt[1]), 10, 32)
	if err != nil || tgt == 0 {
		return swarm.PortConfig{}, fmt.Errorf("invalid target port %q", pt[1])
	}
	switch proto {
	case "tcp", "udp", "sctp":
	default:
		return swarm.PortConfig{}, fmt.Errorf("protocol must be tcp, udp or sctp (got %q)", proto)
	}
	return swarm.PortConfig{
		Protocol:      swarm.PortConfigProtocol(proto),
		PublishedPort: uint32(pub),
		TargetPort:    uint32(tgt),
		PublishMode:   swarm.PortConfigPublishModeIngress,
	}, nil
}

// portsFromStrings converts edited strings back to port configs, rejecting a
// duplicate published-port/protocol pair.
func portsFromStrings(items []string) ([]swarm.PortConfig, error) {
	var out []swarm.PortConfig
	seen := map[string]bool{}
	for _, it := range items {
		p, err := parseServicePort(it)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%d/%s", p.PublishedPort, p.Protocol)
		if seen[key] {
			return nil, fmt.Errorf("duplicate published port %s", key)
		}
		seen[key] = true
		out = append(out, p)
	}
	return out, nil
}

// parseLabel splits "key=value" (value may be empty; key may not).
func parseLabel(s string) (string, string, error) {
	i := strings.Index(s, "=")
	if i <= 0 {
		return "", "", fmt.Errorf("label must be key=value")
	}
	k := strings.TrimSpace(s[:i])
	if k == "" {
		return "", "", fmt.Errorf("label key must not be empty")
	}
	return k, strings.TrimSpace(s[i+1:]), nil
}

// labelsFromStrings converts edited "key=value" strings back to a label map,
// rejecting a duplicate key.
func labelsFromStrings(items []string) (map[string]string, error) {
	out := map[string]string{}
	for _, it := range items {
		k, v, err := parseLabel(it)
		if err != nil {
			return nil, err
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate label key %q", k)
		}
		out[k] = v
	}
	return out, nil
}

// parseKVList parses a comma-separated "k=v,k=v" string into a map, tolerating
// surrounding spaces and empty segments. Empty input yields a nil map.
func parseKVList(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var items []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			items = append(items, p)
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	return labelsFromStrings(items)
}
