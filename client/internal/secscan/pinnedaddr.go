// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

// pinnedOverlayAddressAnalyzer flags a service whose configuration contains a
// literal IP address belonging to one of the cluster's overlay networks.
//
// Why this is a risk and not a style question: an overlay's addresses are not
// stable. Docker assigns them when the network is created, and anything that
// recreates it — a changed MTU option, a changed subnet, a stack redeployed
// from a file — hands out different ones. A service that names an alias is
// unaffected; a service that names an address stops working, and the failure
// is "no route to host" at startup, long after the change that caused it.
//
// The check is deliberately narrow. Only addresses inside a subnet the swarm
// actually owns count: a public IP, a loopback, an RFC1918 address from the
// office LAN and the node's own underlay address are all legitimate things to
// write down, and flagging them would make the check noise.
type pinnedOverlayAddressAnalyzer struct{}

// ipv4Literal matches a dotted quad with no surrounding digits or dots, so
// "10.0.1.118" is found inside "postgres://10.0.1.118:5432/db" while a version
// like "1.10.0.11" is not mistaken for one.
var ipv4Literal = regexp.MustCompile(`(^|[^0-9.])((?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])(?:\.(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])){3})([^0-9.]|$)`)

func (pinnedOverlayAddressAnalyzer) AnalyzeCluster(c Cluster) []SubjectFinding {
	subnets := overlaySubnets(c.Networks)
	if len(subnets) == 0 || len(c.Services) == 0 {
		return nil
	}
	var out []SubjectFinding
	for _, svc := range c.Services {
		hits := pinnedAddrsIn(svc.Text, subnets)
		if len(hits) == 0 {
			continue
		}
		out = append(out, SubjectFinding{
			Subject: Subject{Kind: SubjectService, Name: svc.Name},
			Finding: Finding{
				Rule:     "pinned-overlay-address",
				Title:    "overlay address written into the configuration",
				Severity: SevHigh,
				Detail: fmt.Sprintf(
					"the spec names %s, which %s an overlay network — overlay addresses are "+
						"reassigned whenever the network is recreated, and this service will then "+
						"fail to reach it with \"no route to host\". Use the service's network "+
						"alias instead, which survives a renumbering",
					strings.Join(hits, ", "), verbFor(len(hits))),
			},
		})
	}
	return out
}

func (pinnedOverlayAddressAnalyzer) Describe() string {
	return "overlay addresses hardcoded in a service's configuration"
}

// overlaySubnets collects the CIDRs of the swarm's overlay networks. Only
// overlays: a bridge network is node-local and an address inside it means
// something different.
func overlaySubnets(nets []Network) []netip.Prefix {
	var out []netip.Prefix
	for _, n := range nets {
		if n.Driver != "" && n.Driver != "overlay" {
			continue
		}
		out = append(out, n.Subnets...)
	}
	return out
}

// pinnedAddrsIn returns the distinct overlay addresses the text names, sorted
// so the finding reads the same every time.
func pinnedAddrsIn(text string, subnets []netip.Prefix) []string {
	seen := map[netip.Addr]bool{}
	for _, m := range ipv4Literal.FindAllStringSubmatch(text, -1) {
		addr, err := netip.ParseAddr(m[2])
		if err != nil {
			continue
		}
		for _, s := range subnets {
			if s.Contains(addr) {
				seen[addr] = true
				break
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	addrs := make([]netip.Addr, 0, len(seen))
	for a := range seen {
		addrs = append(addrs, a)
	}
	// By ADDRESS, not by text: sorted as strings, .30 would come before .7 and
	// the list would read as though it were unsorted.
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].Less(addrs[j]) })
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.String())
	}
	return out
}

// verbFor agrees the sentence with the number of addresses found.
func verbFor(n int) string {
	if n == 1 {
		return "belongs to"
	}
	return "belong to"
}
