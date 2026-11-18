// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/clusterread"
	"swarmexec/client/internal/resolve"
)

// Live resource usage is node-local: the manager API knows what the scheduler
// BOOKED against a node, not what is actually being used. So it comes from the
// agents — one Stats call per node, answered from that agent's background
// sampler — and is keyed back to containers here.

// containerUsage and nodeUsage are the agentcall shapes under the names the
// views use.
type (
	containerUsage = agentcall.ContainerUsage
	nodeUsage      = agentcall.NodeUsage
)

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

// Resource markers. These sit at the END of a row, after the padded columns, so
// their colour tags cannot disturb the alignment — the same place the
// rolling-update badge lives.
//
// Words, not glyphs. These were ⚡ and ▣, which cost the same width and read as
// neither cpu nor memory to anyone who had not been told — and a marker whose
// meaning has to be looked up is not doing its job. "cpu"/"mem" also cannot
// fail to render, which no emoji can promise across terminals and fonts.
const (
	cpuGlyph = "cpu"
	memGlyph = "mem"
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
			fmt.Fprintf(&b, "  [%s]%s %s[-]", c, cpuGlyph, clusterread.FormatRatio(u.CPURatio))
		}
	}
	if c := usageColor(levelOf(u.MemRatio)); c != "" {
		fmt.Fprintf(&b, "  [%s]%s %s[-]", c, memGlyph, clusterread.FormatRatio(u.MemRatio))
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

// healthBadge names a failing healthcheck on a row. Healthy and un-checked
// containers get nothing: the badge exists to contradict the running/desired
// count, and a badge on every row would stop doing that.
func healthBadge(h clusterread.ServiceHealth) string {
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
	case clusterread.HealthUnhealthy:
		return "  [red]✖ unhealthy[-]"
	case clusterread.HealthStarting:
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
func healthColor(base tcell.Color, h clusterread.ServiceHealth) tcell.Color {
	switch {
	case h.Unhealthy == 0:
		return base
	case h.Unhealthy >= h.Checked():
		return tcell.ColorRed
	default:
		return tcell.ColorOrange
	}
}
