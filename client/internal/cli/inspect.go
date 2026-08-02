// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/rivo/tview"
)

// The inspect overlay shows a tabular, operator-first view by default (networks,
// labels, volumes, secrets on top; the rest ordered by relevance) plus a raw
// JSON view. Both the parsed object (for the table) and the raw daemon bytes
// (for the JSON) come from one manager inspect call.

// serviceInspectViews returns the formatted (tabular) and raw-JSON views of a
// service inspect. ref is a service name or ID.
func serviceInspectViews(ctx context.Context, dcli *client.Client, ref string) (formatted, raw string, err error) {
	svc, rawb, err := dcli.ServiceInspectWithRaw(ctx, ref, types.ServiceInspectOptions{})
	if err != nil {
		return "", "", err
	}
	return formatServiceInspect(svc, netNameMap(ctx, dcli)), prettyJSON(rawb), nil
}

// taskInspectViews returns the formatted and raw-JSON views of a task inspect —
// the manager's view of a container instance.
func taskInspectViews(ctx context.Context, dcli *client.Client, taskID string) (formatted, raw string, err error) {
	task, rawb, err := dcli.TaskInspectWithRaw(ctx, taskID)
	if err != nil {
		return "", "", err
	}
	return formatTaskInspect(task, netNameMap(ctx, dcli), nodeHostnames(ctx, dcli)), prettyJSON(rawb), nil
}

// netNameMap maps a network ID (and name) to its name so attachments that carry
// only an ID render readably. Best effort.
func netNameMap(ctx context.Context, dcli *client.Client) map[string]string {
	m := map[string]string{}
	nets, err := dcli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return m
	}
	for _, n := range nets {
		m[n.ID] = n.Name
		m[n.Name] = n.Name
	}
	return m
}

func prettyJSON(raw []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") == nil {
		return buf.String()
	}
	return string(raw)
}

// --- tabular formatting -----------------------------------------------------

func formatServiceInspect(svc swarm.Service, netNames map[string]string) string {
	var b strings.Builder
	cs := svc.Spec.TaskTemplate.ContainerSpec
	fmt.Fprintf(&b, "[aqua]SERVICE[-]  %s\n", tview.Escape(svc.Spec.Name))

	inspSection(&b, "NETWORKS")
	inspLines(&b, serviceNetworks(svc, netNames))

	inspSection(&b, "LABELS")
	inspLines(&b, kvPairs(svc.Spec.Labels))

	inspSection(&b, "VOLUMES / MOUNTS")
	inspLines(&b, specMounts(cs))

	inspSection(&b, "SECRETS")
	inspLines(&b, specSecrets(cs))
	if cfgs := specConfigs(cs); len(cfgs) > 0 {
		inspSection(&b, "CONFIGS")
		inspLines(&b, cfgs)
	}

	inspSection(&b, "PORTS")
	inspLines(&b, servicePortLines(svc))

	inspSection(&b, "IMAGE")
	inspLines(&b, imageLine(cs))

	inspSection(&b, "MODE")
	inspLines(&b, []string{serviceModeStr(svc)})

	if cs != nil && len(cs.Env) > 0 {
		inspSection(&b, "ENV")
		inspLines(&b, cs.Env)
	}
	if lines := resourceLines(svc.Spec.TaskTemplate.Resources); len(lines) > 0 {
		inspSection(&b, "RESOURCES")
		inspLines(&b, lines)
	}
	if p := svc.Spec.TaskTemplate.Placement; p != nil && len(p.Constraints) > 0 {
		inspSection(&b, "PLACEMENT")
		inspLines(&b, p.Constraints)
	}
	if uc := svc.Spec.UpdateConfig; uc != nil {
		inspSection(&b, "UPDATE POLICY")
		inspLines(&b, []string{
			fmt.Sprintf("parallelism %d", uc.Parallelism),
			fmt.Sprintf("delay %s", uc.Delay),
			fmt.Sprintf("order %s", uc.Order),
			fmt.Sprintf("on-failure %s", uc.FailureAction),
		})
	}

	inspSection(&b, "META")
	inspKV(&b, "id", svc.ID)
	inspKV(&b, "created", tstr(svc.Meta.CreatedAt))
	inspKV(&b, "updated", tstr(svc.Meta.UpdatedAt))
	return b.String()
}

func formatTaskInspect(task swarm.Task, netNames, nodeNames map[string]string) string {
	var b strings.Builder
	cs := task.Spec.ContainerSpec
	fmt.Fprintf(&b, "[aqua]TASK[-]  %s  [gray](slot %d)[-]\n", shortID(task.ID), task.Slot)

	inspSection(&b, "NETWORKS")
	inspLines(&b, taskNetworks(task, netNames))

	inspSection(&b, "LABELS")
	if cs != nil {
		inspLines(&b, kvPairs(cs.Labels))
	} else {
		inspLines(&b, nil)
	}

	inspSection(&b, "VOLUMES / MOUNTS")
	inspLines(&b, specMounts(cs))

	inspSection(&b, "SECRETS")
	inspLines(&b, specSecrets(cs))
	if cfgs := specConfigs(cs); len(cfgs) > 0 {
		inspSection(&b, "CONFIGS")
		inspLines(&b, cfgs)
	}

	inspSection(&b, "STATE")
	inspKV(&b, "state", string(task.Status.State))
	inspKV(&b, "desired", string(task.DesiredState))
	inspKV(&b, "message", task.Status.Message)
	inspKV(&b, "error", task.Status.Err)
	if st := task.Status.ContainerStatus; st != nil && st.ExitCode != 0 {
		inspKV(&b, "exit code", fmt.Sprintf("%d", st.ExitCode))
	}
	inspKV(&b, "since", tstr(task.Status.Timestamp))

	inspSection(&b, "PLACEMENT")
	node := nodeNames[task.NodeID]
	if node == "" {
		node = shortID(task.NodeID)
	}
	inspKV(&b, "node", node)
	inspKV(&b, "slot", fmt.Sprintf("%d", task.Slot))

	inspSection(&b, "CONTAINER")
	if st := task.Status.ContainerStatus; st != nil {
		inspKV(&b, "id", st.ContainerID)
		if st.PID != 0 {
			inspKV(&b, "pid", fmt.Sprintf("%d", st.PID))
		}
	}

	inspSection(&b, "IMAGE")
	inspLines(&b, imageLine(cs))

	if cs != nil && len(cs.Env) > 0 {
		inspSection(&b, "ENV")
		inspLines(&b, cs.Env)
	}
	if lines := resourceLines(task.Spec.Resources); len(lines) > 0 {
		inspSection(&b, "RESOURCES")
		inspLines(&b, lines)
	}

	inspSection(&b, "META")
	inspKV(&b, "task id", task.ID)
	inspKV(&b, "service id", task.ServiceID)
	inspKV(&b, "created", tstr(task.Meta.CreatedAt))
	return b.String()
}

// --- section helpers --------------------------------------------------------

func inspSection(b *strings.Builder, title string) { fmt.Fprintf(b, "\n[aqua]%s[-]\n", title) }

func inspKV(b *strings.Builder, k, v string) {
	if v == "" {
		return
	}
	fmt.Fprintf(b, "  %-14s %s\n", k, tview.Escape(v))
}

func inspLines(b *strings.Builder, items []string) {
	if len(items) == 0 {
		fmt.Fprintf(b, "  [gray]-[-]\n")
		return
	}
	for _, it := range items {
		fmt.Fprintf(b, "  %s\n", tview.Escape(it))
	}
}

func kvPairs(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

func serviceNetworks(svc swarm.Service, netNames map[string]string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(target string) {
		if target == "" || seen[target] {
			return
		}
		seen[target] = true
		name := netNames[target]
		if name == "" {
			name = target
		}
		out = append(out, name)
	}
	for _, a := range svc.Spec.TaskTemplate.Networks {
		add(a.Target)
	}
	for _, a := range svc.Spec.Networks {
		add(a.Target)
	}
	return out
}

func taskNetworks(task swarm.Task, netNames map[string]string) []string {
	var out []string
	for _, a := range task.NetworksAttachments {
		name := a.Network.Spec.Name
		if name == "" {
			if n := netNames[a.Network.ID]; n != "" {
				name = n
			} else {
				name = shortID(a.Network.ID)
			}
		}
		if ips := strings.Join(a.Addresses, ", "); ips != "" {
			out = append(out, fmt.Sprintf("%s  %s", name, ips))
		} else {
			out = append(out, name)
		}
	}
	return out
}

func specMounts(cs *swarm.ContainerSpec) []string {
	if cs == nil {
		return nil
	}
	var out []string
	for _, m := range cs.Mounts {
		line := fmt.Sprintf("%s -> %s", orDash(m.Source), m.Target)
		if m.Type != mount.TypeVolume {
			line += fmt.Sprintf(" (%s)", m.Type)
		}
		if m.ReadOnly {
			line += " [ro]"
		}
		out = append(out, line)
	}
	return out
}

func specSecrets(cs *swarm.ContainerSpec) []string {
	if cs == nil {
		return nil
	}
	var out []string
	for _, s := range cs.Secrets {
		out = append(out, s.SecretName)
	}
	return out
}

func specConfigs(cs *swarm.ContainerSpec) []string {
	if cs == nil {
		return nil
	}
	var out []string
	for _, c := range cs.Configs {
		out = append(out, c.ConfigName)
	}
	return out
}

func imageLine(cs *swarm.ContainerSpec) []string {
	if cs == nil || cs.Image == "" {
		return nil
	}
	return []string{cs.Image}
}

func servicePortLines(svc swarm.Service) []string {
	ports := svc.Endpoint.Ports
	if len(ports) == 0 && svc.Spec.EndpointSpec != nil {
		ports = svc.Spec.EndpointSpec.Ports
	}
	var out []string
	for _, p := range ports {
		out = append(out, fmt.Sprintf("%d -> %d/%s (%s)", p.PublishedPort, p.TargetPort, p.Protocol, p.PublishMode))
	}
	return out
}

func serviceModeStr(svc swarm.Service) string {
	if r := svc.Spec.Mode.Replicated; r != nil && r.Replicas != nil {
		return fmt.Sprintf("replicated (%d replicas)", *r.Replicas)
	}
	if svc.Spec.Mode.Global != nil {
		return "global"
	}
	return "-"
}

func resourceLines(rr *swarm.ResourceRequirements) []string {
	if rr == nil {
		return nil
	}
	var out []string
	if rr.Limits != nil {
		if s := resStr(rr.Limits.NanoCPUs, rr.Limits.MemoryBytes); s != "" {
			out = append(out, "limits: "+s)
		}
	}
	if rr.Reservations != nil {
		if s := resStr(rr.Reservations.NanoCPUs, rr.Reservations.MemoryBytes); s != "" {
			out = append(out, "reservations: "+s)
		}
	}
	return out
}

func resStr(nanoCPU, memBytes int64) string {
	var parts []string
	if nanoCPU > 0 {
		parts = append(parts, fmt.Sprintf("%.2f CPU", float64(nanoCPU)/1e9))
	}
	if memBytes > 0 {
		parts = append(parts, humanBytes(memBytes))
	}
	return strings.Join(parts, ", ")
}

func tstr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
