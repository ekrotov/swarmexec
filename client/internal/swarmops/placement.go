// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"fmt"
	"sort"
	"time"

	"swarmexec/client/internal/swarmapi"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// Placement diagnosis answers "why isn't this service running everywhere I
// expect?" without the operator running docker node ls / service ps / inspect by
// hand. For a global service it explains, per node, why no task runs there
// (availability, state, a placement constraint, or a platform mismatch); for a
// replicated service it surfaces the scheduler's own message on each task that is
// not running (the authoritative reason for pending / rejected / failed tasks).

// placeRow is one line of the diagnosis: a node (or task) and its status.
type placeRow struct {
	Node   string
	OK     bool // a task is running here
	Detail string
}

// PlacementReport is the full diagnosis for one service.
type PlacementReport struct {
	Service     string
	Mode        string
	Global      bool
	Running     int
	Desired     int
	Rows        []placeRow
	Tasks       []TaskPS // the `docker service ps` equivalent (recent tasks first)
	Unevaluated []string // constraints we can't check client-side (engine.labels.*)
}

// TaskPS is one row of the `docker service ps` equivalent: a task with its
// target node, desired vs current state, when it last changed, and the
// scheduler/runtime error if any. This is the authoritative detail behind a
// vague "no task scheduled" — a failed/rejected task carries the real reason.
type TaskPS struct {
	Name    string    // service.slot (replicated) or service.hostname (global)
	Node    string    // node hostname, or "(unassigned)"
	Desired string    // desired state (Running / Shutdown / …)
	Current string    // current state (running / rejected / failed / …)
	When    time.Time // Status.Timestamp of the last state change
	Err     string    // Status.Err / Message when the state itself isn't self-explanatory
}

// DiagnosePlacement fetches the service, nodes and tasks and builds the
// report.
func DiagnosePlacement(ctx context.Context, dcli swarmapi.Client, name string) (PlacementReport, error) {
	svc, err := ServiceByName(ctx, dcli, name)
	if err != nil {
		return PlacementReport{}, err
	}
	if svc == nil {
		return PlacementReport{}, fmt.Errorf("no service named %q", name)
	}
	nodesRes, err := dcli.NodeList(ctx, client.NodeListOptions{})
	nodes := nodesRes.Items
	if err != nil {
		return PlacementReport{}, err
	}
	tasksRes, err := dcli.TaskList(ctx, client.TaskListOptions{
		Filters: make(client.Filters).Add("service", svc.ID),
	})
	tasks := tasksRes.Items
	if err != nil {
		return PlacementReport{}, err
	}
	return buildPlaceReport(*svc, nodes, tasks), nil
}

// buildPlaceReport is the pure core (no I/O), so it is unit-testable.
func buildPlaceReport(svc swarm.Service, nodes []swarm.Node, tasks []swarm.Task) PlacementReport {
	rep := PlacementReport{Service: svc.Spec.Name, Global: svc.Spec.Mode.Global != nil, Mode: "replicated",
		Running: countRunning(tasks)}
	nodeByID := map[string]swarm.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	if rep.Global {
		rep.Mode = "global"
		rep.Rows, rep.Desired, rep.Unevaluated = globalPlaceRows(svc.Spec.TaskTemplate.Placement, nodes, tasks)
	} else {
		if r := svc.Spec.Mode.Replicated; r != nil && r.Replicas != nil {
			rep.Desired = int(*r.Replicas)
		}
		rep.Rows = replicatedPlaceRows(tasks, nodeByID, rep.Desired-rep.Running)
	}
	rep.Tasks = buildTaskPS(svc.Spec.Name, tasks, nodeByID)
	return rep
}

func countRunning(tasks []swarm.Task) int {
	n := 0
	for _, t := range tasks {
		if t.Status.State == swarm.TaskStateRunning {
			n++
		}
	}
	return n
}

// relevantTaskPerNode is the most relevant task per node: a running one
// wins, else the most recent one — INCLUDING those already retired to
// shutdown, because a repeatedly failing/rejected task (which the scheduler
// sets to desired-shutdown) is exactly the reason a node has "no running
// task", and its error is the answer the operator is looking for.
func relevantTaskPerNode(tasks []swarm.Task) map[string]swarm.Task {
	nodeTask := map[string]swarm.Task{}
	for _, t := range tasks {
		if cur, ok := nodeTask[t.NodeID]; !ok || taskMoreRelevant(t, cur) {
			nodeTask[t.NodeID] = t
		}
	}
	return nodeTask
}

// globalPlaceRows is one row per node (by hostname) for a global service,
// how many of those nodes should run a task, and the constraints that could
// not be checked client-side (sorted, nil when none).
func globalPlaceRows(p *swarm.Placement, nodes []swarm.Node, tasks []swarm.Task) (rows []placeRow, desired int, unevaluated []string) {
	var constraints []string
	var platforms []swarm.Platform
	if p != nil {
		constraints, platforms = p.Constraints, p.Platforms
	}
	nodeTask := relevantTaskPerNode(tasks)
	ordered := append([]swarm.Node{}, nodes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Description.Hostname < ordered[j].Description.Hostname })
	uneval := map[string]bool{}
	for _, n := range ordered {
		var row placeRow
		var wanted bool
		if t, has := nodeTask[n.ID]; has {
			row, wanted = taskNodeRow(n, t, tasks), true
		} else {
			var un []string
			row, wanted, un = idleNodeRow(n, constraints, platforms)
			for _, c := range un {
				uneval[c] = true
			}
		}
		rows = append(rows, row)
		if wanted {
			desired++
		}
	}
	for c := range uneval {
		unevaluated = append(unevaluated, c)
	}
	sort.Strings(unevaluated)
	return rows, desired, unevaluated
}

// taskNodeRow is the row of a node that has a task: running, or the task's
// state/error with the attempt count when the node has seen more than one.
func taskNodeRow(n swarm.Node, t swarm.Task, tasks []swarm.Task) placeRow {
	host := n.Description.Hostname
	if t.Status.State == swarm.TaskStateRunning {
		return placeRow{host, true, "running"}
	}
	detail := taskDetail(t)
	if a := attemptsOnNode(tasks, n.ID); a > 1 {
		detail += fmt.Sprintf(" (%d attempts)", a)
	}
	return placeRow{host, false, detail}
}

// idleNodeRow is the row of a node without a task: excluded (and why), or
// eligible — which counts toward desired. It also returns the constraints
// that could not be evaluated for the node.
func idleNodeRow(n swarm.Node, constraints []string, platforms []swarm.Platform) (placeRow, bool, []string) {
	host := n.Description.Hostname
	reason, excluded, un := nodeExclusionReason(n, constraints, platforms)
	if excluded {
		return placeRow{host, false, "excluded — " + reason}, false, un
	}
	return placeRow{host, false, "eligible but no task scheduled"}, true, un
}

// replicatedPlaceRows surfaces, by slot, the tasks that should run but do
// not; with none to show and missing > 0, one row says how many are not yet
// scheduled.
func replicatedPlaceRows(tasks []swarm.Task, nodeByID map[string]swarm.Node, missing int) []placeRow {
	var rows []placeRow
	ordered := append([]swarm.Task{}, tasks...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Slot < ordered[j].Slot })
	for _, t := range ordered {
		if t.DesiredState == swarm.TaskStateShutdown || t.Status.State == swarm.TaskStateRunning {
			continue // only surface the tasks that are NOT running
		}
		node := "—"
		if n, ok := nodeByID[t.NodeID]; ok && n.Description.Hostname != "" {
			node = n.Description.Hostname
		}
		rows = append(rows, placeRow{node, false, taskDetail(t)})
	}
	if len(rows) == 0 && missing > 0 {
		rows = append(rows, placeRow{"—", false, fmt.Sprintf("%d task(s) not yet scheduled", missing)})
	}
	return rows
}

// buildTaskPS renders the tasks as the `docker service ps` equivalent: most
// recent state change first, each with its target node, desired/current state
// and error. This is the detail the summary rows condense.
func buildTaskPS(svcName string, tasks []swarm.Task, nodeByID map[string]swarm.Node) []TaskPS {
	out := make([]TaskPS, 0, len(tasks))
	for _, t := range tasks {
		node := "(unassigned)"
		if n, ok := nodeByID[t.NodeID]; ok && n.Description.Hostname != "" {
			node = n.Description.Hostname
		}
		// Global tasks have slot 0; name them by node (like docker does), else by slot.
		name := svcName + "." + node
		if t.Slot > 0 {
			name = fmt.Sprintf("%s.%d", svcName, t.Slot)
		}
		msg := t.Status.Err
		if msg == "" && t.Status.Message != "" && t.Status.Message != string(t.Status.State) {
			msg = t.Status.Message
		}
		out = append(out, TaskPS{
			Name:    name,
			Node:    node,
			Desired: string(t.DesiredState),
			Current: string(t.Status.State),
			When:    t.Status.Timestamp,
			Err:     msg,
		})
	}
	// Most recent state change first (zero timestamps sink to the bottom).
	sort.SliceStable(out, func(i, j int) bool { return out[i].When.After(out[j].When) })
	return out
}

// attemptsOnNode counts how many tasks the service has had on a node — a high
// count signals a crash/reject loop rather than a one-off failure.
func attemptsOnNode(tasks []swarm.Task, nodeID string) int {
	n := 0
	for _, t := range tasks {
		if t.NodeID == nodeID {
			n++
		}
	}
	return n
}

// taskRank ranks a node's tasks so a running one wins over a pending/failed one.
func taskRank(t swarm.Task) int {
	if t.Status.State == swarm.TaskStateRunning {
		return 2
	}
	if t.DesiredState == swarm.TaskStateRunning {
		return 1
	}
	return 0
}

// taskMoreRelevant reports whether task a should represent a node over task b: a
// higher rank wins (running > wanted-running > retired), ties break on recency.
func taskMoreRelevant(a, b swarm.Task) bool {
	if ra, rb := taskRank(a), taskRank(b); ra != rb {
		return ra > rb
	}
	return a.Status.Timestamp.After(b.Status.Timestamp)
}

// taskDetail renders a non-running task's state plus the scheduler/runtime
// message that explains it (the authoritative "why").
func taskDetail(t swarm.Task) string {
	state := string(t.Status.State)
	msg := t.Status.Err
	if msg == "" {
		msg = t.Status.Message
	}
	if msg != "" && msg != state {
		return state + ": " + msg
	}
	return state
}

// nodeExclusionReason reports why a global service places no task on a node —
// availability, state, an unmet constraint, or a platform mismatch. excluded is
// false when the node is eligible. unevaluated lists constraints we can't check
// client-side (engine.labels.*), so we don't wrongly call a node ineligible.
func nodeExclusionReason(n swarm.Node, constraints []string, platforms []swarm.Platform) (reason string, excluded bool, unevaluated []string) {
	if n.Spec.Availability != swarm.NodeAvailabilityActive {
		return "node availability is " + string(n.Spec.Availability), true, nil
	}
	if n.Status.State != swarm.NodeStateReady {
		return "node is " + string(n.Status.State), true, nil
	}
	for _, c := range constraints {
		matches, known := nodeMatchesConstraint(n, c)
		if !known {
			unevaluated = append(unevaluated, c)
			continue
		}
		if !matches {
			return "constraint not satisfied: " + c, true, unevaluated
		}
	}
	if len(platforms) > 0 && !nodeMatchesPlatform(n, platforms) {
		return fmt.Sprintf("platform %s/%s not in the service's supported platforms",
			n.Description.Platform.OS, n.Description.Platform.Architecture), true, unevaluated
	}
	return "", false, unevaluated
}

// nodeMatchesPlatform reports whether the node's OS/arch satisfies any of the
// service's supported platforms (an empty field matches anything).
func nodeMatchesPlatform(n swarm.Node, platforms []swarm.Platform) bool {
	for _, p := range platforms {
		archOK := p.Architecture == "" || p.Architecture == n.Description.Platform.Architecture
		osOK := p.OS == "" || p.OS == n.Description.Platform.OS
		if archOK && osOK {
			return true
		}
	}
	return false
}
