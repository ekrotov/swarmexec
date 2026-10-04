// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
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

	// Resources booked on this node by the tasks scheduled to it — the sum of
	// their spec RESERVATIONS, which is what the swarm scheduler subtracts from
	// a node's capacity when deciding where a task fits. This is not live usage
	// (that is node-local and would need an agent RPC): a task with no
	// reservation set books nothing here yet can still consume the whole node.
	ReservedNanoCPUs    int64
	ReservedMemoryBytes int64
	// Tasks scheduled here that declare no reservation at all — they are
	// invisible to the scheduler's arithmetic, so the booked figures understate
	// reality by an unknown amount whenever this is non-zero.
	TasksWithoutReservation int
}

// buildNodeInfos maps the manager's node list plus the task list into the tab's
// row model, counting the running tasks scheduled on each node. Pure, so it is
// unit-testable without a daemon.
func buildNodeInfos(nodes []swarm.Node, tasks []swarm.Task) []swarmNodeInfo {
	running := map[string]int{}
	bookedCPU := map[string]int64{}
	bookedMem := map[string]int64{}
	unreserved := map[string]int{}
	for _, t := range tasks {
		if t.Status.State == swarm.TaskStateRunning {
			running[t.NodeID]++
		}
		if t.NodeID == "" || !taskHoldsResources(t) {
			continue
		}
		res := t.Spec.Resources
		if res == nil || res.Reservations == nil ||
			(res.Reservations.NanoCPUs == 0 && res.Reservations.MemoryBytes == 0) {
			unreserved[t.NodeID]++
			continue
		}
		bookedCPU[t.NodeID] += res.Reservations.NanoCPUs
		bookedMem[t.NodeID] += res.Reservations.MemoryBytes
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

			ReservedNanoCPUs:        bookedCPU[n.ID],
			ReservedMemoryBytes:     bookedMem[n.ID],
			TasksWithoutReservation: unreserved[n.ID],
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

// taskHoldsResources reports whether a task still occupies its node's capacity.
// A task holds its reservation from the moment it is assigned until it reaches a
// terminal state — so counting only "running" would understate a node that is
// mid-deploy, and counting everything would double-count the shut-down tasks
// swarm keeps in the task list as history.
func taskHoldsResources(t swarm.Task) bool {
	switch t.DesiredState {
	case swarm.TaskStateShutdown, swarm.TaskStateRemove:
		return false
	}
	switch t.Status.State {
	case swarm.TaskStateComplete, swarm.TaskStateShutdown,
		swarm.TaskStateFailed, swarm.TaskStateRejected, swarm.TaskStateRemove:
		return false
	}
	return true
}

// setNodeAvailability moves a node between active / pause / drain. Read-modify-
// write on the node spec like setNodeLabels; applies immediately (nodes have no
// rolling update). Draining reschedules the node's tasks elsewhere.
func setNodeAvailability(ctx context.Context, dcli *client.Client, nodeID string, availability swarm.NodeAvailability) error {
	nodeRes, err := dcli.NodeInspect(ctx, nodeID, client.NodeInspectOptions{})
	node := nodeRes.Node
	if err != nil {
		return err
	}
	spec := node.Spec
	spec.Availability = availability
	_, err = dcli.NodeUpdate(ctx, nodeID, client.NodeUpdateOptions{Version: node.Version, Spec: spec})
	return err
}

// listNodeInfos returns the cluster's nodes with their running-task counts.
func listNodeInfos(ctx context.Context, dcli *client.Client) ([]swarmNodeInfo, error) {
	nodesRes, err := dcli.NodeList(ctx, client.NodeListOptions{})
	nodes := nodesRes.Items
	if err != nil {
		return nil, err
	}
	// Task counts are best-effort: a TaskList error just leaves them at zero.
	tasks, _ := dcli.TaskList(ctx, client.TaskListOptions{})
	return buildNodeInfos(nodes, tasks.Items), nil
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
	nRes, err := dcli.NodeInspect(ctx, nodeID, client.NodeInspectOptions{})
	n := nRes.Node
	if err != nil {
		return err
	}
	spec := n.Spec
	spec.Annotations.Labels = labels
	if _, err := dcli.NodeUpdate(ctx, nodeID, client.NodeUpdateOptions{Version: n.Version, Spec: spec}); err != nil {
		return fmt.Errorf("update node %s: %w", nodeID, err)
	}
	return nil
}
