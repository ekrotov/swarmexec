// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/container"
)

// ContainerInspectViews turns the node-local `docker container inspect`
// document (agent RPC InspectContainer, CONTRACT.md §3.10) into the inspect
// overlay's two views: an operator-first summary of what only the node knows —
// runtime state, the last health probes, the container's own network
// endpoints and mounts — and the raw JSON.
//
// The environment is left to the raw view on purpose: the summary is what an
// operator scrolls past in front of others.
func ContainerInspectViews(raw []byte) ([]InspectLine, string, error) {
	var c container.InspectResponse
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, "", fmt.Errorf("container inspect: %w", err)
	}
	return formatContainerInspect(c), prettyJSON(raw), nil
}

func formatContainerInspect(c container.InspectResponse) []InspectLine {
	var b inspBuilder
	b.title(fmt.Sprintf("CONTAINER  %s  %s", shortID(c.ID), strings.TrimPrefix(c.Name, "/")))

	b.section("STATE")
	containerStateLines(&b, c)
	if c.State != nil && c.State.Health != nil {
		b.section("HEALTH")
		containerHealthLines(&b, c.State.Health)
	}
	b.section("NETWORKS")
	b.list(containerNetworkLines(c.NetworkSettings))
	b.section("VOLUMES / MOUNTS")
	b.list(containerMountLines(c.Mounts))
	b.section("PROCESS")
	containerProcessLines(&b, c)
	return b.lines
}

func containerStateLines(b *inspBuilder, c container.InspectResponse) {
	st := c.State
	if st == nil {
		b.list(nil)
		return
	}
	b.kv("status", string(st.Status))
	if st.Pid != 0 {
		b.kv("pid", fmt.Sprintf("%d", st.Pid))
	}
	if !st.Running {
		b.kv("exit code", fmt.Sprintf("%d", st.ExitCode))
	}
	if st.OOMKilled {
		b.kv("oom killed", "yes")
	}
	b.kv("error", st.Error)
	b.kv("started", st.StartedAt)
	if !st.Running {
		b.kv("finished", st.FinishedAt)
	}
	b.kv("restarts", fmt.Sprintf("%d", c.RestartCount))
}

// containerHealthLines shows the verdict and the most recent probes, newest
// first, with their output on one line each — the probe output is what the
// manager never shows and what "why is it unhealthy" is answered by.
func containerHealthLines(b *inspBuilder, h *container.Health) {
	b.kv("status", string(h.Status))
	if h.FailingStreak > 0 {
		b.kv("failing streak", fmt.Sprintf("%d", h.FailingStreak))
	}
	probes := slices.Clone(h.Log)
	slices.Reverse(probes)
	for i, p := range probes {
		if i == 3 || p == nil {
			break
		}
		out := strings.Join(strings.Fields(p.Output), " ")
		if len(out) > 160 {
			out = out[:160] + "…"
		}
		b.kv(fmt.Sprintf("probe exit %d", p.ExitCode), strings.TrimSpace(p.End.UTC().Format("2006-01-02 15:04:05")+"  "+out))
	}
}

func containerNetworkLines(ns *container.NetworkSettings) []string {
	if ns == nil {
		return nil
	}
	var out []string
	for name, ep := range ns.Networks {
		if ep == nil {
			continue
		}
		line := name
		if ep.IPAddress.IsValid() {
			line += fmt.Sprintf("  %s/%d", ep.IPAddress, ep.IPPrefixLen)
		}
		if len(ep.MacAddress) > 0 {
			line += "  mac " + ep.MacAddress.String()
		}
		if len(ep.Aliases) > 0 {
			line += "  aliases " + strings.Join(ep.Aliases, ",")
		}
		out = append(out, line)
	}
	slices.Sort(out)
	return out
}

func containerMountLines(mounts []container.MountPoint) []string {
	var out []string
	for _, m := range mounts {
		src := m.Source
		if m.Name != "" {
			src = m.Name
		}
		mode := "ro"
		if m.RW {
			mode = "rw"
		}
		out = append(out, fmt.Sprintf("%s %s -> %s (%s)", m.Type, src, m.Destination, mode))
	}
	return out
}

func containerProcessLines(b *inspBuilder, c container.InspectResponse) {
	if cfg := c.Config; cfg != nil {
		b.kv("image", cfg.Image)
		b.kv("entrypoint", strings.Join(cfg.Entrypoint, " "))
		b.kv("cmd", strings.Join(cfg.Cmd, " "))
		b.kv("user", cfg.User)
		b.kv("workdir", cfg.WorkingDir)
		b.kv("hostname", cfg.Hostname)
	}
	b.kv("created", c.Created)
	b.kv("id", c.ID)
}
