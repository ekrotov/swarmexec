// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"strings"
	"testing"
	"time"
)

func fixedMeta() ReportMeta {
	return ReportMeta{
		Context:   "prod",
		Generated: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
		Tool:      "swarmexec v1.16.2",
	}
}

// A report is read away from the terminal, by someone who was not there when it
// ran. It has to say which cluster, when, and with what — without that it is an
// unattributable list of weaknesses.
func TestMarkdownCarriesItsProvenance(t *testing.T) {
	md := BuildReport(fixedMeta(), nil, Cluster{Swarm: &SwarmInfo{AutoLockManagers: true}}).Markdown()
	for _, want := range []string{"# Security report — prod", "2026-09-15T10:00:00Z", "swarmexec v1.16.2"} {
		if !strings.Contains(md, want) {
			t.Errorf("report does not state %q:\n%s", want, md)
		}
	}
}

// A pipe in a detail would split the table cell it lands in, and a newline
// would end the row. Neither is hypothetical: a finding can quote an
// environment variable name, and Docker puts no restrictions on those at all.
func TestMarkdownCellsCannotBreakTheTable(t *testing.T) {
	svcs := []ServiceScan{{
		Name: "web",
		Findings: []Finding{{
			Rule:     "secret-env",
			Title:    "secret in env | var",
			Detail:   "the variable A|B is set\nacross two lines",
			Severity: SevHigh,
		}},
	}}
	md := BuildReport(fixedMeta(), svcs, Cluster{Swarm: &SwarmInfo{}}).Markdown()

	// Find the row and check it is still one row with the right cell count.
	var row string
	for _, l := range strings.Split(md, "\n") {
		if strings.Contains(l, "secret in env") {
			row = l
		}
	}
	if row == "" {
		t.Fatalf("finding row missing:\n%s", md)
	}
	if strings.Contains(row, "A|B") {
		t.Errorf("an unescaped pipe survived into a cell: %q", row)
	}
	if !strings.Contains(row, `A\|B`) {
		t.Errorf("the pipe should be escaped, not dropped: %q", row)
	}
	// "| sev | title | detail |" is four pipes' worth of structure; any extra
	// unescaped one would add a column.
	if n := strings.Count(row, "|") - strings.Count(row, `\|`); n != 4 {
		t.Errorf("row has %d structural pipes, want 4: %q", n, row)
	}
}

// The gaps are the part a reader cannot supply for themselves. A report that
// quietly omits what it could not reach reads as an all-clear for ground it
// never covered.
func TestMarkdownStatesWhatItCouldNotCover(t *testing.T) {
	r := BuildReport(fixedMeta(), nil, Cluster{}) // no agents asked, no swarm read, no nodes
	md := r.Markdown()
	if !strings.Contains(md, "## Not covered") {
		t.Fatalf("report has no gaps section:\n%s", md)
	}
	for _, want := range []string{"agents were not queried", "swarm configuration", "No nodes"} {
		if !strings.Contains(md, want) {
			t.Errorf("gaps do not mention %q:\n%s", want, md)
		}
	}

	// With everything gathered there is nothing to disclaim.
	full := BuildReport(fixedMeta(), nil, Cluster{
		AgentsChecked: true,
		Swarm:         &SwarmInfo{AutoLockManagers: true},
		Nodes:         []Node{{Name: "n1"}},
	})
	if strings.Contains(full.Markdown(), "## Not covered") {
		t.Error("a complete scan should not disclaim anything")
	}
}

// "No findings" is a claim. It is only worth anything next to what was looked
// for, which is why the check list is always present.
func TestMarkdownAlwaysNamesTheChecks(t *testing.T) {
	md := BuildReport(fixedMeta(), nil, Cluster{
		AgentsChecked: true,
		Swarm:         &SwarmInfo{AutoLockManagers: true},
		Nodes:         []Node{{Name: "n1"}},
	}).Markdown()
	if !strings.Contains(md, "## What was checked") {
		t.Fatalf("no check list:\n%s", md)
	}
	for _, c := range append(Checks(), ClusterChecks()...) {
		if !strings.Contains(md, c) {
			t.Errorf("check %q is not listed:\n%s", c, md)
		}
	}
}

// Low findings hold for almost every service, so a bare total misrepresents how
// much there is to do. The summary has to separate the two.
func TestSummarySeparatesActionableFromInformational(t *testing.T) {
	svcs := []ServiceScan{
		{Name: "a", Findings: []Finding{{Rule: "root-user", Title: "no user set", Severity: SevLow}}},
		{Name: "b", Findings: []Finding{{Rule: "docker-socket", Title: "socket", Severity: SevHigh}}},
	}
	r := BuildReport(fixedMeta(), svcs, Cluster{AgentsChecked: true, Swarm: &SwarmInfo{AutoLockManagers: true}, Nodes: []Node{{Name: "n"}}})
	if r.Totals.High != 1 || r.Totals.Low != 1 {
		t.Fatalf("totals = %+v, want one of each", r.Totals)
	}
	md := r.Markdown()
	if !strings.Contains(md, "**1** finding(s) above low") {
		t.Errorf("summary does not separate actionable findings:\n%s", md)
	}

	// With nothing above low, say so rather than leading with a total that
	// reads as alarming.
	only := BuildReport(fixedMeta(), svcs[:1], Cluster{AgentsChecked: true, Swarm: &SwarmInfo{AutoLockManagers: true}, Nodes: []Node{{Name: "n"}}})
	if !strings.Contains(only.Markdown(), "Nothing above **low**") {
		t.Errorf("an all-low report should say so:\n%s", only.Markdown())
	}
}

// Services with no findings are counted but not listed — otherwise the report
// is mostly empty headings and the findings are lost among them.
func TestCleanServicesAreCountedNotListed(t *testing.T) {
	svcs := []ServiceScan{
		{Name: "clean"},
		{Name: "dirty", Findings: []Finding{{Rule: "r", Title: "t", Severity: SevMedium}}},
	}
	r := BuildReport(fixedMeta(), svcs, Cluster{})
	if r.ServiceCount != 2 {
		t.Errorf("ServiceCount = %d, want both services counted", r.ServiceCount)
	}
	if len(r.Services) != 1 || r.Services[0].Subject.Name != "dirty" {
		t.Errorf("listed %d group(s), want only the flagged service", len(r.Services))
	}
	if !strings.Contains(r.Markdown(), "Scanned 2 services") {
		t.Errorf("the count must include the clean ones:\n%s", r.Markdown())
	}
}

// Worst first, so the reader meets what matters before the housekeeping.
func TestGroupsAreWorstFirst(t *testing.T) {
	svcs := []ServiceScan{
		{Name: "low-one", Findings: []Finding{{Rule: "a", Severity: SevLow}}},
		{Name: "high-one", Findings: []Finding{{Rule: "b", Severity: SevHigh}}},
		{Name: "med-one", Findings: []Finding{{Rule: "c", Severity: SevMedium}}},
	}
	r := BuildReport(fixedMeta(), svcs, Cluster{})
	var order []string
	for _, g := range r.Services {
		order = append(order, g.Subject.Name)
	}
	want := []string{"high-one", "med-one", "low-one"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("group order = %v, want %v", order, want)
		}
	}
}

// A name containing a backtick would end the code span the heading puts it in
// and let the rest of the line become markup. Docker names cannot contain one
// today; the report must not depend on that staying true.
func TestHeadingNamesCannotEscapeTheirCodeSpan(t *testing.T) {
	svcs := []ServiceScan{{Name: "we`ird", Findings: []Finding{{Rule: "r", Severity: SevHigh}}}}
	md := BuildReport(fixedMeta(), svcs, Cluster{}).Markdown()
	if strings.Contains(md, "we`ird") {
		t.Errorf("a backtick survived into a heading:\n%s", md)
	}
}
