// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"net/netip"
	"strings"
	"testing"
)

func overlayCluster(specs ...ServiceSpecText) Cluster {
	return Cluster{
		Networks: []Network{{
			Name:    "dbnet",
			Driver:  "overlay",
			Subnets: []netip.Prefix{netip.MustParsePrefix("10.0.1.0/24")},
		}},
		Services: specs,
	}
}

// The outage this check is written from: a service pinned its database to an
// overlay IP, the network was recreated, every address moved, and the service
// crash-looped while everything that used an alias came back untouched.
func TestPinnedOverlayAddressIsFound(t *testing.T) {
	c := overlayCluster(ServiceSpecText{
		Name: "gl_gitlab",
		Text: "GITLAB_OMNIBUS_CONFIG=gitlab_rails['db_host'] = '10.0.1.118'",
	})
	got := (pinnedOverlayAddressAnalyzer{}).AnalyzeCluster(c)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	f := got[0]
	if f.Subject.Kind != SubjectService || f.Subject.Name != "gl_gitlab" {
		t.Errorf("subject = %+v, want the service", f.Subject)
	}
	if f.Finding.Severity != SevHigh {
		t.Errorf("severity = %v; a silent startup failure after an unrelated change is high", f.Finding.Severity)
	}
	for _, want := range []string{"10.0.1.118", "alias"} {
		if !strings.Contains(f.Finding.Detail, want) {
			t.Errorf("detail %q does not mention %q", f.Finding.Detail, want)
		}
	}
}

// An alias is the correct form and must never be flagged — otherwise the check
// marks the fix as the problem.
func TestAliasIsNotFlagged(t *testing.T) {
	c := overlayCluster(ServiceSpecText{
		Name: "gl_gitlab",
		Text: "GITLAB_OMNIBUS_CONFIG=gitlab_rails['db_host'] = 'haproxy'",
	})
	if got := (pinnedOverlayAddressAnalyzer{}).AnalyzeCluster(c); len(got) != 0 {
		t.Errorf("an alias was flagged: %+v", got)
	}
}

// Addresses the swarm does not own are legitimate things to write down. A
// check that flagged them would be noise, and noise is how a real finding gets
// scrolled past.
func TestOnlyOverlayAddressesCount(t *testing.T) {
	c := overlayCluster(ServiceSpecText{
		Name: "app",
		Text: "UPSTREAM=203.0.113.9 BIND=127.0.0.1 NODE=10.9.9.9 OFFICE=192.168.7.4",
	})
	if got := (pinnedOverlayAddressAnalyzer{}).AnalyzeCluster(c); len(got) != 0 {
		t.Errorf("non-overlay addresses were flagged: %+v", got)
	}
}

// A version number is not an address.
func TestVersionStringsAreNotAddresses(t *testing.T) {
	c := overlayCluster(ServiceSpecText{
		Name: "app",
		Text: "IMAGE=registry/app:1.10.0.11 BUILD=10.0.1.5.2",
	})
	if got := (pinnedOverlayAddressAnalyzer{}).AnalyzeCluster(c); len(got) != 0 {
		t.Errorf("a version was read as an address: %+v", got)
	}
}

// Several addresses in one spec are reported once, sorted, so the finding does
// not change text between runs.
func TestSeveralAddressesAreOneSortedFinding(t *testing.T) {
	c := overlayCluster(ServiceSpecText{
		Name: "app",
		Text: "A=10.0.1.30 B=10.0.1.7 C=10.0.1.30",
	})
	got := (pinnedOverlayAddressAnalyzer{}).AnalyzeCluster(c)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1", len(got))
	}
	if !strings.Contains(got[0].Finding.Detail, "10.0.1.7, 10.0.1.30") {
		t.Errorf("addresses not deduplicated and sorted: %q", got[0].Finding.Detail)
	}
	if !strings.Contains(got[0].Finding.Detail, "belong to") {
		t.Errorf("plural not agreed: %q", got[0].Finding.Detail)
	}
}

// Without gathered services, or without known subnets, the check says nothing
// rather than guessing.
func TestQuietWithoutTheFactsItNeeds(t *testing.T) {
	if got := (pinnedOverlayAddressAnalyzer{}).AnalyzeCluster(Cluster{}); len(got) != 0 {
		t.Errorf("an empty cluster produced findings: %+v", got)
	}
	noSubnets := Cluster{
		Networks: []Network{{Name: "dbnet", Driver: "overlay"}},
		Services: []ServiceSpecText{{Name: "app", Text: "DB=10.0.1.118"}},
	}
	if got := (pinnedOverlayAddressAnalyzer{}).AnalyzeCluster(noSubnets); len(got) != 0 {
		t.Errorf("flagged without knowing any subnet: %+v", got)
	}
}
