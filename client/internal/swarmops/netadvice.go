// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Advice for a network about to be created: a subnet that collides with
// nothing, and the MTU the overlay has to be told about.
//
// The two belong together because they are the same mistake made twice. A
// swarm overlay is addressed and sized from defaults that assume a plain
// 1500-byte LAN with nothing else in 10.0.0.0/8, and on a cloud network that
// is wrong in both directions at once: the hosts already live in 10.0.x.y, and
// the link carries 1450 bytes, not 1500. Neither failure announces itself —
// the first hands a container an address a node already owns, the second
// drops exactly the packets that are too big to fit, which is to say the
// interesting ones.

// vxlanOverhead is what VXLAN adds to every overlay frame on the wire:
// 20 bytes of outer IPv4, 8 of UDP, 8 of VXLAN and 14 of inner Ethernet.
//
// It is why com.docker.network.driver.mtu is NOT the MTU a container sees.
// The option states the underlay's MTU; Docker subtracts this from it. Set it
// to 1400 on a 1450 link and containers get 1350, which works but wastes 50
// bytes per packet. Set it to the link's own MTU and they get exactly what
// fits.
const vxlanOverhead = 50

// DefaultUnderlayMTU is what Docker assumes when the option is absent, and the
// assumption this whole check exists to question.
const DefaultUnderlayMTU = 1500

// MTUAdvice is what to tell the operator about an overlay's framing.
type MTUAdvice struct {
	// UnderlayMTU is the link MTU the advice was computed from, 0 when it is
	// not known. Everything below is empty when it is 0 — a guess about the
	// underlay is worse than silence, because it would be believed.
	UnderlayMTU int
	// Option is the value com.docker.network.driver.mtu should carry.
	Option int
	// Inside is what a container will then see.
	Inside int
	// Problem is set when the CURRENT setting cannot work: the frames an
	// overlay produces do not fit the underlay, and the difference is dropped
	// silently.
	Problem string
}

// AdviseMTU works out what an overlay should be told, given the underlay's MTU
// and the option the network carries (0 = none, i.e. Docker's 1500).
//
// underlayMTU of 0 means "not known" and yields no advice at all. There is no
// safe default here: assuming 1500 is exactly the assumption that breaks a
// cloud network, and assuming less would invent a problem on a LAN where
// nothing is wrong.
func AdviseMTU(underlayMTU, currentOption int) MTUAdvice {
	if underlayMTU <= vxlanOverhead {
		return MTUAdvice{}
	}
	a := MTUAdvice{
		UnderlayMTU: underlayMTU,
		Option:      underlayMTU,
		Inside:      underlayMTU - vxlanOverhead,
	}
	effective := currentOption
	if effective == 0 {
		effective = DefaultUnderlayMTU
	}
	if effective > underlayMTU {
		inside := effective - vxlanOverhead
		a.Problem = fmt.Sprintf(
			"frames of %d bytes (%d inside the overlay) do not fit a %d-byte underlay: "+
				"the packets that exceed it are dropped without an error, which looks like "+
				"a slow or flaky connection rather than a broken one",
			effective, inside, underlayMTU)
	}
	return a
}

// Option renders the create flag, e.g. "com.docker.network.driver.mtu=1450".
func (a MTUAdvice) OptionFlag() string {
	if a.Option == 0 {
		return ""
	}
	return "com.docker.network.driver.mtu=" + strconv.Itoa(a.Option)
}

// SubnetAdvice is a free subnet, and what it was kept clear of.
type SubnetAdvice struct {
	// Subnet is the proposal, invalid when none was found.
	Subnet netip.Prefix
	// Avoided names what the search stepped around, for the sentence that
	// explains the proposal rather than merely stating it.
	Avoided []string
}

// suggestionPools are the ranges a proposal is drawn from, in order. They are
// deliberately NOT 10.0.0.0/8: that is Docker's own default pool and, on every
// cloud this tool is used on, where the hosts live. 10.40–10.99 keeps a wide
// berth from both, and the 172.16 and 192.168 ranges follow for a site that
// has already spent the 10 range.
var suggestionPools = []netip.Prefix{
	netip.MustParsePrefix("10.40.0.0/13"),
	netip.MustParsePrefix("172.20.0.0/14"),
	netip.MustParsePrefix("192.168.128.0/17"),
}

// SuggestSubnet proposes a subnet of the given prefix length that overlaps
// nothing: no node address, no configured underlay CIDR, no existing network,
// and not the swarm's default address pool.
//
// It proposes rather than enforces on purpose. Whether an overlap is
// acceptable is the operator's call — a lab cluster may well want its overlays
// inside the host range — and a tool that silently picked for them would hide
// the decision instead of informing it.
func SuggestSubnet(bits int, nodes []NodeAddr, underlay []netip.Prefix, nets []Network, pool []netip.Prefix) SubnetAdvice {
	if bits <= 0 || bits > 30 {
		bits = 24
	}
	var avoided []string
	if len(nodes) > 0 {
		avoided = append(avoided, fmt.Sprintf("%d node address(es)", len(nodes)))
	}
	if len(underlay) > 0 {
		avoided = append(avoided, "the configured underlay")
	}
	if n := countSubnets(nets); n > 0 {
		avoided = append(avoided, fmt.Sprintf("%d existing network(s)", n))
	}
	if len(pool) > 0 {
		avoided = append(avoided, "the swarm's default address pool")
	}

	for _, p := range suggestionPools {
		for cand := range candidates(p, bits) {
			if subnetIsFree(cand, nodes, underlay, nets, pool) {
				return SubnetAdvice{Subnet: cand, Avoided: avoided}
			}
		}
	}
	return SubnetAdvice{Avoided: avoided}
}

// candidates yields the subnets of pool at the given prefix length, capped so
// a wide pool cannot turn the search into a scan of millions of ranges. The
// cap is generous: a cluster that has filled 512 candidates in every pool has
// an addressing problem no suggestion will solve.
func candidates(pool netip.Prefix, bits int) func(func(netip.Prefix) bool) {
	return func(yield func(netip.Prefix) bool) {
		if bits < pool.Bits() {
			return
		}
		addr := pool.Masked().Addr()
		for i := 0; i < 512; i++ {
			cand := netip.PrefixFrom(addr, bits)
			if !pool.Contains(addr) {
				return
			}
			if !yield(cand) {
				return
			}
			next, ok := nextSubnet(addr, bits)
			if !ok {
				return
			}
			addr = next
		}
	}
}

// nextSubnet advances to the first address of the following subnet.
func nextSubnet(addr netip.Addr, bits int) (netip.Addr, bool) {
	if !addr.Is4() {
		return netip.Addr{}, false
	}
	b := addr.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	step := uint32(1) << (32 - bits)
	if v+step < v { // wrapped
		return netip.Addr{}, false
	}
	v += step
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

// subnetIsFree reports whether cand collides with nothing known.
func subnetIsFree(cand netip.Prefix, nodes []NodeAddr, underlay []netip.Prefix, nets []Network, pool []netip.Prefix) bool {
	for _, n := range nodes {
		if cand.Contains(n.Addr) {
			return false
		}
	}
	for _, u := range underlay {
		if u.Overlaps(cand) {
			return false
		}
	}
	for _, n := range nets {
		for _, s := range n.Subnets {
			if s.Overlaps(cand) {
				return false
			}
		}
	}
	for _, p := range pool {
		if p.Overlaps(cand) {
			return false
		}
	}
	return true
}

func countSubnets(nets []Network) int {
	n := 0
	for _, net := range nets {
		n += len(net.Subnets)
	}
	return n
}

// Describe renders the advice as the sentence shown under a create form.
func (a SubnetAdvice) Describe() string {
	if !a.Subnet.IsValid() {
		return "no free subnet could be proposed from the usual private ranges"
	}
	s := "suggested subnet " + a.Subnet.String()
	if len(a.Avoided) > 0 {
		sort.Strings(a.Avoided)
		s += " — clear of " + strings.Join(a.Avoided, ", ")
	}
	return s
}
