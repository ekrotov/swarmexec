// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
)

// Nodes are swarm-scoped (managed by the manager), so their general info and
// labels come straight from the manager API (NodeList / NodeUpdate). The
// running-task count per node also comes from the manager (TaskList); the
// per-node volume count is node-local and filled in separately from the agent
// volume sweep (volumeCountsByNode).

// swarmNodeInfo is a cluster node with the aggregations shown in the nodes tab.
type swarmNodeInfo struct {
	ID            string
	Hostname      string
	Role          string // "manager" | "worker"
	Leader        bool   // manager leader
	Availability  string // "active" | "pause" | "drain"
	State         string // "ready" | "down" | …
	Addr          string
	EngineVersion string
	OS            string
	Arch          string
	NanoCPUs      int64
	MemoryBytes   int64
	Labels        map[string]string
	Version       swarm.Version // for NodeUpdate (optimistic concurrency)
	Tasks         int           // running tasks scheduled on this node
}

// buildNodeInfos maps the manager's node list plus the task list into the tab's
// row model, counting the running tasks scheduled on each node. Pure, so it is
// unit-testable without a daemon.
func buildNodeInfos(nodes []swarm.Node, tasks []swarm.Task) []swarmNodeInfo {
	running := map[string]int{}
	for _, t := range tasks {
		if t.Status.State == swarm.TaskStateRunning {
			running[t.NodeID]++
		}
	}
	out := make([]swarmNodeInfo, 0, len(nodes))
	for _, n := range nodes {
		info := swarmNodeInfo{
			ID:            n.ID,
			Hostname:      n.Description.Hostname,
			Role:          string(n.Spec.Role),
			Availability:  string(n.Spec.Availability),
			State:         string(n.Status.State),
			Addr:          n.Status.Addr,
			EngineVersion: n.Description.Engine.EngineVersion,
			OS:            n.Description.Platform.OS,
			Arch:          n.Description.Platform.Architecture,
			NanoCPUs:      n.Description.Resources.NanoCPUs,
			MemoryBytes:   n.Description.Resources.MemoryBytes,
			Labels:        n.Spec.Annotations.Labels,
			Version:       n.Version,
			Tasks:         running[n.ID],
		}
		if n.ManagerStatus != nil {
			info.Leader = n.ManagerStatus.Leader
		}
		out = append(out, info)
	}
	// Managers first, then by hostname — the natural operator ordering.
	sort.Slice(out, func(i, j int) bool {
		mi, mj := out[i].Role == "manager", out[j].Role == "manager"
		if mi != mj {
			return mi
		}
		return out[i].Hostname < out[j].Hostname
	})
	return out
}

// listNodeInfos returns the cluster's nodes with their running-task counts.
func listNodeInfos(ctx context.Context, dcli *client.Client) ([]swarmNodeInfo, error) {
	nodes, err := dcli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	// Task counts are best-effort: a TaskList error just leaves them at zero.
	tasks, _ := dcli.TaskList(ctx, types.TaskListOptions{})
	return buildNodeInfos(nodes, tasks), nil
}

// volumeCountsByNode counts, per node hostname, the volumes that node holds.
// swarmVolume.Nodes carries the nodes a named volume exists on.
func volumeCountsByNode(vols []swarmVolume) map[string]int {
	counts := map[string]int{}
	for _, v := range vols {
		for _, n := range v.Nodes {
			counts[n.Name]++
		}
	}
	return counts
}

// setNodeLabels replaces a node's labels via NodeUpdate (read-modify-write on the
// node spec). Applies immediately — nodes have no rolling update.
func setNodeLabels(ctx context.Context, dcli *client.Client, nodeID string, labels map[string]string) error {
	n, _, err := dcli.NodeInspectWithRaw(ctx, nodeID)
	if err != nil {
		return err
	}
	spec := n.Spec
	spec.Annotations.Labels = labels
	if err := dcli.NodeUpdate(ctx, nodeID, n.Version, spec); err != nil {
		return fmt.Errorf("update node %s: %w", nodeID, err)
	}
	return nil
}
