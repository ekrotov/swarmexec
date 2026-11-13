// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/fanout"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// Live resource usage is node-local: the manager API knows what the scheduler
// BOOKED against a node, not what is actually being used. So it comes from the
// agents — one Stats call per node, answered from that agent's background
// sampler — and is keyed back to containers here.

// ContainerUsage is one container's live resource use in the shape the views
// need: absolute numbers to show, and a ratio to colour by.
type ContainerUsage struct {
	// CPUPercent is docker's own number — the share of ONE cpu, so 250 means
	// two and a half cores. CPUReady is false while the agent has only taken one
	// reading (a percentage is a delta), which is not the same as 0%.
	CPUPercent float64
	CPUReady   bool

	MemBytes int64

	// CPURatio and MemRatio are 0..1 of what this container may actually use:
	// its own limit when it has one, the node's capacity when it does not. The
	// distinction matters — 90% of a 256 MB limit is about to be OOM-killed,
	// 90% of a 64 GB node is a different conversation — so the *Limited flags
	// say which denominator was used.
	CPURatio   float64
	MemRatio   float64
	CPULimited bool
	MemLimited bool

	// The denominators behind the two ratios, kept so a view can show what the
	// percentage is a percentage OF. A bare "94%" is unreadable to anyone who
	// does not already know how this tool measures.
	CPULimitCores float64
	MemLimitBytes int64

	// Health is the container's healthcheck verdict, or "" when it declares
	// none. It comes from the node because the manager cannot know it: a swarm
	// task reads "running" while its container fails every probe, which is
	// exactly the case the tree used to render as a confident "3/3".
	Health string
}

// NodeUsage is one node's totals, for putting live use next to what the
// scheduler booked.
type NodeUsage struct {
	CPUPercent float64 // summed over the node's containers, share of ONE cpu
	CPUReady   bool
	MemBytes   int64
	NodeCPUs   int64
	NodeMem    int64
	SampledAt  string
}

// statsTimeout bounds one node's Stats call. The agent answers from memory, so
// this only has to cover the dial and the round trip — and usage is decoration
// on the view, never something worth making the operator wait for.
const statsTimeout = 4 * time.Second

// How long to leave a node alone after it fails to serve stats. The two cases
// are genuinely different: an agent that predates the RPC will keep saying so
// until someone redeploys it, and asking it every refresh forever is pure
// waste — measured at a full timeout per node per cycle before this existed.
const (
	statsRetryAfter       = 45 * time.Second
	statsUnsupportedAfter = 15 * time.Minute
)

// StatsGate remembers which node agents cannot serve stats, so a cluster that
// has not been upgraded yet does not pay a timeout per node on every refresh.
// A nil gate allows everything.
type StatsGate struct {
	mu   sync.Mutex
	skip map[string]time.Time // node name -> do not ask again before this
}

// NewStatsGate returns an empty gate.
func NewStatsGate() *StatsGate { return &StatsGate{skip: map[string]time.Time{}} }

func (g *StatsGate) allowed(node string) bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.skip[node]
	return !ok || time.Now().After(until)
}

// fail puts a node on the bench. An Unimplemented answer means the agent is too
// old and will not change on its own, so it waits much longer than a node that
// was merely unreachable.
func (g *StatsGate) fail(node string, err error) {
	if g == nil {
		return
	}
	wait := statsRetryAfter
	if status.Code(err) == codes.Unimplemented {
		wait = statsUnsupportedAfter
	}
	g.mu.Lock()
	g.skip[node] = time.Now().Add(wait)
	g.mu.Unlock()
}

func (g *StatsGate) ok(node string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.skip, node)
	g.mu.Unlock()
}

// Usage asks every node's agent for its container usage and returns it keyed
// by container id, plus per-node totals keyed by node name.
//
// Best effort throughout: a node whose agent is unreachable, too old to know
// the RPC, or still warming up simply contributes nothing. Usage is an overlay
// on the view, never a reason for it to fail.
func (c *Client) Usage(ctx context.Context, nodes []resolve.Node, connectTimeout time.Duration, gate *StatsGate) (map[string]ContainerUsage, map[string]NodeUsage) {
	type result struct {
		node string
		resp *pb.StatsResponse
	}
	results := make([]result, len(nodes))
	// Best effort: a node not reached before ctx ended contributes nothing,
	// exactly like an unreachable one.
	_ = fanout.Nodes(ctx, nodes, func(ctx context.Context, i int, n resolve.Node) {
		if !gate.allowed(n.Name) {
			return
		}
		resp, err := c.Stats(ctx, n, connectTimeout)
		if err != nil {
			gate.fail(n.Name, err)
			return
		}
		gate.ok(n.Name)
		results[i] = result{node: n.Name, resp: resp}
	})

	byContainer := map[string]ContainerUsage{}
	byNode := map[string]NodeUsage{}
	for _, r := range results {
		if r.resp == nil {
			continue
		}
		nu := NodeUsage{
			CPUReady:  r.resp.GetCpuReady(),
			NodeCPUs:  r.resp.GetNodeCpus(),
			NodeMem:   r.resp.GetNodeMemoryTotalBytes(),
			SampledAt: r.resp.GetSampledAt(),
		}
		for _, s := range r.resp.GetStats() {
			u := UsageOf(s, r.resp)
			byContainer[s.GetContainerId()] = u
			nu.CPUPercent += u.CPUPercent
			nu.MemBytes += u.MemBytes
		}
		byNode[r.node] = nu
	}
	return byContainer, byNode
}

// UsageOf turns one wire reading into the shape the views use, resolving each
// ratio against the right denominator.
func UsageOf(s *pb.ContainerStats, resp *pb.StatsResponse) ContainerUsage {
	u := ContainerUsage{
		CPUPercent:    s.GetCpuPercent(),
		CPUReady:      resp.GetCpuReady(),
		MemBytes:      s.GetMemoryBytes(),
		MemLimited:    s.GetMemoryLimited(),
		MemLimitBytes: s.GetMemoryLimitBytes(),
		Health:        s.GetHealth(),
	}
	// CPU: against the container's own limit when it has one, otherwise against
	// the whole node — which is the real ceiling for an unlimited container.
	switch cores := s.GetCpuLimitCores(); {
	case cores > 0:
		u.CPULimitCores, u.CPULimited = cores, true
	default:
		u.CPULimitCores = float64(resp.GetNodeCpus())
	}
	if u.CPULimitCores > 0 {
		u.CPURatio = u.CPUPercent / (u.CPULimitCores * 100)
	}
	if u.MemLimitBytes > 0 {
		u.MemRatio = float64(u.MemBytes) / float64(u.MemLimitBytes)
	}
	return u
}

// Stats is one node's Stats call.
func (c *Client) Stats(ctx context.Context, n resolve.Node, connectTimeout time.Duration) (*pb.StatsResponse, error) {
	// Deliberately NOT max(connectTimeout, …): stats are optional, and a long
	// connect timeout configured for interactive work should not turn a silent
	// node into a multi-second stall on every refresh.
	timeout := statsTimeout
	if connectTimeout > 0 && connectTimeout < timeout {
		timeout = connectTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := c.Conn(cctx, n.DialHost)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// No container_ids filter: the tree wants every container on the node
	// anyway, and the agent is answering from memory either way.
	resp, err := conn.Agent().Stats(cctx, &pb.StatsRequest{})
	if err != nil {
		return nil, c.Explain(ctx, err)
	}
	return resp, nil
}
