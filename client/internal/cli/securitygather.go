// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"

	"swarmexec/client/internal/secscan"
)

// This is the mapping layer between what the manager returns and what the
// analyzers read. It deliberately reuses the listing helpers the ui already
// uses (listNetworks, listSecrets, listConfigs), because those resolve the two
// facts that are easy to get wrong on a second attempt: which services
// reference an object — by id OR by name — and what a network's driver options
// mean by "encrypted". A security report that disagrees with the screen about
// either is worse than no report.

// listServicesForScan runs the per-service analyzers over every service in the
// cluster.
func listServicesForScan(ctx context.Context, dcli *client.Client) ([]secscan.ServiceScan, error) {
	svcs, err := dcli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]secscan.ServiceScan, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, secscan.ServiceScan{
			Name:     s.Spec.Name,
			Findings: secscan.Scan(s),
		})
	}
	return out, nil
}

// gatherCluster collects the cluster-level objects.
//
// Networks, secrets and configs are required: each is a thing the report claims
// to have examined, and a silent empty list would turn a failed call into a
// clean bill of health. The swarm configuration is the one exception — it is
// left nil on failure and the analyzer reports it as unknown, because "could
// not read" and "autolock is off" are different statements and only one of them
// is true.
func gatherCluster(ctx context.Context, dcli *client.Client) (secscan.Cluster, error) {
	nets, err := listNetworks(ctx, dcli)
	if err != nil {
		return secscan.Cluster{}, fmt.Errorf("list networks: %w", err)
	}
	secrets, err := listSecrets(ctx, dcli)
	if err != nil {
		return secscan.Cluster{}, fmt.Errorf("list secrets: %w", err)
	}
	configs, err := listConfigs(ctx, dcli)
	if err != nil {
		return secscan.Cluster{}, fmt.Errorf("list configs: %w", err)
	}
	nodes, err := dcli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return secscan.Cluster{}, fmt.Errorf("list nodes: %w", err)
	}

	c := secscan.Cluster{
		Networks: make([]secscan.Network, 0, len(nets)),
		Secrets:  make([]secscan.Credential, 0, len(secrets)),
		Configs:  make([]secscan.Credential, 0, len(configs)),
		Nodes:    make([]secscan.Node, 0, len(nodes)),
	}
	for _, n := range nets {
		c.Networks = append(c.Networks, secscan.Network{
			Name:       n.Name,
			Driver:     n.Driver,
			Scope:      n.Scope,
			Ingress:    n.Ingress,
			Internal:   n.Internal,
			Attachable: n.Attachable,
			Encrypted:  n.Encrypted,
			Services:   n.Services,
		})
	}
	for _, s := range secrets {
		c.Secrets = append(c.Secrets, secscan.Credential{Name: s.Name, Services: s.Services})
	}
	for _, cf := range configs {
		c.Configs = append(c.Configs, secscan.Credential{Name: cf.Name, Services: cf.Services})
	}
	for _, n := range nodes {
		c.Nodes = append(c.Nodes, secscan.Node{
			Name:          nodeDisplayName(n.Description.Hostname, n.ID),
			Role:          string(n.Spec.Role),
			EngineVersion: n.Description.Engine.EngineVersion,
		})
	}

	if sw, err := dcli.SwarmInspect(ctx); err == nil {
		c.Swarm = &secscan.SwarmInfo{AutoLockManagers: sw.Spec.EncryptionConfig.AutoLockManagers}
	}
	return c, nil
}

// nodeDisplayName prefers the hostname and falls back to the id, so a node that
// has not reported a description still appears in the report under something
// the operator can act on rather than as an empty row.
func nodeDisplayName(hostname, id string) string {
	if hostname != "" {
		return hostname
	}
	return shortID(id)
}

// contextLabel names the cluster the report ran against. The docker context
// name is what the operator typed and recognises; the manager's own name is the
// fallback, because a report that does not say which cluster it describes is a
// hazard once two of them are in the same directory.
//
// contextName is passed in rather than read off the global flags, for the same
// reason the agent tunnel now is: in the ui the flag and the cluster actually in
// use come apart the moment someone switches context, and a report headed with
// the wrong cluster is worse than one with no header at all.
func contextLabel(ctx context.Context, dcli *client.Client, contextName string) string {
	if contextName != "" {
		return contextName
	}
	if info, err := dcli.Info(ctx); err == nil && info.Name != "" {
		return info.Name
	}
	return ""
}
