// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"swarmexec/client/internal/swarmapi"
	"swarmexec/internal/dockerfmt"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// Overlay subnets must not overlap the network the hosts themselves sit on
// (the "underlay"). When they do, a container's route to that range points
// into the overlay: the hosts become unreachable from the container, and IPAM
// may even hand a container a host's own address. Docker does not check this —
// the swarm's default address pool is 10.0.0.0/8, which is exactly where many
// cloud private networks live. This file is the pure analysis (plus the thin
// fetch around it for the create form): which networks contain node addresses,
// overlap the configured underlay, or overlap each other.

// OverlapSeverity grades a finding. The order matters: a network's severity
// is the worst of its findings.
type OverlapSeverity int

const (
	// OverlapNone means nothing was found.
	OverlapNone OverlapSeverity = iota
	// OverlapWarn is an overlap that can break routing to part of a range
	// (the configured underlay, another network).
	OverlapWarn
	// OverlapHigh means the subnet contains a node's own address: containers
	// cannot reach that host, and IPAM may hand its address to a container.
	OverlapHigh
)

func (s OverlapSeverity) String() string {
	switch s {
	case OverlapHigh:
		return "high"
	case OverlapWarn:
		return "warn"
	}
	return "none"
}

// Overlap finding kinds.
const (
	OverlapHostIPs     = "host-ips"     // node addresses inside the subnet
	OverlapUnderlay    = "underlay"     // subnet overlaps a configured underlay CIDR
	OverlapNetwork     = "network"      // subnet overlaps another network's subnet
	OverlapDefaultPool = "default-pool" // the pool Docker picks from contains hosts/underlay
)

// OverlapFinding is one reason a network's addressing is risky.
type OverlapFinding struct {
	Kind     string
	Severity OverlapSeverity
	Message  string

	// Structured detail, so a caller with restricted visibility (MCP) can
	// rephrase the finding without revealing what it may not see.
	Subnet  netip.Prefix // the checked subnet (or the pool, for default-pool)
	Hosts   int          // host-ips: how many node addresses lie inside
	Network string       // network: the other network's name
}

// OverlapRemedy is the fix, shown wherever a finding is: Docker cannot
// renumber an existing network, and the default pool is fixed at swarm init.
const OverlapRemedy = "An existing network cannot be renumbered: detach its services, remove it and " +
	"recreate it with an explicit subnet outside the hosts' network (e.g. --subnet 10.100.1.0/24), " +
	"then re-attach. The swarm's default address pool can only be set at `docker swarm init --default-addr-pool`."

// DefaultAddrPool is what swarm uses when `swarm init` set no pool.
var DefaultAddrPool = netip.MustParsePrefix("10.0.0.0/8")

// NodeAddr is one address a node is known by on the underlay.
type NodeAddr struct {
	Node string // hostname
	Addr netip.Addr
}

// WorstSeverity is the highest severity among findings.
func WorstSeverity(fs []OverlapFinding) OverlapSeverity {
	worst := OverlapNone
	for _, f := range fs {
		if f.Severity > worst {
			worst = f.Severity
		}
	}
	return worst
}

// NodeAddrs collects the underlay addresses of the nodes: each node's
// Status.Addr plus, for managers, the host part of ManagerStatus.Addr (which
// may differ when the node advertises another interface). Unusable addresses
// (unset, 0.0.0.0, loopback) are skipped; each (node, addr) appears once.
func NodeAddrs(nodes []swarm.Node) []NodeAddr {
	seen := map[NodeAddr]bool{}
	var out []NodeAddr
	add := func(name, raw string) {
		a, ok := parseNodeAddr(raw)
		if !ok {
			return
		}
		na := NodeAddr{Node: name, Addr: a}
		if !seen[na] {
			seen[na] = true
			out = append(out, na)
		}
	}
	for _, n := range nodes {
		name := n.Description.Hostname
		if name == "" {
			name = dockerfmt.ShortID(n.ID)
		}
		add(name, n.Status.Addr)
		if n.ManagerStatus != nil {
			add(name, n.ManagerStatus.Addr)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if c := out[i].Addr.Compare(out[j].Addr); c != 0 {
			return c < 0
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// parseNodeAddr accepts "10.0.1.2" or "10.0.1.2:2377" (and bracketed IPv6).
func parseNodeAddr(raw string) (netip.Addr, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Addr{}, false
	}
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	a, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, false
	}
	a = a.Unmap()
	if a.IsUnspecified() || a.IsLoopback() {
		return netip.Addr{}, false
	}
	return a, true
}

// CheckSubnet analyses one subnet: node addresses inside it (high), overlap
// with an underlay CIDR (warn), overlap with another network's subnet (warn).
// skipID names the network the subnet belongs to, so it is not compared with
// itself ("" for a network that does not exist yet). Pure.
func CheckSubnet(subnet netip.Prefix, skipID string, nodes []NodeAddr, underlay []netip.Prefix, nets []Network) []OverlapFinding {
	if !subnet.IsValid() {
		return nil
	}
	subnet = subnet.Masked()
	var out []OverlapFinding
	var hosts []string
	for _, n := range nodes {
		if subnet.Contains(n.Addr) {
			hosts = append(hosts, fmt.Sprintf("%s (%s)", n.Addr, n.Node))
		}
	}
	if len(hosts) > 0 {
		out = append(out, OverlapFinding{
			Kind: OverlapHostIPs, Severity: OverlapHigh, Subnet: subnet, Hosts: len(hosts),
			Message: fmt.Sprintf("%s contains host IPs: %s", subnet, strings.Join(hosts, ", ")),
		})
	}
	for _, u := range underlay {
		if subnet.Overlaps(u) {
			out = append(out, OverlapFinding{
				Kind: OverlapUnderlay, Severity: OverlapWarn, Subnet: subnet,
				Message: fmt.Sprintf("%s overlaps the underlay %s", subnet, u.Masked()),
			})
		}
	}
	for _, other := range nets {
		if other.ID == skipID && skipID != "" {
			continue
		}
		for _, sp := range other.Subnets {
			if subnet.Overlaps(sp) {
				out = append(out, OverlapFinding{
					Kind: OverlapNetwork, Severity: OverlapWarn, Subnet: subnet, Network: other.Name,
					Message: fmt.Sprintf("%s overlaps network %s (%s)", subnet, other.Name, sp),
				})
			}
		}
	}
	return out
}

// AnalyzeOverlaps fills in each network's Overlaps from its subnets. Networks
// without IPAM subnets (host, none, an unconfigured local bridge) have nothing
// to check and stay clean. Pure; modifies nets in place.
func AnalyzeOverlaps(nets []Network, nodes []NodeAddr, underlay []netip.Prefix) {
	for i := range nets {
		var fs []OverlapFinding
		for _, s := range nets[i].Subnets {
			fs = append(fs, CheckSubnet(s, nets[i].ID, nodes, underlay, nets)...)
		}
		nets[i].Overlaps = fs
	}
}

// CheckDefaultPool is the create-time check for a network without a subnet:
// Docker will carve a /subnetSize out of the swarm's default address pool, and
// if node addresses or the underlay lie inside that pool the carved subnet may
// land on top of them. An empty pool means swarm's built-in 10.0.0.0/8. Pure.
func CheckDefaultPool(pool []netip.Prefix, subnetSize uint32, nodes []NodeAddr, underlay []netip.Prefix) []OverlapFinding {
	if len(pool) == 0 {
		pool = []netip.Prefix{DefaultAddrPool}
	}
	if subnetSize == 0 {
		subnetSize = 24
	}
	var out []OverlapFinding
	for _, p := range pool {
		p = p.Masked()
		var hits []string
		for _, n := range nodes {
			if p.Contains(n.Addr) {
				hits = append(hits, fmt.Sprintf("%s (%s)", n.Addr, n.Node))
			}
		}
		for _, u := range underlay {
			if p.Overlaps(u) {
				hits = append(hits, "underlay "+u.Masked().String())
			}
		}
		if len(hits) > 0 {
			out = append(out, OverlapFinding{
				Kind: OverlapDefaultPool, Severity: OverlapWarn, Subnet: p,
				Message: fmt.Sprintf("no subnet given: Docker will pick a /%d from the default address pool %s, "+
					"which also holds %s — the picked subnet may overlap the hosts. Set an explicit subnet "+
					"outside the hosts' network (e.g. 10.100.1.0/24).", subnetSize, p, strings.Join(hits, ", ")),
			})
		}
	}
	return out
}

// CheckNewNetwork is the pure core of the create-form check. With a subnet it
// runs CheckSubnet against everything that exists; without one (overlay
// driver only — a local bridge draws from the daemon's own pools, not the
// swarm's) it runs CheckDefaultPool. An unparsable subnet yields nothing: the
// create itself refuses it with a proper message.
func CheckNewNetwork(o NewNetworkOpts, nodes []NodeAddr, underlay []netip.Prefix, nets []Network, pool []netip.Prefix, subnetSize uint32) []OverlapFinding {
	if s := strings.TrimSpace(o.Subnet); s != "" {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil
		}
		return CheckSubnet(p, "", nodes, underlay, nets)
	}
	if !isOverlayDriver(o.Driver) {
		return nil
	}
	return CheckDefaultPool(pool, subnetSize, nodes, underlay)
}

func isOverlayDriver(d string) bool {
	d = strings.TrimSpace(d)
	return d == "" || d == "overlay"
}

// NetworkPlan is what a create should be told before it runs: what is wrong
// with the addressing it asked for, and what would be right instead.
type NetworkPlan struct {
	// Findings are the overlaps of the requested subnet (or of the default
	// pool, when none was given).
	Findings []OverlapFinding
	// Subnet is a proposal, set only when the request had no subnet of its
	// own or the one it had overlaps something. Invalid means none is offered.
	Subnet SubnetAdvice
	// MTU advises on framing; empty when the underlay's MTU is unknown.
	MTU MTUAdvice
}

// Advise is the sentence pair a form or a command prints: what is wrong, and
// what to do. Empty when there is nothing to say.
func (p NetworkPlan) Advise() []string {
	var out []string
	for _, f := range p.Findings {
		out = append(out, f.Severity.String()+": "+f.Message)
	}
	if p.Subnet.Subnet.IsValid() {
		out = append(out, p.Subnet.Describe())
	}
	if p.MTU.Problem != "" {
		out = append(out, "mtu: "+p.MTU.Problem)
	}
	if p.MTU.Option > 0 {
		out = append(out, fmt.Sprintf(
			"set --opt %s so containers get %d bytes, the most this underlay carries",
			p.MTU.OptionFlag(), p.MTU.Inside))
	}
	return out
}

// PlanNetworkCreate is PreflightNetworkCreate plus the two things the operator
// actually needs in order to decide: a subnet that collides with nothing, and
// the MTU the overlay has to be told about.
//
// It proposes; it never substitutes. Whether to accept an overlap is the
// operator's judgement — a lab cluster may legitimately want its overlays in
// the host range — and a tool that quietly picked for them would hide the
// decision rather than inform it.
func PlanNetworkCreate(ctx context.Context, dcli swarmapi.Client, o NewNetworkOpts, underlay []netip.Prefix, underlayMTU int) NetworkPlan {
	nodes, nets, pool, size := networkFacts(ctx, dcli, o)
	plan := NetworkPlan{
		Findings: CheckNewNetwork(o, nodes, underlay, nets, pool, size),
		MTU:      AdviseMTU(underlayMTU, atoiSafe(o.MTU)),
	}
	if WorstSeverity(plan.Findings) > OverlapNone || strings.TrimSpace(o.Subnet) == "" {
		bits := 24
		if size > 0 && size <= 30 {
			bits = int(size)
		}
		plan.Subnet = SuggestSubnet(bits, nodes, underlay, nets, pool)
	}
	return plan
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// PreflightNetworkCreate gathers what CheckNewNetwork needs — node addresses,
// the existing networks' subnets and, when no subnet is given, the swarm's
// default address pool — and runs it. Best effort: a failed lookup drops that
// part of the check rather than blocking the create (the daemon is the
// authority on whether the network can be created at all).
func PreflightNetworkCreate(ctx context.Context, dcli swarmapi.Client, o NewNetworkOpts, underlay []netip.Prefix) []OverlapFinding {
	nodes, nets, pool, size := networkFacts(ctx, dcli, o)
	return CheckNewNetwork(o, nodes, underlay, nets, pool, size)
}

// networkFacts reads what both the check and the proposal need. Best effort: a
// failed lookup drops that part rather than blocking the create, since the
// daemon remains the authority on whether the network can be made at all.
func networkFacts(ctx context.Context, dcli swarmapi.Client, o NewNetworkOpts) ([]NodeAddr, []Network, []netip.Prefix, uint32) {
	var nodes []NodeAddr
	if res, err := dcli.NodeList(ctx, client.NodeListOptions{}); err == nil {
		nodes = NodeAddrs(res.Items)
	}
	var nets []Network
	if res, err := dcli.NetworkList(ctx, client.NetworkListOptions{}); err == nil {
		for _, n := range res.Items {
			subs, _ := ipamSubnets(n.IPAM)
			nets = append(nets, Network{ID: n.ID, Name: n.Name, Subnets: subs})
		}
	}
	var pool []netip.Prefix
	var size uint32
	if isOverlayDriver(o.Driver) {
		if res, err := dcli.SwarmInspect(ctx, client.SwarmInspectOptions{}); err == nil {
			pool, size = res.Swarm.DefaultAddrPool, res.Swarm.SubnetSize
		}
	}
	return nodes, nets, pool, size
}

// ipamSubnets returns a network's configured subnets and gateways (gateways
// only where set).
func ipamSubnets(ipam network.IPAM) ([]netip.Prefix, []netip.Addr) {
	var subs []netip.Prefix
	var gws []netip.Addr
	for _, c := range ipam.Config {
		if c.Subnet.IsValid() {
			subs = append(subs, c.Subnet)
		}
		if c.Gateway.IsValid() {
			gws = append(gws, c.Gateway)
		}
	}
	return subs, gws
}
