// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package clusterread

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/secscan"
	"swarmexec/client/internal/swarmapi"
	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/dockerfmt"
)

// This is the mapping layer between what the manager returns and what the
// analyzers read. It deliberately reuses the listing helpers the ui already
// uses (ListNetworks, ListSecrets, ListConfigs), because those resolve the two
// facts that are easy to get wrong on a second attempt: which services
// reference an object — by id OR by name — and what a network's driver options
// mean by "encrypted". A security report that disagrees with the screen about
// either is worse than no report.

// listServicesForScan runs the per-service analyzers over every service in the
// cluster.
func listServicesForScan(ctx context.Context, dcli swarmapi.Client) ([]secscan.ServiceScan, error) {
	hostNets := HostNetworks(ctx, dcli)
	svcsRes, err := dcli.ServiceList(ctx, client.ServiceListOptions{})
	svcs := svcsRes.Items
	if err != nil {
		return nil, err
	}
	out := make([]secscan.ServiceScan, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, secscan.ServiceScan{
			Name:     s.Spec.Name,
			Findings: secscan.ScanWithHostNetworks(s, hostNets),
		})
	}
	return out, nil
}

// HostNetworks returns the ids and names of the cluster's host-driver
// networks, which is how a deployed service may reference the host network
// (the manager can swap the literal "host" for the id). Best effort: on a list
// error it returns nil and the scan falls back to the literal.
func HostNetworks(ctx context.Context, dcli swarmapi.Client) map[string]bool {
	res, err := dcli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, n := range res.Items {
		if n.Driver == "host" {
			out[n.ID] = true
			out[n.Name] = true
		}
	}
	return out
}

// GatherCluster collects the cluster-level objects.
//
// Networks, secrets and configs are required: each is a thing the report claims
// to have examined, and a silent empty list would turn a failed call into a
// clean bill of health. The swarm configuration is the one exception — it is
// left nil on failure and the analyzer reports it as unknown, because "could
// not read" and "autolock is off" are different statements and only one of them
// is true.
func GatherCluster(ctx context.Context, dcli swarmapi.Client) (secscan.Cluster, error) {
	nets, err := swarmops.ListNetworks(ctx, dcli, nil)
	if err != nil {
		return secscan.Cluster{}, fmt.Errorf("list networks: %w", err)
	}
	secrets, err := swarmops.ListSecrets(ctx, dcli)
	if err != nil {
		return secscan.Cluster{}, fmt.Errorf("list secrets: %w", err)
	}
	configs, err := swarmops.ListConfigs(ctx, dcli)
	if err != nil {
		return secscan.Cluster{}, fmt.Errorf("list configs: %w", err)
	}
	nodesRes, err := dcli.NodeList(ctx, client.NodeListOptions{})
	nodes := nodesRes.Items
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
			Subnets:    n.Subnets,
		})
	}
	// The services' configuration TEXT, for the checks that read a spec
	// against the cluster around it (a pinned overlay address). Best effort:
	// without it those checks stay quiet, which is better than failing the
	// whole report over one list.
	if res, err := dcli.ServiceList(ctx, client.ServiceListOptions{}); err == nil {
		for _, svc := range res.Items {
			c.Services = append(c.Services, secscan.ServiceSpecText{
				Name: svc.Spec.Name,
				Text: specText(svc),
			})
		}
	}
	for _, s := range secrets {
		c.Secrets = append(c.Secrets, secscan.Credential{Name: s.Name, Services: s.Services})
	}
	for _, cf := range configs {
		c.Configs = append(c.Configs, secscan.Credential{Name: cf.Name, Services: cf.Services})
	}
	for _, n := range nodes {
		c.Nodes = append(c.Nodes, secscan.Node{
			Name:          NodeDisplayName(n.Description.Hostname, n.ID),
			Role:          string(n.Spec.Role),
			EngineVersion: n.Description.Engine.EngineVersion,
		})
	}

	if swRes, err := dcli.SwarmInspect(ctx, client.SwarmInspectOptions{}); err == nil {
		sw := swRes.Swarm
		c.Swarm = &secscan.SwarmInfo{AutoLockManagers: sw.Spec.EncryptionConfig.AutoLockManagers}
	}
	return c, nil
}

// NodeDisplayName prefers the hostname and falls back to the id, so a node that
// has not reported a description still appears in the report under something
// the operator can act on rather than as an empty row.
func NodeDisplayName(hostname, id string) string {
	if hostname != "" {
		return hostname
	}
	return dockerfmt.ShortID(id)
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
func contextLabel(ctx context.Context, dcli swarmapi.Client, contextName string) string {
	if contextName != "" {
		return contextName
	}
	if infoRes, err := dcli.Info(ctx, client.InfoOptions{}); err == nil && infoRes.Info.Name != "" {
		info := infoRes.Info
		return info.Name
	}
	return ""
}

// GatherSecurityReport collects everything the scan needs from the manager (and
// optionally the agents) and builds the report.
//
// The manager calls are required: a report missing half the cluster is worse
// than no report, because it looks complete. The agent round trip is optional
// and its absence is recorded as a gap rather than passed over.
// SecurityReportOptions is what a report run needs from its caller: who is
// asking (the version stamp and the tool line), which cluster it is (the
// context label), and whether and how to ask the agents.
type SecurityReportOptions struct {
	// ContextName is the docker context the report describes; the label in
	// the report falls back to the swarm's own name without one.
	ContextName string
	// ClientVersion and ClientProto stamp the report with this client.
	ClientVersion, ClientProto string
	// Tool names the generator in the report header ("swarmexec v1.2.3").
	Tool string
	// SkipAgents leaves the agent round trip out; the report states the gap.
	SkipAgents bool
	// ConnectTimeout bounds each agent's Version call.
	ConnectTimeout time.Duration
	// AddrMode is how node addresses are picked for the agent calls.
	AddrMode resolve.AddrMode
}

func GatherSecurityReport(ctx context.Context, dcli swarmapi.Client, ac *agentcall.Client, opts SecurityReportOptions) (secscan.Report, error) {
	svcs, err := listServicesForScan(ctx, dcli)
	if err != nil {
		return secscan.Report{}, fmt.Errorf("list services: %w", err)
	}

	cluster, err := GatherCluster(ctx, dcli)
	if err != nil {
		return secscan.Report{}, err
	}
	cluster.ClientVersion, cluster.ClientProto = opts.ClientVersion, opts.ClientProto

	if !opts.SkipAgents {
		nodes, nerr := resolve.New(dcli, opts.AddrMode).Nodes(ctx)
		if nerr == nil {
			ApplyAgentVersions(ctx, &cluster, ac, nodes, opts.ConnectTimeout)
		}
		// A failure to resolve dialable nodes leaves AgentsChecked false, which
		// the report turns into a stated gap. Better than a half-filled node
		// list that reads as complete.
	}

	meta := secscan.ReportMeta{
		Context:   contextLabel(ctx, dcli, opts.ContextName),
		Generated: time.Now(),
		Tool:      opts.Tool,
	}
	return secscan.BuildReport(meta, svcs, cluster), nil
}

// ApplyAgentVersions asks every node's agent for its version and records the
// answer — including the failures, which are findings in their own right.
func ApplyAgentVersions(ctx context.Context, c *secscan.Cluster, ac *agentcall.Client, nodes []resolve.Node, timeout time.Duration) {
	results := ac.Versions(ctx, nodes, timeout)
	byName := map[string]agentcall.NodeVersion{}
	for _, r := range results {
		byName[r.Node.Name] = r
	}
	for i := range c.Nodes {
		r, ok := byName[c.Nodes[i].Name]
		if !ok {
			continue
		}
		switch {
		case r.Err != nil && agenterr.TooOld(r.Err):
			// The same signal doctor reads: an agent predating the Version RPC
			// answers Unimplemented rather than failing to connect.
			c.Nodes[i].AgentTooOld = true
		case r.Err != nil:
			c.Nodes[i].AgentError = r.Err.Error()
		default:
			c.Nodes[i].AgentVersion, c.Nodes[i].AgentProto = r.Version, r.Proto
		}
	}
	c.AgentsChecked = true
}

// specText joins the strings a service's configuration is written in:
// environment, command, arguments and labels. Deliberately not the mounts or
// the image — an address in a bind path or a registry host is a different
// thing — and never any secret or config CONTENT, which the Docker API does
// not return anyway.
func specText(svc swarm.Service) string {
	var b strings.Builder
	cs := svc.Spec.TaskTemplate.ContainerSpec
	if cs != nil {
		for _, e := range cs.Env {
			b.WriteString(e)
			b.WriteByte('\n')
		}
		for _, a := range cs.Command {
			b.WriteString(a)
			b.WriteByte('\n')
		}
		for _, a := range cs.Args {
			b.WriteString(a)
			b.WriteByte('\n')
		}
		for k, v := range cs.Labels {
			b.WriteString(k + "=" + v + "\n")
		}
	}
	for k, v := range svc.Spec.Labels {
		b.WriteString(k + "=" + v + "\n")
	}
	return b.String()
}
