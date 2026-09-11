// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

func joinInspLines(lines []inspLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
		if l.Addr != "" { // net rows show the vip/addr in the collapsed header
			b.WriteString("  " + l.AddrLabel + " " + l.Addr)
		}
		b.WriteByte('\n')
		for _, c := range l.Children { // collapsible net rows keep DNS names here
			b.WriteString(c)
			b.WriteByte('\n')
		}
		for _, t := range l.Tasks { // …and the container drill-down here
			b.WriteString(t.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func headerIndex(lines []inspLine, header string) int {
	for i, l := range lines {
		if l.Kind == inspHeader && l.Text == header {
			return i
		}
	}
	return -1
}

func hasKind(lines []inspLine, k inspKind) *inspLine {
	for i := range lines {
		if lines[i].Kind == k {
			return &lines[i]
		}
	}
	return nil
}

func TestFormatServiceInspect_UpdateStatus(t *testing.T) {
	base := func() swarm.Service {
		var s swarm.Service
		s.Spec.Name = "web"
		s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:1"}
		return s
	}

	// A rolling update in flight yields an inspUpdate line carrying its state.
	s := base()
	s.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStateUpdating, Message: "update in progress"}
	lines := formatServiceInspect(s, netInfo{}, imageStatus{})
	up := hasKind(lines, inspUpdate)
	if up == nil {
		t.Fatal("expected an inspUpdate line while the service is updating")
	}
	if up.UpdateState != "updating" {
		t.Errorf("inspUpdate state = %q, want updating", up.UpdateState)
	}
	if !strings.Contains(up.Text, "updating") || !strings.Contains(up.Text, "update in progress") {
		t.Errorf("inspUpdate text = %q, want the label and message", up.Text)
	}

	// A finished update produces no status line.
	s = base()
	s.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStateCompleted}
	if hasKind(formatServiceInspect(s, netInfo{}, imageStatus{}), inspUpdate) != nil {
		t.Error("a completed update should not produce an inspUpdate line")
	}
}

func TestFormatServiceInspect_OrderAndContent(t *testing.T) {
	var s swarm.Service
	s.ID = "svc123"
	s.Spec.Name = "web"
	s.Spec.Labels = map[string]string{"tier": "frontend"}
	s.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{
		Image:   "nginx:1",
		Secrets: []*swarm.SecretReference{{SecretName: "db-pw"}},
		Mounts:  []mount.Mount{{Type: mount.TypeVolume, Source: "assets", Target: "/data"}},
	}

	lines := formatServiceInspect(s, netInfo{names: map[string]string{"netid1": "frontend-net"}, encrypted: map[string]bool{"netid1": true}}, imageStatus{version: "1.2.3", currentTag: "latest", newer: true, latestDigest: "sha256:abcdef0123456789", latestVersion: "1.5.0", updateTarget: "nginx:latest@sha256:abcdef0123456789"})
	joined := joinInspLines(lines)

	for _, want := range []string{"frontend-net", "tier=frontend", "assets -> /data", "db-pw", "nginx:1", "version behind :latest: 1.2.3", "newer version is available: 1.5.0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("formatted output missing %q:\n%s", want, joined)
		}
	}
	// The encrypted network's inspNet row must be marked so the view can show a lock.
	for _, l := range lines {
		if l.Kind == inspNet && l.Net == "frontend-net" && !l.Encrypted {
			t.Errorf("network row not marked encrypted: %+v", l)
		}
	}
	// A newer registry image produces an actionable upgrade row targeting the new digest.
	var up *inspLine
	for i := range lines {
		if lines[i].Kind == inspUpgrade {
			up = &lines[i]
		}
	}
	if up == nil {
		t.Fatal("expected an inspUpgrade row when a newer image is available")
	}
	if !strings.Contains(up.Upgrade, "@sha256:abcdef0123456789") {
		t.Errorf("upgrade target = %q, want the new latest digest", up.Upgrade)
	}
	// Operator-first ordering: networks, labels, volumes, secrets — all before META.
	iNet := headerIndex(lines, "NETWORKS")
	iLbl := headerIndex(lines, "LABELS")
	iVol := headerIndex(lines, "VOLUMES / MOUNTS")
	iSec := headerIndex(lines, "SECRETS")
	iMeta := headerIndex(lines, "META")
	if !(iNet >= 0 && iNet < iLbl && iLbl < iVol && iVol < iSec && iSec < iMeta) {
		t.Errorf("section order wrong: net=%d lbl=%d vol=%d sec=%d meta=%d", iNet, iLbl, iVol, iSec, iMeta)
	}
	// The value lines must be selectable fields, the headers must not be.
	for _, l := range lines {
		if l.Kind == inspField && strings.TrimSpace(l.Text) == "" {
			t.Errorf("empty field line should not be a selectable field")
		}
	}
}

func TestServiceSpecDiff(t *testing.T) {
	var svc swarm.Service
	prev := swarm.ServiceSpec{}
	prev.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:1", Env: []string{"LOG=info", "KEEP=1"}}
	prev.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:2", Env: []string{"LOG=debug", "KEEP=1"}}
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}, {Target: "netid2"}}
	svc.PreviousSpec = &prev

	lines, hasPrev := serviceSpecDiff(svc, map[string]string{"netid1": "frontend", "netid2": "monitoring"})
	if !hasPrev {
		t.Fatal("hasPrev should be true when PreviousSpec is set")
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"- image: nginx:1", "+ image: nginx:2", "- env: LOG=info", "+ env: LOG=debug", "+ network: monitoring"} {
		if !strings.Contains(joined, want) {
			t.Errorf("diff missing %q:\n%s", want, joined)
		}
	}
	// Unchanged fields must be omitted.
	for _, notWant := range []string{"KEEP=1", "network: frontend"} {
		if strings.Contains(joined, notWant) {
			t.Errorf("diff should omit unchanged %q:\n%s", notWant, joined)
		}
	}
	// No previous spec → nothing to diff.
	svc.PreviousSpec = nil
	if _, ok := serviceSpecDiff(svc, nil); ok {
		t.Error("hasPrev should be false when PreviousSpec is nil")
	}
}

func TestFormatTaskInspect_NetworksAndState(t *testing.T) {
	var task swarm.Task
	task.ID = "task123"
	task.Slot = 2
	task.DesiredState = swarm.TaskStateRunning
	task.Status.State = swarm.TaskStateRunning
	task.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: "cabc123"}
	task.NetworksAttachments = []swarm.NetworkAttachment{
		{Network: swarm.Network{ID: "netid1"}, Addresses: []string{"10.0.1.5/24"}},
	}
	task.ServiceID = "svc1"
	task.Spec.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:1"}

	// Owning service carries the DNS name + an alias the task inherits.
	var owning swarm.Service
	owning.Spec.Name = "web"
	owning.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1", Aliases: []string{"frontend"}}}

	lines := formatTaskInspect(task, &owning, netInfo{names: map[string]string{"netid1": "frontend-net"}, encrypted: map[string]bool{"netid1": true}, nodeNames: map[string]string{"node1": "host-a"}})
	joined := joinInspLines(lines)

	// Net name in the collapsible header; DNS names + alias + addr in its children.
	for _, want := range []string{"frontend-net", "10.0.1.5/24", "running", "cabc123", "web", "tasks.web", "frontend"} {
		if !strings.Contains(joined, want) {
			t.Errorf("task inspect missing %q:\n%s", want, joined)
		}
	}
	// The network row is a collapsible inspNet entry with a DNS-name count.
	var netLine *inspLine
	for i := range lines {
		if lines[i].Kind == inspNet && lines[i].Net == "frontend-net" {
			netLine = &lines[i]
		}
	}
	if netLine == nil {
		t.Fatalf("no collapsible net row for frontend-net")
	}
	if netLine.Count != 3 { // web, tasks.web, frontend
		t.Errorf("dns-name count = %d, want 3", netLine.Count)
	}
	if i, j := headerIndex(lines, "NETWORKS"), headerIndex(lines, "STATE"); !(i >= 0 && i < j) {
		t.Errorf("NETWORKS should precede STATE (net=%d state=%d)", i, j)
	}
}

// runningTask builds a running task of a service on one network.
func runningTask(slot int, node, netID, netName, addr string) swarm.Task {
	var t swarm.Task
	t.Slot = slot
	t.NodeID = node
	t.DesiredState = swarm.TaskStateRunning
	t.Status.State = swarm.TaskStateRunning
	var n swarm.Network
	n.ID = netID
	n.Spec.Name = netName
	t.NetworksAttachments = []swarm.NetworkAttachment{{Network: n, Addresses: []string{addr}}}
	return t
}

// The NETWORKS section answers "which address is this service reached at" on
// two levels: the service VIP in the collapsed row, the container addresses
// behind it one drill-down deeper.
func TestServiceNetDNS_VIPAndContainerAddresses(t *testing.T) {
	var svc swarm.Service
	svc.Spec.Name = "web"
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}
	svc.Endpoint.VirtualIPs = []swarm.EndpointVirtualIP{{NetworkID: "netid1", Addr: "10.0.1.2/24"}}

	// A task the manager has given up on: its address is released, so it must not
	// be listed even though the API still reports the attachment.
	shutdown := runningTask(9, "node9", "netid1", "frontend-net", "10.0.1.99/24")
	shutdown.DesiredState = swarm.TaskStateShutdown
	shutdown.Status.State = swarm.TaskStateShutdown

	// A task that is still starting DOES hold its address — during a rolling
	// update that is most of them, so it is listed, with its state.
	starting := runningTask(3, "node1", "netid1", "frontend-net", "10.0.1.7/24")
	starting.Status.State = swarm.TaskStateStarting

	info := netInfo{
		names:     map[string]string{"netid1": "frontend-net", "frontend-net": "frontend-net"},
		encrypted: map[string]bool{},
		nodeNames: map[string]string{"node1": "host-a", "node2": "host-b"},
		tasks: []swarm.Task{
			runningTask(2, "node2", "netid1", "frontend-net", "10.0.1.6/24"),
			runningTask(1, "node1", "netid1", "frontend-net", "10.0.1.5/24"),
			shutdown,
			starting,
		},
	}
	nets := serviceNetDNS(svc, info)
	if len(nets) != 1 {
		t.Fatalf("got %d networks, want 1", len(nets))
	}
	n := nets[0]
	if n.Addr != "10.0.1.2/24" || n.AddrLabel != "vip" {
		t.Errorf("vip = %q/%q, want 10.0.1.2/24 labelled vip", n.AddrLabel, n.Addr)
	}
	if len(n.Tasks) != 3 {
		t.Fatalf("got %d container rows, want 3 (the shut-down task's address is released)", len(n.Tasks))
	}
	// A task that is not serving traffic yet says so; a running one is unadorned.
	if !strings.Contains(n.Tasks[2].Text, "(starting)") {
		t.Errorf("a starting task should be labelled: %q", n.Tasks[2].Text)
	}
	if strings.Contains(n.Tasks[0].Text, "(") {
		t.Errorf("a running task needs no state suffix: %q", n.Tasks[0].Text)
	}
	// Ordered by slot, not by the manager's arbitrary task order.
	if !strings.HasPrefix(n.Tasks[0].Text, "web.1") || !strings.HasPrefix(n.Tasks[1].Text, "web.2") {
		t.Errorf("container rows not ordered by slot: %q, %q", n.Tasks[0].Text, n.Tasks[1].Text)
	}
	for _, want := range []string{"10.0.1.5/24", "host-a"} {
		if !strings.Contains(n.Tasks[0].Text, want) {
			t.Errorf("container row %q missing %q", n.Tasks[0].Text, want)
		}
	}
	// Copying a row yields the bare address — the value you paste into a curl.
	if n.Tasks[0].Copy != "10.0.1.5" {
		t.Errorf("copy value = %q, want the bare address", n.Tasks[0].Copy)
	}
	if got := netRowCopy(inspLine{Text: "frontend-net", Addr: n.Addr}); got != "10.0.1.2" {
		t.Errorf("net row copy = %q, want the bare vip", got)
	}
	// The stale task must not leak in through the name-keyed lookup either.
	if strings.Contains(joinInspLines(formatServiceInspect(svc, info, imageStatus{})), "10.0.1.99") {
		t.Error("a shut-down task's released address must not be shown")
	}
}

// A spec attachment may name the network instead of carrying its id, while the
// manager reports VIPs and task attachments by id — the lookup must bridge that.
func TestServiceNetDNS_AttachmentByName(t *testing.T) {
	var svc swarm.Service
	svc.Spec.Name = "web"
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "frontend-net"}}
	svc.Endpoint.VirtualIPs = []swarm.EndpointVirtualIP{{NetworkID: "netid1", Addr: "10.0.1.2/24"}}

	info := netInfo{
		names:     map[string]string{"netid1": "frontend-net", "frontend-net": "frontend-net"},
		encrypted: map[string]bool{},
		tasks:     []swarm.Task{runningTask(1, "node1", "netid1", "", "10.0.1.5/24")},
	}
	n := serviceNetDNS(svc, info)[0]
	if n.Addr != "10.0.1.2/24" {
		t.Errorf("vip = %q, want it resolved through the network name", n.Addr)
	}
	if len(n.Tasks) != 1 {
		t.Errorf("got %d container rows, want 1", len(n.Tasks))
	}
}

// A dnsrr service has no VIP by design — say so rather than showing a blank.
func TestServiceNetDNS_DNSRRHasNoVIP(t *testing.T) {
	var svc swarm.Service
	svc.Spec.Name = "web"
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}
	svc.Spec.EndpointSpec = &swarm.EndpointSpec{Mode: swarm.ResolutionModeDNSRR}

	n := serviceNetDNS(svc, netInfo{names: map[string]string{"netid1": "net"}, encrypted: map[string]bool{}})[0]
	if n.Addr != "" {
		t.Errorf("dnsrr service should have no vip, got %q", n.Addr)
	}
	if len(n.Extra) != 1 || !strings.Contains(n.Extra[0], "dnsrr") {
		t.Errorf("expected a note explaining the missing vip, got %v", n.Extra)
	}
}

// A global service's tasks have no slot; Swarm names them <service>.<node>.
func TestTaskDisplayName(t *testing.T) {
	var global swarm.Task
	global.NodeID = "nodeabcdef0123"
	if got := taskDisplayName("agent", global); got != "agent."+shortID("nodeabcdef0123") {
		t.Errorf("global task name = %q", got)
	}
	if got := taskDisplayName("web", swarm.Task{Slot: 3}); got != "web.3" {
		t.Errorf("replicated task name = %q, want web.3", got)
	}
}

func TestTaskAddrRowsAlignment(t *testing.T) {
	rows := taskAddrRows([]netTaskAddr{
		{Name: "web.1", Addr: "10.0.1.5/24", Node: "host-a"},
		{Name: "web.10", Addr: "10.0.1.11/24"},
	})
	if len(rows[0].Text) < len("web.10") {
		t.Fatalf("row too short: %q", rows[0].Text)
	}
	// The address column starts at the same offset in both rows.
	if strings.Index(rows[0].Text, "10.0.1.5") != strings.Index(rows[1].Text, "10.0.1.11") {
		t.Errorf("address column not aligned:\n%q\n%q", rows[0].Text, rows[1].Text)
	}
	// No node → no trailing padding left behind.
	if rows[1].Text != strings.TrimRight(rows[1].Text, " ") {
		t.Errorf("trailing whitespace in %q", rows[1].Text)
	}
}

func TestFirstIPv4AndStripMask(t *testing.T) {
	if got := firstIPv4([]string{"fd00::5/64", "10.0.1.5/24"}); got != "10.0.1.5/24" {
		t.Errorf("firstIPv4 = %q, want the IPv4 address in CIDR form", got)
	}
	if got := firstIPv4([]string{"fd00::5/64"}); got != "" {
		t.Errorf("IPv6-only attachment should yield no IPv4, got %q", got)
	}
	if got := firstIPv4(nil); got != "" {
		t.Errorf("no addresses should yield %q", got)
	}
	if got := stripMask("10.0.1.5"); got != "10.0.1.5" {
		t.Errorf("stripMask of a bare address = %q", got)
	}
}

// The container drill-down's collapse key must never collide with a network's
// own key — a network named like the synthetic key would silently toggle both.
func TestNetTasksKeyCannotCollide(t *testing.T) {
	if netTasksKey("web") == "web" {
		t.Fatal("the container level must not share the network's own collapse key")
	}
	if !strings.Contains(netTasksKey("web"), "\x00") {
		t.Error("key must contain a NUL, which a Docker network name cannot")
	}
	if netTasksKey("a") == netTasksKey("b") {
		t.Error("keys must be per network")
	}
}

// The same network can be attached under its id in TaskTemplate.Networks and
// under its name in the deprecated Spec.Networks — it must be listed once.
func TestServiceNetDNS_DeduplicatesByName(t *testing.T) {
	var svc swarm.Service
	svc.Spec.Name = "web"
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}
	svc.Spec.Networks = []swarm.NetworkAttachmentConfig{{Target: "frontend-net"}}

	info := netInfo{
		names:     map[string]string{"netid1": "frontend-net", "frontend-net": "frontend-net"},
		encrypted: map[string]bool{},
	}
	if nets := serviceNetDNS(svc, info); len(nets) != 1 {
		t.Errorf("got %d rows, want the network listed once: %+v", len(nets), nets)
	}
}

// Swarm joins a service to the ingress network by itself as soon as it
// publishes a port in ingress mode — the attachment is nowhere in the spec, but
// the VIP is the address the routing mesh answers on, so it gets a row.
func TestServiceNetDNS_IngressRow(t *testing.T) {
	var svc swarm.Service
	svc.Spec.Name = "web"
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}
	svc.Endpoint.VirtualIPs = []swarm.EndpointVirtualIP{
		{NetworkID: "netid1", Addr: "10.0.1.2/24"},
		{NetworkID: "ingressid", Addr: "10.0.0.5/24"},
	}
	svc.Endpoint.Ports = []swarm.PortConfig{
		{PublishedPort: 8080, TargetPort: 80, Protocol: swarm.PortConfigProtocolTCP, PublishMode: swarm.PortConfigPublishModeIngress},
		{PublishedPort: 9000, TargetPort: 9000, Protocol: swarm.PortConfigProtocolTCP, PublishMode: swarm.PortConfigPublishModeHost},
	}

	task := runningTask(1, "node1", "netid1", "frontend-net", "10.0.1.5/24")
	var ing swarm.Network
	ing.ID = "ingressid"
	ing.Spec.Name = "ingress"
	task.NetworksAttachments = append(task.NetworksAttachments,
		swarm.NetworkAttachment{Network: ing, Addresses: []string{"10.0.0.9/24"}})

	info := netInfo{
		names:     map[string]string{"netid1": "frontend-net", "frontend-net": "frontend-net", "ingressid": "ingress", "ingress": "ingress"},
		encrypted: map[string]bool{},
		ingress:   map[string]bool{"ingressid": true, "ingress": true},
		nodeNames: map[string]string{"node1": "host-a"},
		tasks:     []swarm.Task{task},
	}
	nets := serviceNetDNS(svc, info)
	if len(nets) != 2 {
		t.Fatalf("got %d rows, want the spec network plus ingress: %+v", len(nets), nets)
	}
	// The spec's own networks come first; ingress is appended, not interleaved.
	if nets[0].Name != "frontend-net" || nets[1].Name != "ingress" {
		t.Fatalf("wrong order: %q then %q", nets[0].Name, nets[1].Name)
	}
	ingRow := nets[1]
	if ingRow.Addr != "10.0.0.5/24" || ingRow.AddrLabel != "vip" {
		t.Errorf("ingress vip = %q/%q", ingRow.AddrLabel, ingRow.Addr)
	}
	// No service DNS name resolves on ingress, so the count would read "0 dns
	// names" — the note replaces it.
	if ingRow.Note != "routing mesh" {
		t.Errorf("ingress note = %q, want routing mesh", ingRow.Note)
	}
	if len(ingRow.DNS) != 0 {
		t.Errorf("ingress should carry no service DNS names: %v", ingRow.DNS)
	}
	// The published ports are the reason the row exists; host-mode ones bypass
	// ingress and must not be claimed for it.
	if len(ingRow.Extra) != 1 || !strings.Contains(ingRow.Extra[0], "8080 -> 80/tcp") {
		t.Errorf("ingress ports = %v, want only the ingress-published one", ingRow.Extra)
	}
	// It drills down to the containers' ingress addresses, like any other row.
	if len(ingRow.Tasks) != 1 || !strings.Contains(ingRow.Tasks[0].Text, "10.0.0.9/24") {
		t.Errorf("ingress container rows = %+v", ingRow.Tasks)
	}
}

// A VIP on a network the spec DOES attach must not produce a second row.
func TestServiceNetDNS_NoDuplicateRowForSpecNetwork(t *testing.T) {
	var svc swarm.Service
	svc.Spec.Name = "web"
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}
	svc.Endpoint.VirtualIPs = []swarm.EndpointVirtualIP{{NetworkID: "netid1", Addr: "10.0.1.2/24"}}

	info := netInfo{
		names:     map[string]string{"netid1": "frontend-net", "frontend-net": "frontend-net"},
		encrypted: map[string]bool{},
		ingress:   map[string]bool{},
	}
	if nets := serviceNetDNS(svc, info); len(nets) != 1 {
		t.Errorf("got %d rows, want 1: %+v", len(nets), nets)
	}
}

// A service with no published ports never lands on ingress, so no extra row.
func TestServiceNetDNS_NoIngressWithoutVIP(t *testing.T) {
	var svc swarm.Service
	svc.Spec.Name = "web"
	svc.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "netid1"}}

	info := netInfo{names: map[string]string{"netid1": "net"}, encrypted: map[string]bool{}, ingress: map[string]bool{}}
	if nets := serviceNetDNS(svc, info); len(nets) != 1 {
		t.Errorf("got %d rows, want only the spec network: %+v", len(nets), nets)
	}
}
