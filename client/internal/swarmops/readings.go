// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"fmt"
	"time"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/resolve"
)

// The readings both front-ends show the same way — the TUI and the MCP
// server: a container's usage against its limits, a service's health tally,
// an agent's version verdict, the topology read.

// ClassifyNode turns a node's version-probe result into a display status.
func ClassifyNode(r agentcall.NodeVersion, clientBinary, clientProto string) (status, version, proto string, ok bool) {
	switch {
	case r.Err != nil && agenterr.TooOld(r.Err):
		return "too old (init --force)", "-", "-", false
	case r.Err != nil:
		return "unreachable", "-", "-", false
	case r.Proto != clientProto:
		return "PROTO MISMATCH", r.Version, r.Proto, false
	case r.Version != clientBinary && clientBinary != "" && clientBinary != "dev":
		return "ok (version skew)", r.Version, r.Proto, true
	default:
		return "ok", r.Version, r.Proto, true
	}
}

// FormatCPUUsage renders a CPU reading as "used of allowed", because the
// percentage on its own is not readable: it tells you nothing about the scale
// it is measured against.
func FormatCPUUsage(u agentcall.ContainerUsage) string {
	switch {
	case !u.CPUReady:
		return "…"
	case u.CPULimitCores > 0:
		return fmt.Sprintf("%.2f / %.2f cores (%s)", u.CPUPercent/100, u.CPULimitCores, FormatRatio(u.CPURatio))
	default:
		return fmt.Sprintf("%.2f cores", u.CPUPercent/100)
	}
}

// FormatMemUsage renders memory as "used of allowed". It uses the binary units
// FormatMemBytes gives (MiB/GiB), not SI: cgroup limits are binary, docker
// reports binary, and the node detail already shows the same figures that way —
// the same number must not read differently in two places.
func FormatMemUsage(u agentcall.ContainerUsage) string {
	used := FormatMemBytes(u.MemBytes)
	if used == "" {
		used = "0B" // FormatMemBytes yields "" at zero; a table cell needs a value
	}
	if u.MemLimitBytes <= 0 {
		return used
	}
	return fmt.Sprintf("%s / %s (%s)", used, FormatMemBytes(u.MemLimitBytes), FormatRatio(u.MemRatio))
}

// FormatRatio renders a 0..1 ratio as a whole percentage.
func FormatRatio(r float64) string { return fmt.Sprintf("%.0f%%", r*100) }

// ServiceHealth counts the healthcheck verdicts across a service's containers.
// unknown covers both "declares no healthcheck" and "no reading from the node";
// neither entitles the view to claim anything.
type ServiceHealth struct {
	Healthy, Unhealthy, Starting, Unknown int
}

// Total is how many containers were looked at.
func (h ServiceHealth) Total() int { return h.Healthy + h.Unhealthy + h.Starting + h.Unknown }

// Checked is how many actually report a verdict — the denominator that makes
// "1 unhealthy" mean something.
func (h ServiceHealth) Checked() int { return h.Healthy + h.Unhealthy + h.Starting }

// HealthOf tallies the healthcheck verdicts of service's running containers
// (cands), as the nodes reported them in usage.
func HealthOf(usage map[string]agentcall.ContainerUsage, cands []resolve.Candidate, service string) ServiceHealth {
	var h ServiceHealth
	for _, c := range cands {
		if c.Service != service {
			continue
		}
		switch usage[c.ContainerID].Health {
		case HealthHealthy:
			h.Healthy++
		case HealthUnhealthy:
			h.Unhealthy++
		case HealthStarting:
			h.Starting++
		default:
			h.Unknown++
		}
	}
	return h
}

// ServicesAndContainers is the topology read the tree and the MCP server
// share: every service with its counts, and every running container. op
// prefixes the timing log lines so a slow manager shows up under whoever
// asked.
func ServicesAndContainers(ctx context.Context, r *resolve.Resolver, op string) ([]resolve.Service, []resolve.Candidate, error) {
	start := time.Now()
	svcs, err := r.ServicesWithRisks(ctx)
	clientlog.Timed(op+".Services", start, err)
	if err != nil {
		return nil, nil, err
	}
	// Fetch every running container (not just the CLI-arg service); the tree
	// filters client-side so services with 0 containers still appear.
	start = time.Now()
	cands, err := r.Candidates(ctx, "")
	clientlog.Timed(op+".Candidates", start, err)
	if err != nil {
		return nil, nil, err
	}
	return svcs, cands, nil
}

// Health verdicts, matching docker's vocabulary and the agent's.
const (
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
	HealthStarting  = "starting"
)

// FormatUptime renders how long a container has run, compactly ("45s",
// "12m", "3h5m", "3d4h"), or "-" when unknown.
func FormatUptime(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
