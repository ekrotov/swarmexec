// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"time"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
)

// Live container usage, one row per container — what the MCP tool `stats`
// and the CLI's `swarmexec stats` both answer. One implementation, so the
// assistant and the operator cannot be told two different things about the
// same container (review P-03).

// StatsRow is one container's reading, or why there is none.
type StatsRow struct {
	Service   string `json:"service"`
	Slot      int    `json:"slot,omitempty"`
	Node      string `json:"node"`
	Container string `json:"container"`
	CPU       string `json:"cpu"`
	Memory    string `json:"memory"`
	Health    string `json:"health,omitempty"`
	Note      string `json:"note,omitempty"`
}

// What a stats row says when it has no number. The two cases want different
// things from the reader: a sampler that is warming up has the answer a few
// seconds from now, an agent that did not answer does not.
const (
	StatsWarmingUp     = "warming up"
	StatsWarmingUpNote = "the node's agent had not finished its first readings yet (it samples only while someone asks, and CPU needs two readings); ask again in a few seconds"
	StatsNoAgent       = "unavailable"
	StatsNoAgentNote   = "the agent on this node did not answer the stats request (unreachable, or too old to serve stats); doctor shows which"
)

// Defaults for how long a one-shot stats read waits for a cold agent sampler.
//
// The agent samples only while someone is asking and stops after a minute
// without a request. A one-shot reader (the MCP tool, the CLI) asks once, not
// every few seconds as the ui does, so it almost always meets a sampler that
// is asleep: the first answer is empty, the reading with memory arrives one
// sampling interval (5s) later, and CPU — a delta — one more interval after
// that. Without waiting, every answer was "unavailable".
const (
	StatsWaitDefault = 12 * time.Second
	StatsPollDefault = 2 * time.Second
)

// StatsPoll is how a one-shot stats read asks: the per-node connect timeout,
// how long to keep asking while samplers warm up, and how often.
type StatsPoll struct {
	ConnectTimeout time.Duration
	Wait           time.Duration
	Interval       time.Duration
}

// GatherStats reads the live usage of the containers of every service wanted
// says yes to, asking only the nodes that run one of them.
func GatherStats(ctx context.Context, r *resolve.Resolver, ac *agentcall.Client, wanted func(svc string) bool, p StatsPoll) ([]StatsRow, error) {
	cands, err := r.Candidates(ctx, "")
	if err != nil {
		return nil, err
	}
	nodes, err := r.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	mine, ask := StatsTargets(cands, nodes, wanted)
	usage, answered, err := PollUsage(ctx, ac, ask, mine, p)
	if err != nil {
		return nil, err
	}
	return StatsRows(mine, usage, answered), nil
}

// StatsTargets is the containers of the services wanted, and only the nodes
// that run one of them: there is nothing to report from the others, and
// asking them costs a round trip each.
func StatsTargets(cands []resolve.Candidate, nodes []resolve.Node, wanted func(svc string) bool) (mine []resolve.Candidate, ask []resolve.Node) {
	onNode := map[string]bool{}
	for _, cd := range cands {
		if wanted(cd.Service) {
			mine = append(mine, cd)
			onNode[cd.NodeID] = true
		}
	}
	for _, n := range nodes {
		if onNode[n.ID] {
			ask = append(ask, n)
		}
	}
	return mine, ask
}

// StatsComplete reports whether every container in mine has a full reading —
// or cannot get one by waiting, because its node's agent did not answer.
func StatsComplete(mine []resolve.Candidate, usage map[string]agentcall.ContainerUsage, answered map[string]bool) bool {
	for _, cd := range mine {
		if !answered[cd.NodeName] {
			continue
		}
		if u, ok := usage[cd.ContainerID]; !ok || !u.CPUReady {
			return false
		}
	}
	return true
}

// PollUsage asks the nodes for usage, and while a node's sampler is still
// warming up, asks again — within p.Wait, so a node that never gets there
// costs seconds, not the session. answered holds the node names that replied
// at all.
func PollUsage(ctx context.Context, ac *agentcall.Client, ask []resolve.Node, mine []resolve.Candidate, p StatsPoll) (usage map[string]agentcall.ContainerUsage, answered map[string]bool, err error) {
	usage = map[string]agentcall.ContainerUsage{}
	answered = map[string]bool{}
	deadline := time.Now().Add(p.Wait)
	for {
		u, byNode := ac.Usage(ctx, ask, p.ConnectTimeout)
		for id, cu := range u {
			usage[id] = cu
		}
		for n := range byNode {
			answered[n] = true
		}
		if len(byNode) == 0 || StatsComplete(mine, usage, answered) || !time.Now().Add(p.Interval).Before(deadline) {
			return usage, answered, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(p.Interval):
		}
	}
}

// StatsRows is one row per container: its reading, or why there is none.
// Never nil, so an empty answer encodes as [] rather than null.
func StatsRows(mine []resolve.Candidate, usage map[string]agentcall.ContainerUsage, answered map[string]bool) []StatsRow {
	rows := []StatsRow{}
	for _, cd := range mine {
		row := StatsRow{Service: cd.Service, Slot: cd.Slot, Node: orDash(cd.NodeName), Container: shortID(cd.ContainerID)}
		u, ok := usage[cd.ContainerID]
		switch {
		case ok:
			row.CPU, row.Memory, row.Health = FormatCPUUsage(u), FormatMemUsage(u), u.Health
			if !u.CPUReady {
				row.CPU, row.Note = StatsWarmingUp, StatsWarmingUpNote
			}
		case answered[cd.NodeName]:
			row.CPU, row.Memory, row.Note = StatsWarmingUp, StatsWarmingUp, StatsWarmingUpNote
		default:
			row.CPU, row.Memory, row.Note = StatsNoAgent, StatsNoAgent, StatsNoAgentNote
		}
		rows = append(rows, row)
	}
	return rows
}
