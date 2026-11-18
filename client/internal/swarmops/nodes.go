// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/swarmapi"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// Nodes are swarm-scoped (managed by the manager), so their general info and
// labels come straight from the manager API (NodeList / NodeUpdate). The
// running-task count per node also comes from the manager (TaskList); the
// per-node volume count is node-local and filled in separately from the agent
// volume sweep (VolumeCountsByNode).

// NodeInfo is a cluster node with the aggregations shown in the nodes tab.
type NodeInfo struct {
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
	// TasksKnown is false when the task list could not be read: Tasks (and
	// the booked-resource figures) are then not "0" but unknown.
	TasksKnown bool

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
func buildNodeInfos(nodes []swarm.Node, tasks []swarm.Task) []NodeInfo {
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
	out := make([]NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		info := NodeInfo{
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

// SetNodeAvailability moves a node between active / pause / drain. Read-modify-
// write on the node spec like SetNodeLabels; applies immediately (nodes have no
// rolling update). Draining reschedules the node's tasks elsewhere.
//
// loaded is the node's Version from when the caller read it (NodeInfo.Version);
// the write is refused with a *ConflictError if the node changed since (KOR-05).
func SetNodeAvailability(ctx context.Context, dcli swarmapi.Client, nodeID string, loaded swarm.Version, availability swarm.NodeAvailability) error {
	return updateNode(ctx, dcli, nodeID, loaded, func(spec *swarm.NodeSpec) { spec.Availability = availability })
}

// updateNode is the node read-modify-write: inspect, refuse when the version
// moved since the caller's read, mutate the spec, update with the loaded
// version.
func updateNode(ctx context.Context, dcli swarmapi.Client, nodeID string, loaded swarm.Version, mutate func(spec *swarm.NodeSpec)) error {
	res, err := dcli.NodeInspect(ctx, nodeID, client.NodeInspectOptions{})
	if err != nil {
		return err
	}
	node := res.Node
	name := node.Description.Hostname
	if name == "" {
		name = nodeID
	}
	if node.Version.Index != loaded.Index {
		return &ConflictError{Kind: "node", Name: name, Loaded: loaded.Index, Current: node.Version.Index}
	}
	spec := node.Spec
	mutate(&spec)
	if _, err := dcli.NodeUpdate(ctx, nodeID, client.NodeUpdateOptions{Version: loaded, Spec: spec}); err != nil {
		if c := asConflict("node", name, err); c != err {
			return c
		}
		return fmt.Errorf("update node %s: %w", nodeID, err)
	}
	return nil
}

// ListNodes returns the cluster's nodes with their running-task counts.
func ListNodes(ctx context.Context, dcli swarmapi.Client) ([]NodeInfo, error) {
	nodesRes, err := dcli.NodeList(ctx, client.NodeListOptions{})
	nodes := nodesRes.Items
	if err != nil {
		return nil, err
	}
	// Task counts are best-effort, but a failed lookup is not "no tasks": the
	// nodes come back with TasksKnown false so the UI can say "unknown".
	// Only tasks the swarm wants running matter for the count and for the
	// resources booked; the history (up to five dead tasks per slot) is left
	// on the manager.
	tasks, err := dcli.TaskList(ctx, client.TaskListOptions{Filters: desiredRunning()})
	infos := buildNodeInfos(nodes, tasks.Items)
	for i := range infos {
		infos[i].TasksKnown = err == nil
	}
	return infos, nil
}

// desiredRunning filters a TaskList to the tasks the swarm wants running,
// which leaves out the task history.
func desiredRunning() client.Filters {
	return make(client.Filters).Add("desired-state", "running")
}

// TaskCount is the node's running-task count for display: "?" when unknown.
func (n NodeInfo) TaskCount() string {
	if !n.TasksKnown {
		return "?"
	}
	return strconv.Itoa(n.Tasks)
}

// VolumeCountsByNode counts, per node hostname, the volumes that node holds.
// swarmVolume.Nodes carries the nodes a named volume exists on.
func VolumeCountsByNode(vols []agentcall.Volume) map[string]int {
	counts := map[string]int{}
	for _, v := range vols {
		for _, n := range v.Nodes {
			counts[n.Name]++
		}
	}
	return counts
}

// SetNodeLabels replaces a node's labels via NodeUpdate (read-modify-write on the
// node spec). Applies immediately — nodes have no rolling update. loaded is the
// node's Version from when the caller read it, as for SetNodeAvailability.
func SetNodeLabels(ctx context.Context, dcli swarmapi.Client, nodeID string, loaded swarm.Version, labels map[string]string) error {
	return updateNode(ctx, dcli, nodeID, loaded, func(spec *swarm.NodeSpec) { spec.Annotations.Labels = labels })
}
