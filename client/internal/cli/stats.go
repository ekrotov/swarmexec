// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dial"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// Live resource usage is node-local: the manager API knows what the scheduler
// BOOKED against a node, not what is actually being used. So it comes from the
// agents — one Stats call per node, answered from that agent's background
// sampler — and is keyed back to containers here.

// containerUsage is one container's live resource use in the shape the views
// need: absolute numbers to show, and a ratio to colour by.
type containerUsage struct {
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

// Health verdicts, matching docker's vocabulary and the agent's.
const (
	healthHealthy   = "healthy"
	healthUnhealthy = "unhealthy"
	healthStarting  = "starting"
)

// nodeUsage is one node's totals, for putting live use next to what the
// scheduler booked.
type nodeUsage struct {
	CPUPercent float64 // summed over the node's containers, share of ONE cpu
	CPUReady   bool
	MemBytes   int64
	NodeCPUs   int64
	NodeMem    int64
	SampledAt  string
}

// usageLevel classifies a ratio for colouring. The thresholds are deliberately
// the ones an operator already thinks in.
type usageLevel int

const (
	usageOK usageLevel = iota
	usageWarn
	usageCrit
)

const (
	usageWarnAt = 0.70
	usageCritAt = 0.90
)

func levelOf(ratio float64) usageLevel {
	switch {
	case ratio >= usageCritAt:
		return usageCrit
	case ratio >= usageWarnAt:
		return usageWarn
	default:
		return usageOK
	}
}

// worst returns the more severe of two levels — a container is as alarming as
// its worst resource, so cpu and memory fold together this way, and so does a
// service over its containers.
func worst(a, b usageLevel) usageLevel {
	if b > a {
		return b
	}
	return a
}

// level is the container's overall level: the worse of cpu and memory, ignoring
// a CPU reading that is not ready yet.
func (u containerUsage) level() usageLevel {
	lvl := levelOf(u.MemRatio)
	if u.CPUReady {
		lvl = worst(lvl, levelOf(u.CPURatio))
	}
	return lvl
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

// statsGate remembers which node agents cannot serve stats, so a cluster that
// has not been upgraded yet does not pay a timeout per node on every refresh.
type statsGate struct {
	mu   sync.Mutex
	skip map[string]time.Time // node name -> do not ask again before this
}

func newStatsGate() *statsGate { return &statsGate{skip: map[string]time.Time{}} }

func (g *statsGate) allowed(node string) bool {
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
func (g *statsGate) fail(node string, err error) {
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

func (g *statsGate) ok(node string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.skip, node)
	g.mu.Unlock()
}

// collectUsage asks every node's agent for its container usage and returns it
// keyed by container id, plus per-node totals keyed by node name.
//
// Best effort throughout: a node whose agent is unreachable, too old to know
// the RPC, or still warming up simply contributes nothing. Usage is an overlay
// on the view, never a reason for it to fail.
func collectUsage(ctx context.Context, cfg config.Config, nodes []resolve.Node, connectTimeout time.Duration, gate *statsGate) (map[string]containerUsage, map[string]nodeUsage) {
	type result struct {
		node string
		resp *pb.StatsResponse
	}
	results := make([]result, len(nodes))
	forEachNode(nodes, func(i int, n resolve.Node) {
		if !gate.allowed(n.Name) {
			return
		}
		resp, err := fetchNodeStats(ctx, cfg, n, connectTimeout)
		if err != nil {
			gate.fail(n.Name, err)
			return
		}
		gate.ok(n.Name)
		results[i] = result{node: n.Name, resp: resp}
	})

	byContainer := map[string]containerUsage{}
	byNode := map[string]nodeUsage{}
	for _, r := range results {
		if r.resp == nil {
			continue
		}
		nu := nodeUsage{
			CPUReady:  r.resp.GetCpuReady(),
			NodeCPUs:  r.resp.GetNodeCpus(),
			NodeMem:   r.resp.GetNodeMemoryTotalBytes(),
			SampledAt: r.resp.GetSampledAt(),
		}
		for _, s := range r.resp.GetStats() {
			u := usageOf(s, r.resp)
			byContainer[s.GetContainerId()] = u
			nu.CPUPercent += u.CPUPercent
			nu.MemBytes += u.MemBytes
		}
		byNode[r.node] = nu
	}
	return byContainer, byNode
}

// usageOf turns one wire reading into the shape the views use, resolving each
// ratio against the right denominator.
func usageOf(s *pb.ContainerStats, resp *pb.StatsResponse) containerUsage {
	u := containerUsage{
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

func fetchNodeStats(ctx context.Context, cfg config.Config, n resolve.Node, connectTimeout time.Duration) (*pb.StatsResponse, error) {
	// Deliberately NOT max(connectTimeout, …): stats are optional, and a long
	// connect timeout configured for interactive work should not turn a silent
	// node into a multi-second stall on every refresh.
	timeout := statsTimeout
	if connectTimeout > 0 && connectTimeout < timeout {
		timeout = connectTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dial.Dial(cctx, n.DialHost, cfg.Port, cfg)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// No container_ids filter: the tree wants every container on the node
	// anyway, and the agent is answering from memory either way.
	resp, err := pb.NewAgentClient(conn).Stats(cctx, &pb.StatsRequest{})
	if err != nil {
		return nil, wrapGRPC(err)
	}
	return resp, nil
}

// formatCPUUsage renders a CPU reading as "used of allowed", because the
// percentage on its own is not readable: it tells you nothing about the scale
// it is measured against.
func formatCPUUsage(u containerUsage) string {
	switch {
	case !u.CPUReady:
		return "…"
	case u.CPULimitCores > 0:
		return fmt.Sprintf("%.2f / %.2f cores (%s)", u.CPUPercent/100, u.CPULimitCores, formatRatio(u.CPURatio))
	default:
		return fmt.Sprintf("%.2f cores", u.CPUPercent/100)
	}
}

func formatMemUsage(u containerUsage) string {
	if u.MemLimitBytes <= 0 {
		return humanBytes(u.MemBytes)
	}
	return fmt.Sprintf("%s / %s (%s)", humanBytes(u.MemBytes), humanBytes(u.MemLimitBytes), formatRatio(u.MemRatio))
}

func formatRatio(r float64) string { return fmt.Sprintf("%.0f%%", r*100) }

// Resource markers. These sit at the END of a row, after the padded columns, so
// their colour tags cannot disturb the alignment — the same place the
// rolling-update badge lives.
//
// Glyphs rather than words because the row is already dense; the percentage
// next to them carries the actual meaning, and the colour carries the urgency.
const (
	cpuGlyph = "⚡"
	memGlyph = "▣"
)

// usageColor maps a level to a tview colour tag. usageOK has none — a row that
// is fine gets no marker at all, or the badges would be wallpaper rather than a
// signal.
func usageColor(l usageLevel) string {
	switch l {
	case usageCrit:
		return "red"
	case usageWarn:
		return "orange"
	default:
		return ""
	}
}

// usageBadge renders the markers for one container: one per resource that has
// crossed the warning threshold, each with its own colour. Empty below the
// threshold, and empty for CPU while the agent has only taken one reading —
// "not measured yet" must not look like "fine".
func usageBadge(u containerUsage) string {
	var b strings.Builder
	if u.CPUReady {
		if c := usageColor(levelOf(u.CPURatio)); c != "" {
			fmt.Fprintf(&b, "  [%s]%s %s[-]", c, cpuGlyph, formatRatio(u.CPURatio))
		}
	}
	if c := usageColor(levelOf(u.MemRatio)); c != "" {
		fmt.Fprintf(&b, "  [%s]%s %s[-]", c, memGlyph, formatRatio(u.MemRatio))
	}
	return b.String()
}

// serviceUsage folds a service's containers into one reading for its row: the
// highest ratio per resource, not the sum or the average. A service is as
// stressed as its worst replica — averaging would hide exactly the one about to
// be OOM-killed, and summing would invent a number that is no container's.
func serviceUsage(usage map[string]containerUsage, cands []resolve.Candidate, service string) (containerUsage, bool) {
	var out containerUsage
	found := false
	for _, c := range cands {
		if c.Service != service {
			continue
		}
		u, ok := usage[c.ContainerID]
		if !ok {
			continue
		}
		found = true
		if u.CPUReady && u.CPURatio > out.CPURatio {
			out.CPURatio, out.CPUPercent, out.CPULimited = u.CPURatio, u.CPUPercent, u.CPULimited
		}
		out.CPUReady = out.CPUReady || u.CPUReady
		if u.MemRatio > out.MemRatio {
			out.MemRatio, out.MemLimited, out.MemLimitBytes = u.MemRatio, u.MemLimited, u.MemLimitBytes
		}
		// Memory in absolute terms is the one figure that does add up across
		// replicas — that is what the service is costing the cluster.
		out.MemBytes += u.MemBytes
	}
	return out, found
}

// serviceHealth counts the healthcheck verdicts across a service's containers.
// unknown covers both "declares no healthcheck" and "no reading from the node";
// neither entitles the view to claim anything.
type serviceHealth struct {
	Healthy, Unhealthy, Starting, Unknown int
}

// Total is how many containers were looked at.
func (h serviceHealth) Total() int { return h.Healthy + h.Unhealthy + h.Starting + h.Unknown }

// Checked is how many actually report a verdict — the denominator that makes
// "1 unhealthy" mean something.
func (h serviceHealth) Checked() int { return h.Healthy + h.Unhealthy + h.Starting }

func healthOf(usage map[string]containerUsage, cands []resolve.Candidate, service string) serviceHealth {
	var h serviceHealth
	for _, c := range cands {
		if c.Service != service {
			continue
		}
		switch usage[c.ContainerID].Health {
		case healthHealthy:
			h.Healthy++
		case healthUnhealthy:
			h.Unhealthy++
		case healthStarting:
			h.Starting++
		default:
			h.Unknown++
		}
	}
	return h
}

// healthBadge names a failing healthcheck on a row. Healthy and un-checked
// containers get nothing: the badge exists to contradict the running/desired
// count, and a badge on every row would stop doing that.
func healthBadge(h serviceHealth) string {
	switch {
	case h.Unhealthy > 0:
		return fmt.Sprintf("  [red]✖ %d unhealthy[-]", h.Unhealthy)
	case h.Starting > 0:
		return fmt.Sprintf("  [yellow]◌ %d starting[-]", h.Starting)
	default:
		return ""
	}
}

// containerHealthBadge is the same marker for a single container leaf.
func containerHealthBadge(health string) string {
	switch health {
	case healthUnhealthy:
		return "  [red]✖ unhealthy[-]"
	case healthStarting:
		return "  [yellow]◌ starting[-]"
	default:
		return ""
	}
}

// healthColor adjusts a service row's colour for what the healthchecks say.
//
// This is the point of the whole feature: the row was coloured by running vs
// desired alone, so a service whose every container fails its probe still read
// as a calm "3/3" in aqua. A failing probe is degradation of the same kind as a
// missing replica, so it is coloured the same way — every container failing is
// as bad as none running.
func healthColor(base tcell.Color, h serviceHealth) tcell.Color {
	switch {
	case h.Unhealthy == 0:
		return base
	case h.Unhealthy >= h.Checked():
		return tcell.ColorRed
	default:
		return tcell.ColorOrange
	}
}
