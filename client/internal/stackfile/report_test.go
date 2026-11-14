// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"bytes"
	"strings"
	"testing"

	"swarmexec/client/internal/secscan"
)

func TestNameFromPath(t *testing.T) {
	for in, want := range map[string]string{
		"web.yml":           "web",
		"prod/web.yaml":     "web",
		`C:\stacks\db.json`: "db",
		"noext":             "noext",
		".yml":              ".yml", // nothing before the extension: keep the name
	} {
		if got := NameFromPath(in); got != want {
			t.Errorf("NameFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Nothing above informational still names what was checked, so "nothing
// found" is not a claim with nothing behind it.
func TestWritePlanReportClean(t *testing.T) {
	var b bytes.Buffer
	WritePlanReport(&b, &Plan{Path: "web.yml", Services: []PlannedService{
		{Name: "web", Findings: []secscan.Finding{{Title: "info", Severity: secscan.SevLow}}},
	}, Unsupported: []string{"build"}})
	out := b.String()
	for _, want := range []string{"swarm ignores these keys in web.yml: build", "1 service(s) checked", "Checked: "} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// Flagged services are listed with their findings, informational ones left
// out.
func TestWritePlanReportFlagged(t *testing.T) {
	var b bytes.Buffer
	WritePlanReport(&b, &Plan{Path: "web.yml", Services: []PlannedService{
		{Name: "web", Findings: []secscan.Finding{
			{Title: "runs as root", Detail: "no user set", Severity: secscan.SevHigh},
			{Title: "chatty", Detail: "informational", Severity: secscan.SevLow},
		}},
		{Name: "db"},
	}})
	out := b.String()
	if !strings.Contains(out, "1 of 2 service(s) flagged in web.yml") || !strings.Contains(out, "runs as root — no user set") {
		t.Errorf("report:\n%s", out)
	}
	if strings.Contains(out, "chatty") {
		t.Errorf("an informational finding was listed:\n%s", out)
	}
}
