// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"strings"
	"testing"
)

// The measured cluster: every node has a public NIC at 1500 and the private
// one the swarm talks over at 1450. The answer has to be 1450 — the number
// Docker never looked at.
func TestDetermineUnderlayMTUPicksTheWireNotTheBridge(t *testing.T) {
	var links []NodeLink
	for _, n := range []string{"docker2v2", "docker3v2", "generic-node-1"} {
		links = append(links,
			NodeLink{Node: n, Name: "lo", MTU: 65536, Operstate: "unknown", ARPType: 772},
			NodeLink{Node: n, Name: "eth0", MTU: 1500, Operstate: "up", ARPType: 1},
			NodeLink{Node: n, Name: "enp7s0", MTU: 1450, Operstate: "up", ARPType: 1},
			NodeLink{Node: n, Name: "docker0", MTU: 1500, Operstate: "up", ARPType: 1},
			NodeLink{Node: n, Name: "docker_gwbridge", MTU: 1500, Operstate: "up", ARPType: 1},
			NodeLink{Node: n, Name: "veth1234", MTU: 1500, Operstate: "up", ARPType: 1},
		)
	}
	got := DetermineUnderlayMTU(links)
	if got.MTU != 1450 {
		t.Fatalf("MTU = %d, want 1450", got.MTU)
	}
	if got.Disagree {
		t.Error("the nodes agree; Disagree must be false")
	}
	if len(got.From) != 3 {
		t.Errorf("From = %v, want one entry per node", got.From)
	}
	d := got.Describe()
	for _, want := range []string{"1450", "enp7s0", "docker2v2"} {
		if !strings.Contains(d, want) {
			t.Errorf("description %q lacks %q", d, want)
		}
	}
}

// Nothing reported is NOT 1500. The description has to say why, because the
// operator's next step depends on it (old agent vs missing mount).
func TestDetermineUnderlayMTUUnknownWithoutLinks(t *testing.T) {
	got := DetermineUnderlayMTU(nil)
	if got.MTU != 0 || got.Candidates != 0 {
		t.Fatalf("got %+v, want nothing determined", got)
	}
	if !strings.Contains(got.Describe(), "no agent reported") {
		t.Errorf("description must explain the absence: %q", got.Describe())
	}
}

// A host covered in bridges and veths must not answer the question with
// Docker's own 1500 — that is the assumption being questioned.
func TestDetermineUnderlayMTUIgnoresVirtualLinks(t *testing.T) {
	links := []NodeLink{
		{Node: "n1", Name: "docker0", MTU: 1500, Operstate: "up", ARPType: 1},
		{Node: "n1", Name: "br-abc", MTU: 1500, Operstate: "up", ARPType: 1},
		{Node: "n1", Name: "vethXY", MTU: 1500, Operstate: "up", ARPType: 1},
		{Node: "n1", Name: "wg0", MTU: 1300, Operstate: "unknown", ARPType: 65534},
	}
	if got := DetermineUnderlayMTU(links); got.MTU != 0 {
		t.Errorf("MTU = %d from virtual links alone, want unknown (%+v)", got.MTU, got)
	}
}

// A down link is not the underlay, however promising its MTU.
func TestDetermineUnderlayMTUSkipsDownLinks(t *testing.T) {
	links := []NodeLink{
		{Node: "n1", Name: "eno1", MTU: 9000, Operstate: "down", ARPType: 1},
		{Node: "n1", Name: "eno2", MTU: 1500, Operstate: "up", ARPType: 1},
	}
	if got := DetermineUnderlayMTU(links); got.MTU != 1500 {
		t.Errorf("MTU = %d, want the link that is up", got.MTU)
	}
}

// Nodes that disagree take the lowest, and say so: an overlay has to fit the
// smallest link that carries it.
func TestDetermineUnderlayMTUTakesTheLowestAndSaysSo(t *testing.T) {
	links := []NodeLink{
		{Node: "a", Name: "ens1", MTU: 1500, Operstate: "up", ARPType: 1},
		{Node: "b", Name: "ens1", MTU: 1400, Operstate: "up", ARPType: 1},
	}
	got := DetermineUnderlayMTU(links)
	if got.MTU != 1400 {
		t.Errorf("MTU = %d, want the lowest", got.MTU)
	}
	if !got.Disagree {
		t.Error("Disagree must be set when the nodes differ")
	}
	if !strings.Contains(got.Describe(), "do not agree") {
		t.Errorf("description must surface the disagreement: %q", got.Describe())
	}
}
