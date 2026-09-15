// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"strings"
	"testing"
)

// rules returns the rule ids a scan produced, for asserting on what fired
// without pinning the wording of every detail line.
func rules(fs []SubjectFinding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Finding.Rule)
	}
	return out
}

func has(fs []SubjectFinding, rule string) bool {
	for _, f := range fs {
		if f.Finding.Rule == rule {
			return true
		}
	}
	return false
}

func findingFor(t *testing.T, fs []SubjectFinding, rule string) SubjectFinding {
	t.Helper()
	for _, f := range fs {
		if f.Finding.Rule == rule {
			return f
		}
	}
	t.Fatalf("no %q finding in %v", rule, rules(fs))
	return SubjectFinding{}
}

// Overlay traffic between nodes is unencrypted unless asked for, so a network
// carrying services without it is the finding. The two exclusions matter as
// much as the rule: an overlay with nothing on it carries nothing, and ingress
// cannot be encrypted at all — flagging either would put noise in every report,
// and the ingress one could never be cleared by anybody.
func TestNetworkEncryptionAnalyzer(t *testing.T) {
	got := ScanCluster(Cluster{Networks: []Network{
		{Name: "app", Driver: "overlay", Services: []string{"web", "db"}},
		{Name: "secure", Driver: "overlay", Encrypted: true, Services: []string{"web"}},
		{Name: "empty", Driver: "overlay"},
		{Name: "ingress", Driver: "overlay", Ingress: true, Services: []string{"web"}},
		{Name: "bridgey", Driver: "bridge", Services: []string{"web"}},
	}})

	var flagged []string
	for _, f := range got {
		if f.Finding.Rule == "network-unencrypted" {
			flagged = append(flagged, f.Subject.Name)
		}
	}
	if len(flagged) != 1 || flagged[0] != "app" {
		t.Errorf("flagged %v, want only the unencrypted overlay carrying services", flagged)
	}

	f := findingFor(t, got, "network-unencrypted")
	if f.Subject.Kind != SubjectNetwork {
		t.Errorf("subject kind = %q, want network", f.Subject.Kind)
	}
	// The detail must name who is affected, or the reader cannot judge it.
	if !strings.Contains(f.Finding.Detail, "web") || !strings.Contains(f.Finding.Detail, "db") {
		t.Errorf("detail should name the attached services: %q", f.Finding.Detail)
	}
}

// A long service list is summarised rather than spilled into a table cell.
func TestServiceListSummarises(t *testing.T) {
	if got := serviceList([]string{"a", "b", "c", "d", "e"}); !strings.Contains(got, "3 more") {
		t.Errorf("serviceList = %q, want the tail summarised", got)
	}
	if got := serviceList([]string{"only"}); got != "only" {
		t.Errorf("serviceList = %q, want the bare name", got)
	}
	// Order must not depend on the caller's slice order, or two scans of an
	// unchanged cluster produce different text.
	if a, b := serviceList([]string{"b", "a"}), serviceList([]string{"a", "b"}); a != b {
		t.Errorf("serviceList is order-dependent: %q vs %q", a, b)
	}
}

// Autolock has three states, not two. Unknown must be reported as unknown:
// assuming "off" invents a finding, and assuming "on" is a false all-clear
// about the thing protecting every secret in the cluster.
func TestAutolockAnalyzerThreeStates(t *testing.T) {
	off := ScanCluster(Cluster{Swarm: &SwarmInfo{AutoLockManagers: false}})
	if !has(off, "autolock-disabled") {
		t.Errorf("autolock off should be reported, got %v", rules(off))
	}

	on := ScanCluster(Cluster{Swarm: &SwarmInfo{AutoLockManagers: true}})
	if has(on, "autolock-disabled") || has(on, "autolock-unknown") {
		t.Errorf("autolock on should be silent, got %v", rules(on))
	}

	unknown := ScanCluster(Cluster{Swarm: nil})
	if !has(unknown, "autolock-unknown") {
		t.Errorf("an unreadable swarm config must say so, got %v", rules(unknown))
	}
	if has(unknown, "autolock-disabled") {
		t.Error("unknown must not be reported as disabled")
	}
}

// Nobody asked is not the same as nobody answered. With AgentsChecked false the
// analyzer stays silent and the report carries a gap note instead; inventing
// per-node findings there would report a problem that does not exist.
func TestAgentAnalyzerSilentWhenNotChecked(t *testing.T) {
	// A known-good swarm, so anything reported here came from the agent check.
	c := Cluster{
		Nodes:         []Node{{Name: "n1"}},
		ClientVersion: "v1.0.0",
		Swarm:         &SwarmInfo{AutoLockManagers: true},
	}
	if got := ScanCluster(c); len(got) != 0 {
		t.Errorf("unchecked agents produced %v, want nothing", rules(got))
	}
}

func TestAgentAnalyzerClassifies(t *testing.T) {
	c := Cluster{
		AgentsChecked: true,
		ClientVersion: "v1.16.2",
		ClientProto:   "swarmexec/v1",
		Nodes: []Node{
			{Name: "matching", AgentVersion: "v1.16.2", AgentProto: "swarmexec/v1"},
			{Name: "skewed", AgentVersion: "v1.15.0", AgentProto: "swarmexec/v1"},
			{Name: "otherproto", AgentVersion: "v1.16.2", AgentProto: "swarmexec/v2"},
			{Name: "ancient", AgentTooOld: true},
			{Name: "silent", AgentError: "dial tcp: i/o timeout"},
		},
	}
	got := ScanCluster(c)

	want := map[string]string{
		"skewed":     "agent-version-skew",
		"otherproto": "agent-proto-mismatch",
		"ancient":    "agent-too-old",
		"silent":     "agent-unreachable",
	}
	for _, f := range got {
		if f.Subject.Kind != SubjectNode {
			continue
		}
		if w, ok := want[f.Subject.Name]; !ok {
			t.Errorf("node %q should not be flagged (%s)", f.Subject.Name, f.Finding.Rule)
		} else if f.Finding.Rule != w {
			t.Errorf("node %q = %q, want %q", f.Subject.Name, f.Finding.Rule, w)
		} else {
			delete(want, f.Subject.Name)
		}
	}
	if len(want) != 0 {
		t.Errorf("never flagged: %v", want)
	}

	// A disagreement about the contract outranks a disagreement about the build.
	if f := findingFor(t, got, "agent-proto-mismatch"); f.Finding.Severity != SevHigh {
		t.Errorf("proto mismatch = %v, want high — the two ends disagree about the contract", f.Finding.Severity)
	}
	// An unreachable node is a gap, not a verdict, and must not read as one.
	f := findingFor(t, got, "agent-unreachable")
	if !strings.Contains(f.Finding.Detail, "gap") {
		t.Errorf("an unreachable agent must be stated as a gap: %q", f.Finding.Detail)
	}
	// The dial error arrives with whitespace in it and must stay one line.
	if strings.ContainsAny(f.Finding.Detail, "\n\r") {
		t.Errorf("detail must be one line: %q", f.Finding.Detail)
	}
}

// A local build calls itself "dev" and differs from every released agent by
// construction. Comparing it would put a finding on every node of every cluster
// a developer scans — the same exemption doctor makes.
func TestAgentSkewSkipsUnreleasedClient(t *testing.T) {
	for _, v := range []string{"dev", ""} {
		c := Cluster{
			AgentsChecked: true,
			ClientVersion: v,
			Nodes:         []Node{{Name: "n1", AgentVersion: "v1.16.0"}},
		}
		if got := ScanCluster(c); has(got, "agent-version-skew") {
			t.Errorf("client %q should not be compared against agents, got %v", v, rules(got))
		}
	}
}

// A rotated-but-not-removed secret is still live in the cluster. Configs get
// their own rule so a report can tell clutter from a stale credential.
func TestUnusedCredentials(t *testing.T) {
	got := ScanCluster(Cluster{
		Secrets: []Credential{{Name: "used", Services: []string{"web"}}, {Name: "stale"}},
		Configs: []Credential{{Name: "live", Services: []string{"web"}}, {Name: "leftover"}},
	})
	if !has(got, "unused-secret") || !has(got, "unused-config") {
		t.Fatalf("expected both unused rules, got %v", rules(got))
	}
	if f := findingFor(t, got, "unused-secret"); f.Subject.Name != "stale" {
		t.Errorf("flagged secret %q, want the unreferenced one", f.Subject.Name)
	}
	if f := findingFor(t, got, "unused-config"); f.Subject.Name != "leftover" {
		t.Errorf("flagged config %q, want the unreferenced one", f.Subject.Name)
	}
}

// The order must depend only on the findings, so two reports from an unchanged
// cluster can be diffed against each other.
func TestScanClusterOrderIsStable(t *testing.T) {
	c := Cluster{
		Networks: []Network{
			{Name: "zeta", Driver: "overlay", Services: []string{"s"}},
			{Name: "alpha", Driver: "overlay", Services: []string{"s"}},
		},
		Secrets: []Credential{{Name: "b"}, {Name: "a"}},
		Swarm:   &SwarmInfo{},
	}
	first := ScanCluster(c)

	// Worst first: the mediums (networks, autolock) before the low secrets.
	for i := 1; i < len(first); i++ {
		if first[i-1].Finding.Severity < first[i].Finding.Severity {
			t.Fatalf("findings are not worst-first at %d: %v", i, rules(first))
		}
	}
	// Within a severity, by subject name — not by input order.
	var nets []string
	for _, f := range first {
		if f.Subject.Kind == SubjectNetwork {
			nets = append(nets, f.Subject.Name)
		}
	}
	if len(nets) != 2 || nets[0] != "alpha" {
		t.Errorf("network order = %v, want sorted regardless of input order", nets)
	}

	second := ScanCluster(c)
	if len(first) != len(second) {
		t.Fatalf("two scans of the same cluster differ in length")
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("scan is not repeatable at %d: %+v vs %+v", i, first[i], second[i])
		}
	}
}

// Every cluster analyzer must survive a zero Cluster: a partially gathered
// cluster is normal, and a panic here would take the whole report with it.
func TestScanClusterToleratesEmptyInput(t *testing.T) {
	got := ScanCluster(Cluster{})
	// Only the unknown-autolock finding, which is the honest answer for a
	// cluster we know nothing about.
	if len(got) != 1 || got[0].Finding.Rule != "autolock-unknown" {
		t.Errorf("empty cluster = %v, want only autolock-unknown", rules(got))
	}
}

// "Nothing found" is only informative if the report can say what it looked for.
func TestClusterChecksAreDescribed(t *testing.T) {
	checks := ClusterChecks()
	if len(checks) == 0 {
		t.Fatal("no cluster check describes itself")
	}
	for _, c := range checks {
		if strings.TrimSpace(c) == "" {
			t.Error("a check description is empty")
		}
	}
}
