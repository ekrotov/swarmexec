// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"swarmexec/client/internal/swarmops"

	"github.com/rivo/tview"
)

// The view-side halves of the swarmops tests: how the TUI renders what
// swarmops returns.

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

func TestNetworkType(t *testing.T) {
	cases := []struct {
		n    swarmops.Network
		want string
	}{
		{swarmops.Network{Ingress: true}, "ingress"},
		{swarmops.Network{Internal: true}, "internal"},
		{swarmops.Network{Attachable: true}, "attachable"},
		{swarmops.Network{Driver: "overlay"}, "overlay"},
		{swarmops.Network{Driver: "bridge"}, "bridge"},
		{swarmops.Network{}, "-"},
		// Attachable wins when several flags are set (matches the colour priority).
		{swarmops.Network{Ingress: true, Internal: true, Attachable: true}, "attachable"},
		{swarmops.Network{Ingress: true, Internal: true}, "internal"},
	}
	for _, c := range cases {
		if got := networkType(c.n); got != c.want {
			t.Errorf("networkType(%+v) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestNodeResourceSection(t *testing.T) {
	n := swarmops.NodeInfo{
		Hostname: "host-a", NanoCPUs: 4 * nanoCPU, MemoryBytes: 8 << 30,
		ReservedNanoCPUs: 2 * nanoCPU, ReservedMemoryBytes: 4 << 30,
	}
	got := nodeResourceSection(n)
	if !strings.Contains(got, "free") {
		t.Errorf("section should report what is left:\n%s", got)
	}
	// Half booked → neither the empty nor the full bar.
	if !strings.Contains(got, "█") || !strings.Contains(got, "░") {
		t.Errorf("bar should be partially filled:\n%s", got)
	}

	// A node reporting no capacity must say so rather than divide by zero.
	empty := nodeResourceSection(swarmops.NodeInfo{Hostname: "x"})
	if !strings.Contains(empty, "no capacity") {
		t.Errorf("a capacity-less node should be called out:\n%s", empty)
	}

	// Over-commitment must be visible, and "free" must not go negative.
	over := nodeResourceSection(swarmops.NodeInfo{
		Hostname: "host-b", NanoCPUs: nanoCPU, MemoryBytes: 1 << 30,
		ReservedNanoCPUs: 3 * nanoCPU, ReservedMemoryBytes: 3 << 30,
	})
	if !strings.Contains(over, "over-committed") {
		t.Errorf("over-commitment should be flagged:\n%s", over)
	}
	if strings.Contains(over, "-") && strings.Contains(over, "(-") {
		t.Errorf("free must not render negative:\n%s", over)
	}

	// Unreserved tasks are called out, so 0%% does not read as "idle".
	withBare := nodeResourceSection(swarmops.NodeInfo{
		Hostname: "host-c", NanoCPUs: 4 * nanoCPU, MemoryBytes: 8 << 30,
		TasksWithoutReservation: 3,
	})
	if !strings.Contains(withBare, "no reservation") {
		t.Errorf("unreserved tasks should be called out:\n%s", withBare)
	}
}

func TestResourceBarBounds(t *testing.T) {
	if got := resourceBar(0, 100); strings.Count(got, "█") != 0 {
		t.Errorf("empty bar has %d filled cells, want 0", strings.Count(got, "█"))
	}
	if got := resourceBar(100, 100); strings.Count(got, "█") != resourceBarWidth {
		t.Errorf("full bar has %d filled cells, want %d", strings.Count(got, "█"), resourceBarWidth)
	}
	// Over-committed must clamp, not overflow the bar.
	if got := resourceBar(500, 100); strings.Count(got, "█") != resourceBarWidth {
		t.Errorf("over-committed bar has %d filled cells, want %d", strings.Count(got, "█"), resourceBarWidth)
	}
	if got := resourceBar(1, 0); strings.Count(got, "█") != 0 {
		t.Errorf("zero capacity must not panic or fill: %q", got)
	}
}

// "(no stack)" is a display grouping, not a stack. Nothing carries that label,
// so removing it would either match nothing or — if anyone ever named a stack
// that — match the wrong thing entirely.
func TestRemoveStackRefusesTheUnstackedBucket(t *testing.T) {
	// No manager client at all: reaching the API would panic.
	u := &ui{app: tview.NewApplication(), help: tview.NewTextView()}
	u.openRemoveStack(noStackLabel, nil, nil)

	if got := u.help.GetText(true); !strings.Contains(got, "no stack label") {
		t.Errorf("footer = %q, want it to explain why nothing happened", got)
	}
}

// nanoCPU is one CPU in the NanoCPUs unit swarm reports capacity in.
const nanoCPU = 1_000_000_000

// Copying a network row yields the bare address — the value you paste into a
// curl — and the network name when the attachment has none.
func TestNetRowCopy(t *testing.T) {
	if got := netRowCopy(swarmops.InspectLine{Text: "frontend-net", Addr: "10.0.1.2/24"}); got != "10.0.1.2" {
		t.Errorf("net row copy = %q, want the bare vip", got)
	}
	if got := netRowCopy(swarmops.InspectLine{Text: "frontend-net"}); got != "frontend-net" {
		t.Errorf("net row copy without an address = %q, want the name", got)
	}
}
