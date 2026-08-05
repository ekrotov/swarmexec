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
)

// The inspect overlay shows a tabular, operator-first view by default (networks,
// labels, volumes, secrets on top; the rest ordered by relevance) plus a raw
// JSON view. The tabular view is emitted as structured lines so the overlay can
// colour them, make data lines individually selectable, and copy one. Both views
// come from one manager inspect call.

// inspKind classifies a formatted inspect line for display and selectability.
type inspKind int

const (
	inspTitle  inspKind = iota // the header line (service/task identity)
	inspHeader                 // a section header
	inspField                  // a data line — selectable/copyable
	inspDim                    // a placeholder ("-") — not selectable
	inspBlank                  // spacer
	inspNet                    // a collapsible network row: "+ name (N dns names)"
)

// inspLine is one line of the tabular inspect view. For inspNet rows, Net is the
// network name (collapse key), Count the number of DNS names, and Children the
// lines revealed when expanded (DNS names, then any extra like addresses).
type inspLine struct {
	Text     string
	Kind     inspKind
	Net      string
	Count    int
	Children []string
}

type inspBuilder struct{ lines []inspLine }

func (b *inspBuilder) push(k inspKind, s string) {
	b.lines = append(b.lines, inspLine{Text: s, Kind: k})
}
func (b *inspBuilder) title(s string)   { b.push(inspTitle, s) }
func (b *inspBuilder) section(s string) { b.push(inspBlank, ""); b.push(inspHeader, s) }

// net adds a collapsible network row. dnsCount is shown in the header; children
// are the lines shown when expanded.
func (b *inspBuilder) net(name string, dnsCount int, children []string) {
	b.lines = append(b.lines, inspLine{Kind: inspNet, Text: name, Net: name, Count: dnsCount, Children: children})
}

func (b *inspBuilder) kv(k, v string) {
	if v == "" {
		return
	}
	b.push(inspField, fmt.Sprintf("  %-14s %s", k, v))
}

func (b *inspBuilder) list(items []string) {
	if len(items) == 0 {
		b.push(inspDim, "  -")
		return
	}
	for _, it := range items {
		b.push(inspField, "  "+it)
	}
}

// serviceInspectViews returns the formatted (tabular) lines and raw-JSON view of
// a service inspect. ref is a service name or ID.
func serviceInspectViews(ctx context.Context, dcli *client.Client, ref string) ([]inspLine, string, error) {
	svc, rawb, err := dcli.ServiceInspectWithRaw(ctx, ref, types.ServiceInspectOptions{})
	if err != nil {
		return nil, "", err
	}
	return formatServiceInspect(svc, netNameMap(ctx, dcli)), prettyJSON(rawb), nil
}

// taskInspectViews returns the formatted lines and raw-JSON view of a task
// inspect — the manager's view of a container instance.
func taskInspectViews(ctx context.Context, dcli *client.Client, taskID string) ([]inspLine, string, error) {
	task, rawb, err := dcli.TaskInspectWithRaw(ctx, taskID)
	if err != nil {
		return nil, "", err
	}
	// The owning service carries the DNS name and per-network aliases the task
	// inherits; fetch it best-effort so the task's NETWORKS can show them too.
	var owning *swarm.Service
	if task.ServiceID != "" {
		if s, _, e := dcli.ServiceInspectWithRaw(ctx, task.ServiceID, types.ServiceInspectOptions{}); e == nil {
			owning = &s
		}
	}
	return formatTaskInspect(task, owning, netNameMap(ctx, dcli), nodeHostnames(ctx, dcli)), prettyJSON(rawb), nil
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

func formatServiceInspect(svc swarm.Service, netNames map[string]string) []inspLine {
	var b inspBuilder
	cs := svc.Spec.TaskTemplate.ContainerSpec
	b.title("SERVICE  " + svc.Spec.Name)

	b.section("NETWORKS")
	nets := serviceNetDNS(svc, netNames)
	if len(nets) == 0 {
		b.list(nil)
	}
	for _, n := range nets {
		b.net(n.Name, len(n.DNS), append(append([]string{}, n.DNS...), n.Extra...))
	}
	b.section("LABELS")
	b.list(kvPairs(svc.Spec.Labels))
	b.section("VOLUMES / MOUNTS")
	b.list(specMounts(cs))
	b.section("SECRETS")
	b.list(specSecrets(cs))
	if cfgs := specConfigs(cs); len(cfgs) > 0 {
		b.section("CONFIGS")
		b.list(cfgs)
	}
	b.section("PORTS")
	b.list(servicePortLines(svc))
	b.section("IMAGE")
	b.list(imageLine(cs))
	b.section("MODE")
	b.list([]string{serviceModeStr(svc)})
	if cs != nil && len(cs.Env) > 0 {
		b.section("ENV")
		b.list(cs.Env)
	}
	if lines := resourceLines(svc.Spec.TaskTemplate.Resources); len(lines) > 0 {
		b.section("RESOURCES")
		b.list(lines)
	}
	if p := svc.Spec.TaskTemplate.Placement; p != nil && len(p.Constraints) > 0 {
		b.section("PLACEMENT")
		b.list(p.Constraints)
	}
	if uc := svc.Spec.UpdateConfig; uc != nil {
		b.section("UPDATE POLICY")
		b.list([]string{
			fmt.Sprintf("parallelism %d", uc.Parallelism),
			fmt.Sprintf("delay %s", uc.Delay),
			fmt.Sprintf("order %s", uc.Order),
			fmt.Sprintf("on-failure %s", uc.FailureAction),
		})
	}
	b.section("META")
	b.kv("id", svc.ID)
	b.kv("created", tstr(svc.Meta.CreatedAt))
	b.kv("updated", tstr(svc.Meta.UpdatedAt))
	return b.lines
}

func formatTaskInspect(task swarm.Task, owning *swarm.Service, netNames, nodeNames map[string]string) []inspLine {
	var b inspBuilder
	cs := task.Spec.ContainerSpec
	b.title(fmt.Sprintf("TASK  %s  (slot %d)", shortID(task.ID), task.Slot))

	b.section("NETWORKS")
	nets := taskNetDNS(task, owning, netNames)
	if len(nets) == 0 {
		b.list(nil)
	}
	for _, n := range nets {
		b.net(n.Name, len(n.DNS), append(append([]string{}, n.DNS...), n.Extra...))
	}
	b.section("LABELS")
	if cs != nil {
		b.list(kvPairs(cs.Labels))
	} else {
		b.list(nil)
	}
	b.section("VOLUMES / MOUNTS")
	b.list(specMounts(cs))
	b.section("SECRETS")
	b.list(specSecrets(cs))
	if cfgs := specConfigs(cs); len(cfgs) > 0 {
		b.section("CONFIGS")
		b.list(cfgs)
	}
	b.section("STATE")
	b.kv("state", string(task.Status.State))
	b.kv("desired", string(task.DesiredState))
	b.kv("message", task.Status.Message)
	b.kv("error", task.Status.Err)
	if st := task.Status.ContainerStatus; st != nil && st.ExitCode != 0 {
		b.kv("exit code", fmt.Sprintf("%d", st.ExitCode))
	}
	b.kv("since", tstr(task.Status.Timestamp))

	b.section("PLACEMENT")
	node := nodeNames[task.NodeID]
	if node == "" {
		node = shortID(task.NodeID)
	}
	b.kv("node", node)
	b.kv("slot", fmt.Sprintf("%d", task.Slot))

	b.section("CONTAINER")
	if st := task.Status.ContainerStatus; st != nil {
		b.kv("id", st.ContainerID)
		if st.PID != 0 {
			b.kv("pid", fmt.Sprintf("%d", st.PID))
		}
	}
	b.section("IMAGE")
	b.list(imageLine(cs))
	if cs != nil && len(cs.Env) > 0 {
		b.section("ENV")
		b.list(cs.Env)
	}
	if lines := resourceLines(task.Spec.Resources); len(lines) > 0 {
		b.section("RESOURCES")
		b.list(lines)
	}
	b.section("META")
	b.kv("task id", task.ID)
	b.kv("service id", task.ServiceID)
	b.kv("created", tstr(task.Meta.CreatedAt))
	return b.lines
}

// --- field helpers ----------------------------------------------------------

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

// netDNS is a network a service/task is attached to, with the DNS names that
// resolve to it there (the default service name, tasks.<name>, and any custom
// aliases) and optional extra lines shown when expanded (e.g. task addresses).
type netDNS struct {
	Name  string
	DNS   []string
	Extra []string
}

// dnsNames builds the DNS names resolvable for a service on a network: the
// service name, tasks.<name>, plus the attachment's aliases.
func dnsNames(svcName string, aliases []string) []string {
	if svcName == "" {
		return append([]string{}, aliases...)
	}
	out := []string{svcName, "tasks." + svcName}
	return append(out, aliases...)
}

// serviceNetDNS returns the service's networks with their DNS names/aliases.
func serviceNetDNS(svc swarm.Service, netNames map[string]string) []netDNS {
	var out []netDNS
	seen := map[string]bool{}
	add := func(a swarm.NetworkAttachmentConfig) {
		if a.Target == "" || seen[a.Target] {
			return
		}
		seen[a.Target] = true
		name := netNames[a.Target]
		if name == "" {
			name = a.Target
		}
		out = append(out, netDNS{Name: name, DNS: dnsNames(svc.Spec.Name, a.Aliases)})
	}
	for _, a := range svc.Spec.TaskTemplate.Networks {
		add(a)
	}
	for _, a := range svc.Spec.Networks {
		add(a)
	}
	return out
}

// taskNetDNS returns a task's networks with the DNS names/aliases it inherits
// from its owning service (may be nil), plus the task's address on each network.
func taskNetDNS(task swarm.Task, owning *swarm.Service, netNames map[string]string) []netDNS {
	svcName, aliasByNet := "", map[string][]string{}
	if owning != nil {
		svcName = owning.Spec.Name
		for _, a := range owning.Spec.TaskTemplate.Networks {
			n := netNames[a.Target]
			if n == "" {
				n = a.Target
			}
			aliasByNet[n] = a.Aliases
		}
	}
	var out []netDNS
	for _, a := range task.NetworksAttachments {
		name := a.Network.Spec.Name
		if name == "" {
			if n := netNames[a.Network.ID]; n != "" {
				name = n
			} else {
				name = shortID(a.Network.ID)
			}
		}
		nd := netDNS{Name: name, DNS: dnsNames(svcName, aliasByNet[name])}
		if ips := strings.Join(a.Addresses, ", "); ips != "" {
			nd.Extra = []string{"addr: " + ips}
		}
		out = append(out, nd)
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
