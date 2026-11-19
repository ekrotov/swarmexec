// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"fmt"
	"sort"
	"strings"
)

// Picking the underlay's MTU out of a node's interfaces.
//
// The agent reports what sysfs says and nothing more: names, MTUs, operstate,
// ARP type. It cannot say which link is "the underlay", because sysfs has no
// addresses and the agent's own namespace has no view of the host's. So the
// choice is made here, by a rule that is written down and shown to the
// operator, rather than inside the agent where nobody would see it.

// arpTypeEthernet is sysfs "type" 1. Loopback is 772; tunnels, wireguard and
// the like are other values.
const arpTypeEthernet = 1

// NodeLink is one interface of one node, as the agent read it from sysfs.
type NodeLink struct {
	Node      string
	Name      string
	MTU       int
	Operstate string
	ARPType   int
}

// UnderlayMTU is the conclusion drawn from the nodes' links.
type UnderlayMTU struct {
	// MTU is the value to size overlays against, 0 when it could not be
	// determined. Zero means unknown and must never be treated as a default.
	MTU int
	// From names the links the value came from, e.g. "docker2v2 enp7s0 1450".
	From []string
	// Candidates is how many links were considered across all nodes. Zero
	// means no agent reported any — typically because the agent predates this
	// or its deployment has no sysfs mount.
	Candidates int
	// Disagree is set when the nodes do not share one MTU. The lowest wins,
	// because an overlay has to fit the smallest link that carries it, and
	// the field exists so the report can say so instead of hiding it.
	Disagree bool
}

// Describe renders the conclusion for a report or a dialog.
func (u UnderlayMTU) Describe() string {
	if u.MTU == 0 {
		if u.Candidates == 0 {
			return "the underlay's MTU is unknown: no agent reported its links " +
				"(an older agent, or a deployment without the read-only /sys mount)"
		}
		return "the underlay's MTU could not be determined from the links reported"
	}
	s := fmt.Sprintf("underlay MTU %d, from %s", u.MTU, strings.Join(u.From, ", "))
	if u.Disagree {
		s += " — the nodes do not agree; the lowest is used, because an overlay " +
			"has to fit the smallest link that carries it"
	}
	return s
}

// DetermineUnderlayMTU picks the MTU to size overlays against.
//
// The rule, in full, because it has to be defensible: only Ethernet links
// count (sysfs type 1), which drops loopback and tunnels; only links that are
// up count; the obvious virtual ones are skipped by name, since a node running
// containers is covered in bridges and veths whose 1500 says nothing about the
// wire. Of what remains, the LOWEST MTU wins — an overlay frame has to fit
// whichever link carries it, and being 50 bytes conservative costs throughput
// while being 50 bytes optimistic costs packets.
func DetermineUnderlayMTU(links []NodeLink) UnderlayMTU {
	var out UnderlayMTU
	best := map[string]NodeLink{} // lowest candidate per node
	for _, l := range links {
		if !isUnderlayCandidate(l) {
			continue
		}
		out.Candidates++
		if cur, ok := best[l.Node]; !ok || l.MTU < cur.MTU {
			best[l.Node] = l
		}
	}
	if len(best) == 0 {
		return out
	}
	nodes := make([]string, 0, len(best))
	for n := range best {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		l := best[n]
		out.From = append(out.From, fmt.Sprintf("%s %s %d", n, l.Name, l.MTU))
		if out.MTU == 0 || l.MTU < out.MTU {
			out.MTU = l.MTU
		}
	}
	for _, n := range nodes {
		if best[n].MTU != out.MTU {
			out.Disagree = true
			break
		}
	}
	return out
}

// virtualPrefixes are the interface names a container host grows by itself.
// Their MTU is Docker's own choice, not the wire's, so counting them would
// answer the question with the assumption being questioned.
var virtualPrefixes = []string{
	"docker", "br-", "veth", "virbr", "vnet", "vx-", "cni", "flannel", "cali",
	"tap", "tun", "wg", "ovs", "kube", "lxc", "dummy",
}

func isUnderlayCandidate(l NodeLink) bool {
	if l.MTU <= 0 || l.ARPType != arpTypeEthernet {
		return false
	}
	// "unknown" is what a link with no carrier state reports; it is common on
	// virtual links and is not evidence of being up.
	if !strings.EqualFold(l.Operstate, "up") {
		return false
	}
	name := strings.ToLower(l.Name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}
