// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"net/netip"
	"strings"
	"testing"

	"swarmexec/client/internal/swarmops"
)

// A restricted caller sees overlap findings, but never the name or subnet of
// a network it may not see, nor node addresses.
func TestNetworkRowRedactsWhatTheCallerMayNotSee(t *testing.T) {
	sub := netip.MustParsePrefix("10.0.1.0/24")
	n := swarmops.Network{Name: "shop_net", Subnets: []netip.Prefix{sub}, Overlaps: []swarmops.OverlapFinding{
		{Kind: swarmops.OverlapHostIPs, Severity: swarmops.OverlapHigh, Subnet: sub, Hosts: 2,
			Message: "10.0.1.0/24 contains host IPs: 10.0.1.2 (a), 10.0.1.3 (b)"},
		{Kind: swarmops.OverlapNetwork, Severity: swarmops.OverlapWarn, Subnet: sub, Network: "secret_net",
			Message: "10.0.1.0/24 overlaps network secret_net (10.0.1.128/25)"},
		{Kind: swarmops.OverlapNetwork, Severity: swarmops.OverlapWarn, Subnet: sub, Network: "web_net",
			Message: "10.0.1.0/24 overlaps network web_net (10.0.1.0/25)"},
	}}
	r := networkRow(n, []string{"shop_web"}, map[string]bool{"shop_net": true, "web_net": true})
	all := strings.Join(r.Overlaps, "\n")
	for _, leak := range []string{"secret_net", "10.0.1.128/25", "10.0.1.2", "(a)"} {
		if strings.Contains(all, leak) {
			t.Errorf("findings reveal %q:\n%s", leak, all)
		}
	}
	for _, want := range []string{"10.0.1.0/24 contains 2 host IPs", "10.0.1.0/24 overlaps another network (not visible)", "overlaps network web_net"} {
		if !strings.Contains(all, want) {
			t.Errorf("findings lack %q:\n%s", want, all)
		}
	}
	if r.OverlapSeverity != "high" || r.Remedy == "" {
		t.Errorf("row = %+v", r)
	}
}
