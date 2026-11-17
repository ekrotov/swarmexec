// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
)

// The cluster at a glance — nodes, services, replicas, health — as the MCP
// tool `cluster_overview` and the CLI's `swarmexec status` both report it.
// The JSON shape is the MCP tool's, unchanged: assistants already read it.

// OverviewNode is one node of the overview.
type OverviewNode struct {
	Hostname     string `json:"hostname"`
	Role         string `json:"role"`
	State        string `json:"state"`
	Availability string `json:"availability"`
	Leader       bool   `json:"leader,omitempty"`
	Engine       string `json:"engine,omitempty"`
}

// HealthTally is a service's container health counts.
type HealthTally struct {
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
	Starting  int `json:"starting"`
	NoCheck   int `json:"no_healthcheck_or_unknown"`
}

// ServiceRow is one service of the overview (and of the MCP services list).
type ServiceRow struct {
	Name        string         `json:"name"`
	Stack       string         `json:"stack,omitempty"`
	Mode        string         `json:"mode"`
	Replicas    string         `json:"replicas"`
	Image       string         `json:"image,omitempty"`
	Ports       string         `json:"ports,omitempty"`
	UpdateState string         `json:"update_state,omitempty"`
	Health      *HealthTally   `json:"health,omitempty"`
	Containers  []ContainerRow `json:"containers,omitempty"`
}

// ContainerRow is one running container of a service.
type ContainerRow struct {
	Slot      int    `json:"slot,omitempty"`
	Container string `json:"container"`
	Node      string `json:"node"`
	Uptime    string `json:"uptime"`
}

// Overview is the whole report.
type Overview struct {
	Context  string         `json:"context"`
	Manager  string         `json:"manager"`
	Engine   string         `json:"engine,omitempty"`
	Nodes    []OverviewNode `json:"nodes"`
	Services []ServiceRow   `json:"services"`
	Summary  string         `json:"summary"`
	Note     string         `json:"note,omitempty"`
	// Degraded is how many listed services run short or have an unhealthy
	// container; the summary says it in words.
	Degraded int `json:"-"`
}

// OverviewNoHealth is the overview's note when no agent answered.
const OverviewNoHealth = "no live health data from the node agents; health omitted"

// OverviewAPI is the slice of the Docker client the overview reads.
type OverviewAPI interface {
	Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error)
	NodeList(context.Context, client.NodeListOptions) (client.NodeListResult, error)
}

// GatherOverview builds the overview of the cluster label names. usage is the
// agents' container usage — best effort: empty means none answered, and the
// health tallies are then left out and the note says so rather than showing
// "all healthy". visible filters the services (nil: all of them).
func GatherOverview(ctx context.Context, dcli OverviewAPI, r *resolve.Resolver, label string, usage map[string]agentcall.ContainerUsage, visible func(string) bool) (Overview, error) {
	info, err := dcli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return Overview{}, err
	}
	nodes, err := dcli.NodeList(ctx, client.NodeListOptions{})
	if err != nil {
		return Overview{}, err
	}
	out := Overview{Context: label, Manager: info.Info.Name, Engine: info.Info.ServerVersion,
		Nodes: OverviewNodes(nodes.Items), Services: []ServiceRow{}}

	svcs, cands, err := ServicesAndContainers(ctx, r, "overview")
	if err != nil {
		return Overview{}, err
	}
	if len(usage) == 0 {
		out.Note = OverviewNoHealth
	}
	for _, rs := range svcs {
		if visible != nil && !visible(rs.Name) {
			continue
		}
		row := OverviewServiceRow(rs, usage, cands)
		have, want := rs.Progress()
		if (!rs.IsJob() && have < want) || (row.Health != nil && row.Health.Unhealthy > 0) {
			out.Degraded++
		}
		out.Services = append(out.Services, row)
	}
	out.Summary = fmt.Sprintf("%d node(s), %d service(s) visible, %d degraded", len(out.Nodes), len(out.Services), out.Degraded)
	return out, nil
}

// ClusterUsage asks every node's agent for container usage, once. Empty when
// the node list cannot be read or no agent answers.
func ClusterUsage(ctx context.Context, r *resolve.Resolver, ac *agentcall.Client, connectTimeout time.Duration) map[string]agentcall.ContainerUsage {
	usage := map[string]agentcall.ContainerUsage{}
	if rn, err := r.Nodes(ctx); err == nil {
		usage, _ = ac.Usage(ctx, rn, connectTimeout)
	}
	return usage
}

// OverviewNodes are the overview's node rows, sorted by hostname.
func OverviewNodes(nodes []swarm.Node) []OverviewNode {
	rows := []OverviewNode{}
	for _, n := range nodes {
		row := OverviewNode{
			Hostname:     NodeDisplayName(n.Description.Hostname, n.ID),
			Role:         string(n.Spec.Role),
			State:        string(n.Status.State),
			Availability: string(n.Spec.Availability),
			Engine:       n.Description.Engine.EngineVersion,
		}
		if n.ManagerStatus != nil {
			row.Leader = n.ManagerStatus.Leader
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Hostname < rows[j].Hostname })
	return rows
}

// OverviewServiceRow is one service's row, with health when there is usage.
func OverviewServiceRow(rs resolve.Service, usage map[string]agentcall.ContainerUsage, cands []resolve.Candidate) ServiceRow {
	row := ServiceRowOf(rs)
	if len(usage) > 0 {
		h := HealthOf(usage, cands, rs.Name)
		row.Health = &HealthTally{Healthy: h.Healthy, Unhealthy: h.Unhealthy, Starting: h.Starting, NoCheck: h.Unknown}
	}
	return row
}

// ServiceRowOf is a service's row without health or containers.
func ServiceRowOf(rs resolve.Service) ServiceRow {
	have, want := rs.Progress()
	return ServiceRow{
		Name: rs.Name, Stack: rs.Stack, Mode: rs.Mode,
		Replicas:    fmt.Sprintf("%d/%d", have, want),
		Image:       rs.Image,
		Ports:       rs.Ports,
		UpdateState: rs.UpdateState,
	}
}
