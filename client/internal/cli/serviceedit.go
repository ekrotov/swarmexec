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
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
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
