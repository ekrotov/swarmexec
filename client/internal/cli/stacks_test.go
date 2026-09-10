package cli

import (
	"testing"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/secscan"
)

func svcIn(stack, name string, running, desired int) resolve.Service {
	return resolve.Service{Name: name, Stack: stack, Running: running, Desired: desired, Mode: "replicated"}
}

func TestGroupByStack(t *testing.T) {
	svcs := []resolve.Service{
		svcIn("web", "web_api", 2, 3),
		svcIn("", "standalone", 1, 1), // no stack label
		svcIn("db", "db_pg", 1, 1),
		svcIn("web", "web_front", 1, 1),
	}
	got := groupByStack(svcs)

	// Stacks sort by name; the unstacked bucket sinks to the bottom.
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	want := []string{"db", "web", noStackLabel}
	if len(names) != len(want) {
		t.Fatalf("stacks = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("stack[%d] = %q, want %q (all: %v)", i, names[i], want[i], names)
		}
	}

	// Aggregates and member ordering.
	web := got[1]
	if len(web.Services) != 2 {
		t.Fatalf("web has %d services, want 2", len(web.Services))
	}
	if web.Services[0].Name != "web_api" || web.Services[1].Name != "web_front" {
		t.Errorf("web services not sorted by name: %+v", web.Services)
	}
	if web.Running != 3 || web.Desired != 4 {
		t.Errorf("web tasks = %d/%d, want 3/4", web.Running, web.Desired)
	}
}

// Every service must land in a row — a service with no stack label is grouped,
// never dropped.
func TestGroupByStackAccountsForEveryService(t *testing.T) {
	svcs := []resolve.Service{
		svcIn("a", "a1", 1, 1),
		svcIn("", "loose1", 1, 1),
		svcIn("   ", "loose2", 1, 1), // whitespace-only label counts as unstacked
	}
	total := 0
	for _, r := range groupByStack(svcs) {
		total += len(r.Services)
	}
	if total != len(svcs) {
		t.Errorf("grouped %d services, want %d", total, len(svcs))
	}
	rows := groupByStack(svcs)
	last := rows[len(rows)-1]
	if last.Name != noStackLabel || len(last.Services) != 2 {
		t.Errorf("unstacked bucket = %q with %d services, want %q with 2", last.Name, len(last.Services), noStackLabel)
	}
}

// The per-stack counters summarise the state the operator scans for.
func TestGroupByStackCounters(t *testing.T) {
	updating := svcIn("s", "a", 1, 1)
	updating.UpdateState = "updating"
	paused := svcIn("s", "b", 1, 1)
	paused.UpdateState = "paused" // sticky historical state — must NOT count
	risky := svcIn("s", "c", 1, 1)
	risky.Risks = []secscan.Finding{{Rule: "root-user", Severity: secscan.SevHigh}}
	lowOnly := svcIn("s", "d", 1, 1)
	lowOnly.Risks = []secscan.Finding{{Rule: "root-user", Severity: secscan.SevLow}} // informational
	ported := svcIn("s", "e", 1, 1)
	ported.Ports = "*:80->80/tcp"

	got := groupByStack([]resolve.Service{updating, paused, risky, lowOnly, ported})
	if len(got) != 1 {
		t.Fatalf("got %d stacks, want 1", len(got))
	}
	st := got[0]
	if st.Updating != 1 {
		t.Errorf("Updating = %d, want 1 (a sticky 'paused' must not count)", st.Updating)
	}
	if st.Risky != 1 {
		t.Errorf("Risky = %d, want 1 (a low-only finding must not count)", st.Risky)
	}
	if st.Ports != 1 {
		t.Errorf("Ports = %d, want 1", st.Ports)
	}
}

func TestGroupByStackEmpty(t *testing.T) {
	if got := groupByStack(nil); len(got) != 0 {
		t.Errorf("groupByStack(nil) = %+v, want empty", got)
	}
}
