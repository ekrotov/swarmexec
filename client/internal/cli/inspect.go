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
	"github.com/docker/docker/api/types/filters"
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
	inspTitle   inspKind = iota // the header line (service/task identity)
	inspHeader                  // a section header
	inspField                   // a data line — selectable/copyable
	inspDim                     // a placeholder ("-") — not selectable
	inspBlank                   // spacer
	inspNet                     // a collapsible network row: "+ name (N dns names)"
	inspUpgrade                 // an actionable "newer version available" row
	inspUpdate                  // a rolling-update-in-progress status line
	inspImage                   // the image ref; carries the version-picker data
)

// inspTaskRow is one container row in a network's drill-down: the column-aligned
// text, and the value copying that row should yield (the bare address — what an
// operator actually wants to paste).
type inspTaskRow struct{ Text, Copy string }

// inspLine is one line of the tabular inspect view. For inspNet rows, Net is the
// network name (collapse key), Count the number of DNS names, and Children the
// lines revealed when expanded (DNS names, then any extra like addresses).
type inspLine struct {
	Text      string
	Kind      inspKind
	Net       string
	Count     int
	Children  []string
	Encrypted bool   // inspNet: network has overlay data-plane encryption on
	Upgrade   string // inspUpgrade: the image ref to update the service to (default target)

	// inspNet: the address that identifies this attachment — a service's virtual
	// IP or a task's own address — with the label to show it under ("vip" /
	// "addr"). Shown in the collapsed row, because "what is its IP on this
	// network" is the question the section is most often opened for.
	Addr      string
	AddrLabel string

	// inspNet: the service's running containers on this network, revealed by a
	// second expand level below the DNS names. Empty for a task inspect (a task
	// is one container — its own address is in Addr).
	Tasks []inspTaskRow

	// inspUpgrade picker data (version-pinned services): the repo, the running
	// tag, the newer same-family tags (suggestions) and every known repo tag (to
	// validate a typed-in override). Empty for a :latest digest update.
	UpRepo    string
	UpCurrent string
	UpNewer   []string
	UpAll     []string

	// inspUpdate: the raw swarm update state ("updating", "paused", …), so the
	// view can tint the line by severity.
	UpdateState string
}

type inspBuilder struct{ lines []inspLine }

func (b *inspBuilder) push(k inspKind, s string) {
	b.lines = append(b.lines, inspLine{Text: s, Kind: k})
}
func (b *inspBuilder) title(s string)   { b.push(inspTitle, s) }
func (b *inspBuilder) section(s string) { b.push(inspBlank, ""); b.push(inspHeader, s) }

// net adds a collapsible network row from a resolved attachment: the header
// carries the DNS-name count, the lock icon and the address; the first expand
// level shows the DNS names, the second the container addresses.
func (b *inspBuilder) net(n netDNS) {
	b.lines = append(b.lines, inspLine{
		Kind:      inspNet,
		Text:      n.Name,
		Net:       n.Name,
		Count:     len(n.DNS),
		Children:  append(append([]string{}, n.DNS...), n.Extra...),
		Encrypted: n.Encrypted,
		Addr:      n.Addr,
		AddrLabel: n.AddrLabel,
		Tasks:     n.Tasks,
	})
}

// upgrade adds an actionable row offering to update the service's image. It
// carries st's picker data (repo, running tag, newer tags, all tags) so the
// overlay can open the version picker; target is the default (highest) ref.
func (b *inspBuilder) upgrade(text, target string, st imageStatus) {
	b.lines = append(b.lines, inspLine{
		Kind: inspUpgrade, Text: text, Upgrade: target,
		UpRepo: st.repo, UpCurrent: st.currentTag, UpNewer: st.newerVersions, UpAll: st.knownTags,
	})
}

// updateStatus adds a rolling-update-in-progress line (tinted by severity in the
// view). detail is an optional message from the manager (e.g. a failure reason).
func (b *inspBuilder) updateStatus(state, detail string) {
	label, _, _ := updateStatusLabel(state)
	text := label
	if detail != "" {
		text += " — " + detail
	}
	b.lines = append(b.lines, inspLine{Kind: inspUpdate, Text: text, UpdateState: state})
}

// image adds the image reference lines. The first carries the version-picker
// data (repo, running tag, newer tags, every known tag) so "u" can set a version
// whether or not a newer one exists — pinning or rolling back to a specific tag
// is just as valid as taking the newest.
func (b *inspBuilder) image(lines []string, st imageStatus) {
	if len(lines) == 0 {
		b.list(nil)
		return
	}
	b.lines = append(b.lines, inspLine{
		Kind: inspImage, Text: "  " + lines[0],
		UpRepo: st.repo, UpCurrent: st.currentTag, UpNewer: st.newerVersions, UpAll: st.knownTags,
	})
	for _, l := range lines[1:] {
		b.push(inspField, "  "+l)
	}
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
func serviceInspectViews(ctx context.Context, dcli *client.Client, ref string, reg *registryCache) ([]inspLine, string, error) {
	svc, rawb, err := dcli.ServiceInspectWithRaw(ctx, ref, types.ServiceInspectOptions{})
	if err != nil {
		return nil, "", err
	}
	info := networkInfo(ctx, dcli)
	// The service's own tasks carry the container addresses shown under each
	// network, and their node names. Both are best effort: an API error just
	// leaves the drill-down empty rather than failing the whole inspect.
	info.nodeNames = nodeHostnames(ctx, dcli)
	info.tasks, _ = dcli.TaskList(ctx, types.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", svc.ID)),
	})
	var img imageStatus
	if reg != nil && svc.Spec.TaskTemplate.ContainerSpec != nil {
		img = reg.statusNow(ctx, svc.Spec.TaskTemplate.ContainerSpec.Image, 5*time.Second)
	}
	return formatServiceInspect(svc, info, img), prettyJSON(rawb), nil
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
	info := networkInfo(ctx, dcli)
	info.nodeNames = nodeHostnames(ctx, dcli)
	return formatTaskInspect(task, owning, info), prettyJSON(rawb), nil
}

// serviceDiffLines inspects a service and returns a unified diff of its current
// spec against its PreviousSpec (what the last update changed). hasPrev is false
// when the service has never been updated (no PreviousSpec to compare against).
func serviceDiffLines(ctx context.Context, dcli *client.Client, ref string) (lines []string, hasPrev bool, err error) {
	svc, _, err := dcli.ServiceInspectWithRaw(ctx, ref, types.ServiceInspectOptions{})
	if err != nil {
		return nil, false, err
	}
	l, ok := serviceSpecDiff(svc, networkInfo(ctx, dcli).names)
	return l, ok, nil
}

// serviceSpecDiff compares a service's current spec to its PreviousSpec and
// returns the changed lines ("- " removed, "+ " added), grouped by field (the
// canonical line prefix). Unchanged fields are omitted.
func serviceSpecDiff(svc swarm.Service, netNames map[string]string) (lines []string, hasPrev bool) {
	if svc.PreviousSpec == nil {
		return nil, false
	}
	prev := specLines(*svc.PreviousSpec, netNames)
	cur := specLines(svc.Spec, netNames)
	prevSet := map[string]bool{}
	for _, l := range prev {
		prevSet[l] = true
	}
	curSet := map[string]bool{}
	for _, l := range cur {
		curSet[l] = true
	}
	seen := map[string]bool{}
	var all []string
	for _, l := range append(append([]string{}, prev...), cur...) {
		if !seen[l] {
			seen[l] = true
			all = append(all, l)
		}
	}
	sort.Strings(all) // groups by "field:" prefix, so an edited value's -/+ sit together
	for _, l := range all {
		switch {
		case prevSet[l] && curSet[l]:
			// unchanged — omit
		case curSet[l]:
			lines = append(lines, "+ "+l)
		default:
			lines = append(lines, "- "+l)
		}
	}
	return lines, true
}

// specLines renders the operator-relevant fields of a service spec as canonical,
// comparable "field: value" lines (sorted-friendly prefixes) for diffing.
func specLines(spec swarm.ServiceSpec, netNames map[string]string) []string {
	var out []string
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		out = append(out, "image: "+cs.Image)
		if len(cs.Command) > 0 {
			out = append(out, "command: "+strings.Join(cs.Command, " "))
		}
		if len(cs.Args) > 0 {
			out = append(out, "args: "+strings.Join(cs.Args, " "))
		}
		for _, e := range cs.Env {
			out = append(out, "env: "+e)
		}
		for _, s := range cs.Secrets {
			out = append(out, "secret: "+s.SecretName)
		}
		for _, m := range cs.Mounts {
			out = append(out, "mount: "+formatServiceMount(m))
		}
	}
	if spec.Mode.Replicated != nil && spec.Mode.Replicated.Replicas != nil {
		out = append(out, fmt.Sprintf("mode: replicated %d", *spec.Mode.Replicated.Replicas))
	} else if spec.Mode.Global != nil {
		out = append(out, "mode: global")
	}
	for k, v := range spec.Labels {
		out = append(out, "label: "+k+"="+v)
	}
	for _, n := range spec.TaskTemplate.Networks {
		name := netNames[n.Target]
		if name == "" {
			name = n.Target
		}
		out = append(out, "network: "+name)
		for _, a := range n.Aliases {
			out = append(out, "alias: "+name+"/"+a)
		}
	}
	if p := spec.TaskTemplate.Placement; p != nil {
		for _, c := range p.Constraints {
			out = append(out, "constraint: "+c)
		}
		for _, pr := range p.Preferences {
			if pr.Spread != nil {
				out = append(out, "spread: "+pr.Spread.SpreadDescriptor)
			}
		}
	}
	if spec.EndpointSpec != nil {
		for _, port := range spec.EndpointSpec.Ports {
			out = append(out, "port: "+formatServicePort(port))
		}
	}
	if r := spec.TaskTemplate.Resources; r != nil {
		if l := r.Limits; l != nil {
			if l.NanoCPUs > 0 {
				out = append(out, fmt.Sprintf("cpu-limit: %.2f", float64(l.NanoCPUs)/1e9))
			}
			if l.MemoryBytes > 0 {
				out = append(out, fmt.Sprintf("mem-limit: %d", l.MemoryBytes))
			}
		}
		if rv := r.Reservations; rv != nil {
			if rv.NanoCPUs > 0 {
				out = append(out, fmt.Sprintf("cpu-reservation: %.2f", float64(rv.NanoCPUs)/1e9))
			}
			if rv.MemoryBytes > 0 {
				out = append(out, fmt.Sprintf("mem-reservation: %d", rv.MemoryBytes))
			}
		}
	}
	return out
}

// netInfo is the cluster context the NETWORKS section needs beyond the object
// being inspected. Attachments reference a network by either id or name, so the
// maps are keyed by both. tasks are the inspected service's own tasks — the
// source of the per-container addresses; empty for a task inspect.
type netInfo struct {
	names     map[string]string // network id/name → name
	encrypted map[string]bool   // network id/name → overlay data-plane encryption
	nodeNames map[string]string // node id → hostname
	tasks     []swarm.Task
}

// networkInfo fills the network half of a netInfo from one NetworkList. Best
// effort; empty maps on error (the inspect still renders, just less readably).
func networkInfo(ctx context.Context, dcli *client.Client) netInfo {
	info := netInfo{names: map[string]string{}, encrypted: map[string]bool{}}
	nets, err := dcli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return info
	}
	for _, n := range nets {
		info.names[n.ID] = n.Name
		info.names[n.Name] = n.Name
		enc := networkEncrypted(n.Options)
		info.encrypted[n.ID] = enc
		info.encrypted[n.Name] = enc
	}
	return info
}

func prettyJSON(raw []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, raw, "", "  ") == nil {
		return buf.String()
	}
	return string(raw)
}

// --- tabular formatting -----------------------------------------------------

func formatServiceInspect(svc swarm.Service, info netInfo, img imageStatus) []inspLine {
	var b inspBuilder
	cs := svc.Spec.TaskTemplate.ContainerSpec
	b.title("SERVICE  " + svc.Spec.Name)

	// The upgrade hint sits directly under the title so it is the first thing the
	// operator sees and can act on ("u") without scrolling down to the IMAGE
	// section. Only shown when a newer image is actually available.
	if img.newer && img.updateTarget != "" && cs != nil {
		// Prefer the concrete newer version (the :latest image's label, or the
		// newer tag); fall back to a short digest only for a :latest image with no
		// version label.
		label := img.latestVersion
		if label == "" {
			label = shortDigest(img.latestDigest)
		}
		b.upgrade("⚠ a newer version is available: "+label+" — press u to update", img.updateTarget, img)
	}

	// A rolling update in flight is surfaced right under the title too, so it is
	// obvious the service is mid-update without reading task states.
	if us := svc.UpdateStatus; us != nil {
		if _, _, active := updateStatusLabel(string(us.State)); active {
			b.updateStatus(string(us.State), strings.TrimSpace(us.Message))
		}
	}

	b.section("NETWORKS")
	nets := serviceNetDNS(svc, info)
	if len(nets) == 0 {
		b.list(nil)
	}
	for _, n := range nets {
		b.net(n)
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
	b.image(imageLines(cs, img), img)
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

func formatTaskInspect(task swarm.Task, owning *swarm.Service, info netInfo) []inspLine {
	var b inspBuilder
	cs := task.Spec.ContainerSpec
	b.title(fmt.Sprintf("TASK  %s  (slot %d)", shortID(task.ID), task.Slot))

	b.section("NETWORKS")
	nets := taskNetDNS(task, owning, info)
	if len(nets) == 0 {
		b.list(nil)
	}
	for _, n := range nets {
		b.net(n)
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
	node := info.nodeNames[task.NodeID]
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
// aliases), the address it answers on, its containers, and optional extra lines
// shown when expanded.
type netDNS struct {
	Name string
	DNS  []string

	// Addr is the address this attachment is reached at — a service's virtual IP
	// or a task's own address — and AddrLabel says which ("vip" / "addr"). Both
	// empty when there is none (e.g. a dnsrr service, which has no VIP).
	Addr      string
	AddrLabel string

	Tasks     []inspTaskRow // the service's containers on this network
	Extra     []string
	Encrypted bool
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

// serviceNetDNS returns the service's networks with their DNS names/aliases,
// the service's virtual IP there, the containers behind that VIP, and whether
// the network is encrypted.
func serviceNetDNS(svc swarm.Service, info netInfo) []netDNS {
	vips := serviceVIPs(svc, info)
	addrs := serviceTaskAddrs(svc.Spec.Name, info)
	dnsrr := svc.Spec.EndpointSpec != nil && svc.Spec.EndpointSpec.Mode == swarm.ResolutionModeDNSRR

	var out []netDNS
	seen := map[string]bool{}
	add := func(a swarm.NetworkAttachmentConfig) {
		if a.Target == "" {
			return
		}
		name := info.names[a.Target]
		if name == "" {
			name = a.Target
		}
		// Deduplicate on the resolved name, not on Target: the same network can
		// appear once by id and once by name across the two attachment lists.
		if seen[name] {
			return
		}
		seen[name] = true
		nd := netDNS{
			Name:      name,
			DNS:       dnsNames(svc.Spec.Name, a.Aliases),
			Tasks:     taskAddrRows(pickAddrs(addrs, a.Target, name)),
			Encrypted: info.encrypted[a.Target],
		}
		if vip := pickAddr(vips, a.Target, name); vip != "" {
			nd.Addr, nd.AddrLabel = vip, "vip"
		} else if dnsrr {
			// Not a gap in the data: a dnsrr service deliberately has no VIP —
			// its DNS name resolves straight to the container addresses below.
			nd.Extra = []string{"no vip — dnsrr endpoint mode, the DNS name resolves to the containers"}
		}
		out = append(out, nd)
	}
	// TaskTemplate.Networks is current; Spec.Networks is the deprecated pre-v1.44
	// location — read both so older services still show their attachments.
	for _, a := range svc.Spec.TaskTemplate.Networks {
		add(a)
	}
	for _, a := range svc.Spec.Networks {
		add(a)
	}
	return out
}

// taskNetDNS returns a task's networks with the DNS names/aliases it inherits
// from its owning service (may be nil), plus the task's own address on each.
func taskNetDNS(task swarm.Task, owning *swarm.Service, info netInfo) []netDNS {
	svcName, aliasByNet := "", map[string][]string{}
	if owning != nil {
		svcName = owning.Spec.Name
		for _, a := range owning.Spec.TaskTemplate.Networks {
			n := info.names[a.Target]
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
			if n := info.names[a.Network.ID]; n != "" {
				name = n
			} else {
				name = shortID(a.Network.ID)
			}
		}
		nd := netDNS{
			Name:      name,
			DNS:       dnsNames(svcName, aliasByNet[name]),
			Encrypted: info.encrypted[a.Network.ID] || info.encrypted[name],
		}
		if ip := firstIPv4(a.Addresses); ip != "" {
			nd.Addr, nd.AddrLabel = ip, "addr"
		}
		// Any further addresses (IPv6, secondaries) still belong in the detail.
		if len(a.Addresses) > 1 {
			nd.Extra = []string{"addresses: " + strings.Join(a.Addresses, ", ")}
		}
		out = append(out, nd)
	}
	return out
}

// serviceVIPs maps a service's networks to the virtual IP the manager assigned
// it there. The manager reports VIPs by network id while a spec attachment may
// name the network instead, so the map is keyed by both. A service in dnsrr
// endpoint mode has no VIPs at all.
func serviceVIPs(svc swarm.Service, info netInfo) map[string]string {
	m := map[string]string{}
	for _, v := range svc.Endpoint.VirtualIPs {
		if v.Addr == "" {
			continue
		}
		m[v.NetworkID] = v.Addr
		if n := info.names[v.NetworkID]; n != "" {
			m[n] = v.Addr
		}
	}
	return m
}

// netTaskAddr is one running container of a service on one network — the
// deepest level of the NETWORKS drill-down.
type netTaskAddr struct {
	Name  string // "<service>.<slot>", or "<service>.<node>" for a global service
	Addr  string // its address on this network, as the manager reports it (CIDR)
	Node  string
	State string // current task state, shown only when it is not yet "running"
	Slot  int
}

// serviceTaskAddrs groups a service's current containers by the network they are
// attached to, keyed by network id AND name — a spec attachment may name either.
//
// The filter is the task's DESIRED state, not its current one. A task the
// manager has given up on (desired state shutdown) has had its address released
// — it may already belong to a different container, so listing it would be
// actively wrong. But a task that is only `preparing` or `starting` already
// holds its address, and during a rolling update that is most of them: filtering
// on the current state would empty the drill-down exactly when it is interesting.
// Such a task is listed with its state, so a row that is not serving traffic yet
// says so.
func serviceTaskAddrs(svcName string, info netInfo) map[string][]netTaskAddr {
	out := map[string][]netTaskAddr{}
	for _, t := range info.tasks {
		if t.DesiredState != swarm.TaskStateRunning {
			continue
		}
		for _, a := range t.NetworksAttachments {
			addr := firstIPv4(a.Addresses)
			if addr == "" {
				continue
			}
			ta := netTaskAddr{
				Name: taskDisplayName(svcName, t),
				Addr: addr,
				Node: info.nodeNames[t.NodeID],
				Slot: t.Slot,
			}
			if t.Status.State != swarm.TaskStateRunning {
				ta.State = string(t.Status.State)
			}
			id := a.Network.ID
			name := a.Network.Spec.Name
			if name == "" {
				name = info.names[id]
			}
			if id != "" {
				out[id] = append(out[id], ta)
			}
			if name != "" && name != id {
				out[name] = append(out[name], ta)
			}
		}
	}
	for k := range out {
		rows := out[k]
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Slot != rows[j].Slot {
				return rows[i].Slot < rows[j].Slot
			}
			return rows[i].Name < rows[j].Name
		})
	}
	return out
}

// pickAddr / pickAddrs resolve a spec attachment's Target — which may be a
// network id or a name — against a map keyed by both.
func pickAddr(m map[string]string, target, name string) string {
	if v := m[target]; v != "" {
		return v
	}
	return m[name]
}

func pickAddrs(m map[string][]netTaskAddr, target, name string) []netTaskAddr {
	if v := m[target]; len(v) > 0 {
		return v
	}
	return m[name]
}

// taskDisplayName names a task the way Swarm does: <service>.<slot> for a
// replicated service, <service>.<node> for a global one (which has no slot).
func taskDisplayName(svcName string, t swarm.Task) string {
	base := svcName
	if base == "" {
		base = shortID(t.ServiceID)
	}
	switch {
	case t.Slot > 0:
		return fmt.Sprintf("%s.%d", base, t.Slot)
	case t.NodeID != "":
		return base + "." + shortID(t.NodeID)
	default:
		return base + "." + shortID(t.ID)
	}
}

// taskAddrRows renders the container rows of one network: column-aligned for
// reading, each carrying the bare address as its copy value.
func taskAddrRows(ts []netTaskAddr) []inspTaskRow {
	nameW, addrW := 0, 0
	for _, t := range ts {
		if w := len(t.Name); w > nameW {
			nameW = w
		}
		if w := len(t.Addr); w > addrW {
			addrW = w
		}
	}
	nodeW := 0
	for _, t := range ts {
		if t.State != "" && len(t.Node) > nodeW { // only padded when a state follows
			nodeW = len(t.Node)
		}
	}
	out := make([]inspTaskRow, 0, len(ts))
	for _, t := range ts {
		row := fmt.Sprintf("%-*s  %-*s", nameW, t.Name, addrW, t.Addr)
		if t.Node != "" {
			row += fmt.Sprintf("  %-*s", nodeW, t.Node)
		}
		if t.State != "" {
			row += "  (" + t.State + ")"
		}
		out = append(out, inspTaskRow{Text: strings.TrimRight(row, " "), Copy: stripMask(t.Addr)})
	}
	return out
}

// firstIPv4 returns an attachment's first IPv4 address in the CIDR form the
// manager reports ("10.0.1.5/24"); "" when it has only IPv6 or no address yet.
func firstIPv4(addrs []string) string {
	for _, a := range addrs {
		if strings.Contains(stripMask(a), ".") {
			return a
		}
	}
	return ""
}

// stripMask turns "10.0.1.5/24" into "10.0.1.5" — the form that is useful to
// paste into a curl or a ping.
func stripMask(addr string) string {
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		return addr[:i]
	}
	return addr
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

// imageLines is imageLine plus, for a :latest image, the real version behind the
// pinned digest. The "newer available" notice is a separate actionable row.
func imageLines(cs *swarm.ContainerSpec, st imageStatus) []string {
	out := imageLine(cs)
	if out == nil {
		return nil
	}
	// Only meaningful for a :latest image (the digest hides the real version); a
	// version-pinned tag is already shown verbatim in the image ref.
	if st.currentTag == "latest" && st.version != "" {
		out = append(out, "version behind :latest: "+st.version)
	}
	return out
}

// shortDigest trims a "sha256:<hex>" digest to a readable prefix.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// stripDigest removes an "@sha256:…" suffix from an image ref, leaving repo:tag.
func stripDigest(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
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
