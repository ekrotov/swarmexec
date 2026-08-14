// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
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

// placeReport is the full diagnosis for one service.
type placeReport struct {
	Service     string
	Mode        string
	Global      bool
	Running     int
	Desired     int
	Rows        []placeRow
	Tasks       []taskPS // the `docker service ps` equivalent (recent tasks first)
	Unevaluated []string // constraints we can't check client-side (engine.labels.*)
}

// taskPS is one row of the `docker service ps` equivalent: a task with its
// target node, desired vs current state, when it last changed, and the
// scheduler/runtime error if any. This is the authoritative detail behind a
// vague "no task scheduled" — a failed/rejected task carries the real reason.
type taskPS struct {
	Name    string    // service.slot (replicated) or service.hostname (global)
	Node    string    // node hostname, or "(unassigned)"
	Desired string    // desired state (Running / Shutdown / …)
	Current string    // current state (running / rejected / failed / …)
	When    time.Time // Status.Timestamp of the last state change
	Err     string    // Status.Err / Message when the state itself isn't self-explanatory
}

// diagnoseServicePlacement fetches the service, nodes and tasks and builds the
// report.
func diagnoseServicePlacement(ctx context.Context, dcli *client.Client, name string) (placeReport, error) {
	svc, err := serviceByName(ctx, dcli, name)
	if err != nil {
		return placeReport{}, err
	}
	if svc == nil {
		return placeReport{}, fmt.Errorf("no service named %q", name)
	}
	nodes, err := dcli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return placeReport{}, err
	}
	tasks, err := dcli.TaskList(ctx, types.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", svc.ID)),
	})
	if err != nil {
		return placeReport{}, err
	}
	return buildPlaceReport(*svc, nodes, tasks), nil
}

// buildPlaceReport is the pure core (no I/O), so it is unit-testable.
func buildPlaceReport(svc swarm.Service, nodes []swarm.Node, tasks []swarm.Task) placeReport {
	rep := placeReport{Service: svc.Spec.Name, Global: svc.Spec.Mode.Global != nil}
	rep.Mode = "replicated"
	if rep.Global {
		rep.Mode = "global"
	}
	var constraints []string
	var platforms []swarm.Platform
	if p := svc.Spec.TaskTemplate.Placement; p != nil {
		constraints = p.Constraints
		platforms = p.Platforms
	}
	for _, t := range tasks {
		if t.Status.State == swarm.TaskStateRunning {
			rep.Running++
		}
	}

	// The most relevant task per node: a running one wins, else the most recent
	// one — INCLUDING those already retired to shutdown, because a repeatedly
	// failing/rejected task (which the scheduler sets to desired-shutdown) is
	// exactly the reason a node has "no running task", and its error is the
	// answer the operator is looking for.
	nodeTask := map[string]swarm.Task{}
	for _, t := range tasks {
		if cur, ok := nodeTask[t.NodeID]; !ok || taskMoreRelevant(t, cur) {
			nodeTask[t.NodeID] = t
		}
	}
	nodeByID := map[string]swarm.Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}

	uneval := map[string]bool{}
	if rep.Global {
		ordered := append([]swarm.Node{}, nodes...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].Description.Hostname < ordered[j].Description.Hostname })
		for _, n := range ordered {
			host := n.Description.Hostname
			t, has := nodeTask[n.ID]
			switch {
			case has && t.Status.State == swarm.TaskStateRunning:
				rep.Rows = append(rep.Rows, placeRow{host, true, "running"})
				rep.Desired++
			case has:
				detail := taskDetail(t)
				if a := attemptsOnNode(tasks, n.ID); a > 1 {
					detail += fmt.Sprintf(" (%d attempts)", a)
				}
				rep.Rows = append(rep.Rows, placeRow{host, false, detail})
				rep.Desired++
			default:
				reason, excluded, un := nodeExclusionReason(n, constraints, platforms)
				for _, c := range un {
					uneval[c] = true
				}
				if excluded {
					rep.Rows = append(rep.Rows, placeRow{host, false, "excluded — " + reason})
				} else {
					rep.Rows = append(rep.Rows, placeRow{host, false, "eligible but no task scheduled"})
					rep.Desired++
				}
			}
		}
	} else {
		if r := svc.Spec.Mode.Replicated; r != nil && r.Replicas != nil {
			rep.Desired = int(*r.Replicas)
		}
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
			rep.Rows = append(rep.Rows, placeRow{node, false, taskDetail(t)})
		}
		if len(rep.Rows) == 0 && rep.Running < rep.Desired {
			rep.Rows = append(rep.Rows, placeRow{"—", false, fmt.Sprintf("%d task(s) not yet scheduled", rep.Desired-rep.Running)})
		}
	}
	for c := range uneval {
		rep.Unevaluated = append(rep.Unevaluated, c)
	}
	sort.Strings(rep.Unevaluated)
	rep.Tasks = buildTaskPS(svc.Spec.Name, tasks, nodeByID)
	return rep
}

// buildTaskPS renders the tasks as the `docker service ps` equivalent: most
// recent state change first, each with its target node, desired/current state
// and error. This is the detail the summary rows condense.
func buildTaskPS(svcName string, tasks []swarm.Task, nodeByID map[string]swarm.Node) []taskPS {
	out := make([]taskPS, 0, len(tasks))
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
		out = append(out, taskPS{
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
