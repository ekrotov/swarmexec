// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"testing"

	"swarmexec/client/internal/secscan"
)

func planWith(findings ...[]secscan.Finding) *Plan {
	p := &Plan{Stack: "demo", Path: "demo.yml"}
	for i, f := range findings {
		p.Services = append(p.Services, PlannedService{Name: string(rune('a' + i)), Findings: f})
	}
	return p
}

func find(sev secscan.Severity, rule string) secscan.Finding {
	return secscan.Finding{Rule: rule, Title: rule, Severity: sev}
}

// The gate uses the SAME threshold as the shield in the tree. If they differed,
// a file could pass the gate and then be flagged the moment it is running —
// and the operator would have been told something false on the way in.
func TestGateThresholdMatchesTheTreeBadge(t *testing.T) {
	informational := planWith([]secscan.Finding{find(secscan.SevLow, "root-user")})
	if informational.Actionable() {
		t.Error("a low-only finding must not stop a deploy; it is true of nearly every service")
	}
	// ...and secscan agrees, which is the point.
	if secscan.Actionable(informational.Services[0].Findings) {
		t.Error("the plan and secscan disagree about what counts")
	}

	real := planWith([]secscan.Finding{find(secscan.SevHigh, "secret-in-env")})
	if !real.Actionable() {
		t.Error("a high finding must stop a deploy")
	}
}

// Worst-first, so the reader meets the thing that matters before the rest.
func TestFlaggedIsWorstFirst(t *testing.T) {
	p := planWith(
		[]secscan.Finding{find(secscan.SevMedium, "added-capability")},
		[]secscan.Finding{find(secscan.SevLow, "unpinned-image")},
		[]secscan.Finding{find(secscan.SevHigh, "docker-socket")},
	)
	got := p.Flagged()
	if len(got) != 2 {
		t.Fatalf("flagged %d service(s), want the two above informational", len(got))
	}
	if secscan.MaxSeverity(got[0].Findings) != secscan.SevHigh {
		t.Errorf("first flagged service = %v, want the high one", got[0].Name)
	}
	if p.Worst() != secscan.SevHigh {
		t.Errorf("Worst() = %v, want high", p.Worst())
	}
}

// An empty plan must not read as dangerous, and must not read as checked
// either — both are handled by the caller, so the primitives have to be honest.
func TestEmptyPlanIsNotActionable(t *testing.T) {
	p := planWith()
	if p.Actionable() {
		t.Error("a plan with no services cannot be actionable")
	}
	if len(p.Flagged()) != 0 {
		t.Error("nothing to flag")
	}
	if p.Worst() != secscan.SevLow {
		t.Errorf("Worst() = %v, want the zero value", p.Worst())
	}
}

// The summary is shared by the terminal and the ui so the two cannot word the
// same deploy differently.
func TestApplySummary(t *testing.T) {
	if got := (&ApplyResult{}).Summary(); got != "nothing to do" {
		t.Errorf("empty summary = %q", got)
	}
	r := &ApplyResult{Created: []string{"a"}, Updated: []string{"b", "c"}, Removed: []string{"d"}}
	got := r.Summary()
	for _, want := range []string{"1 service(s) created", "2 updated", "1 service(s) removed"} {
		if !contains(got, want) {
			t.Errorf("summary %q does not mention %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
