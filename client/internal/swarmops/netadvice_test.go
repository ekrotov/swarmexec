// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"net/netip"
	"strings"
	"testing"
)

// The measured case this check exists for: a 1450-byte underlay, an overlay
// left at Docker's default. 1500 > 1450, so every full-size frame is dropped
// and nothing says so.
func TestAdviseMTUNamesTheDefaultAsTheProblem(t *testing.T) {
	a := AdviseMTU(1450, 0)
	if a.Problem == "" {
		t.Fatal("a 1500-byte default on a 1450 underlay must be reported")
	}
	if a.Option != 1450 || a.Inside != 1400 {
		t.Errorf("advice = option %d inside %d, want 1450/1400", a.Option, a.Inside)
	}
	if !strings.Contains(a.Problem, "1450") || !strings.Contains(a.Problem, "dropped") {
		t.Errorf("problem must name the underlay and the silent drop: %q", a.Problem)
	}
	if got := a.OptionFlag(); got != "com.docker.network.driver.mtu=1450" {
		t.Errorf("OptionFlag = %q", got)
	}
}

// The option states the UNDERLAY's MTU, not the container's. Getting that
// backwards is the easy mistake: it costs 50 bytes a packet and looks right.
func TestAdviseMTUOptionIsTheUnderlayNotTheInside(t *testing.T) {
	a := AdviseMTU(1450, 1400) // what a cautious operator sets
	if a.Problem != "" {
		t.Errorf("1400 fits a 1450 underlay, no problem expected: %q", a.Problem)
	}
	if a.Option != 1450 {
		t.Errorf("Option = %d, want the underlay's own 1450", a.Option)
	}
	if a.Inside != 1400 {
		t.Errorf("Inside = %d, want 1450-50", a.Inside)
	}
}

// A plain 1500 LAN needs no advice and must not be given a problem.
func TestAdviseMTUQuietOnAnOrdinaryLAN(t *testing.T) {
	if a := AdviseMTU(1500, 0); a.Problem != "" {
		t.Errorf("1500 underlay with the default must be quiet: %q", a.Problem)
	}
}

// Unknown is not 1500. Assuming it would recreate the exact assumption that
// breaks a cloud network, and the operator would believe it.
func TestAdviseMTUSaysNothingWhenTheUnderlayIsUnknown(t *testing.T) {
	a := AdviseMTU(0, 0)
	if a.UnderlayMTU != 0 || a.Option != 0 || a.Problem != "" {
		t.Errorf("unknown underlay must yield no advice, got %+v", a)
	}
	if a.OptionFlag() != "" {
		t.Error("no option flag without a known underlay")
	}
}

func TestSuggestSubnetAvoidsEverythingKnown(t *testing.T) {
	nodes := []NodeAddr{
		{Node: "n1", Addr: netip.MustParseAddr("10.0.1.2")},
		{Node: "n2", Addr: netip.MustParseAddr("10.0.1.3")},
	}
	underlay := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")}
	nets := []Network{
		{Name: "ingress", Subnets: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}},
		{Name: "gateway", Subnets: []netip.Prefix{netip.MustParsePrefix("10.0.2.0/24")}},
	}
	pool := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}

	a := SuggestSubnet(24, nodes, underlay, nets, pool)
	if !a.Subnet.IsValid() {
		t.Fatal("a proposal must be found: the private ranges are far from full")
	}
	if a.Subnet.Bits() != 24 {
		t.Errorf("proposal %s does not have the requested size", a.Subnet)
	}
	if !subnetIsFree(a.Subnet, nodes, underlay, nets, pool) {
		t.Errorf("proposal %s collides with something it was meant to avoid", a.Subnet)
	}
	// Specifically: never inside the default pool, which is where the whole
	// problem comes from.
	if pool[0].Overlaps(a.Subnet) {
		t.Errorf("proposal %s is inside the default pool", a.Subnet)
	}
	d := a.Describe()
	for _, want := range []string{a.Subnet.String(), "node address", "underlay"} {
		if !strings.Contains(d, want) {
			t.Errorf("description %q does not mention %q", d, want)
		}
	}
}

// The proposal has to step over a range that is already taken, not stop at it.
func TestSuggestSubnetStepsOverTakenRanges(t *testing.T) {
	var nets []Network
	for i := range 8 {
		nets = append(nets, Network{
			Name:    "n",
			Subnets: []netip.Prefix{netip.MustParsePrefix("10.40." + itoa(i) + ".0/24")},
		})
	}
	a := SuggestSubnet(24, nil, nil, nets, nil)
	if !a.Subnet.IsValid() {
		t.Fatal("no proposal")
	}
	for _, n := range nets {
		if n.Subnets[0].Overlaps(a.Subnet) {
			t.Fatalf("proposal %s reuses the taken %s", a.Subnet, n.Subnets[0])
		}
	}
}

// A size the caller did not state falls back to /24 rather than to something
// unusable.
func TestSuggestSubnetDefaultsToASaneSize(t *testing.T) {
	if got := SuggestSubnet(0, nil, nil, nil, nil); got.Subnet.Bits() != 24 {
		t.Errorf("bits 0 gave %s, want a /24", got.Subnet)
	}
	if got := SuggestSubnet(31, nil, nil, nil, nil); got.Subnet.Bits() != 24 {
		t.Errorf("bits 31 gave %s, want a /24", got.Subnet)
	}
}

func itoa(i int) string { return string(rune('0' + i)) }
