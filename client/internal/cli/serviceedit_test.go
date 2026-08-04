// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/swarm"
)

func TestParseServicePort(t *testing.T) {
	ok := []struct {
		in   string
		want string // via formatServicePort
	}{
		{"8080:80/tcp", "8080:80/tcp"},
		{"8080:80", "8080:80/tcp"}, // default proto
		{"53:53/udp", "53:53/udp"},
		{" 443 : 8443 / tcp ", "443:8443/tcp"},
	}
	for _, c := range ok {
		p, err := parseServicePort(c.in)
		if err != nil {
			t.Errorf("parseServicePort(%q) error: %v", c.in, err)
			continue
		}
		if got := formatServicePort(p); got != c.want {
			t.Errorf("parseServicePort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "8080", "abc:80", "8080:xyz", "0:80", "8080:0", "8080:80/foo"} {
		if _, err := parseServicePort(bad); err == nil {
			t.Errorf("parseServicePort(%q) should have errored", bad)
		}
	}
}

func TestPortsFromStrings_DuplicateRejected(t *testing.T) {
	if _, err := portsFromStrings([]string{"8080:80/tcp", "8080:81/tcp"}); err == nil {
		t.Error("duplicate published port/proto should be rejected")
	}
	// Same published port but different protocol is fine.
	if _, err := portsFromStrings([]string{"53:53/tcp", "53:53/udp"}); err != nil {
		t.Errorf("tcp+udp on same port should be allowed: %v", err)
	}
}

func TestParseLabel(t *testing.T) {
	k, v, err := parseLabel("tier=frontend")
	if err != nil || k != "tier" || v != "frontend" {
		t.Errorf("parseLabel = %q/%q/%v", k, v, err)
	}
	if _, v, err := parseLabel("empty="); err != nil || v != "" {
		t.Errorf("empty value should be allowed, got v=%q err=%v", v, err)
	}
	for _, bad := range []string{"nokey", "=v", "  =v"} {
		if _, _, err := parseLabel(bad); err == nil {
			t.Errorf("parseLabel(%q) should have errored", bad)
		}
	}
}

func TestLabelsFromStrings_DuplicateRejected(t *testing.T) {
	m, err := labelsFromStrings([]string{"a=1", "b=2"})
	if err != nil || m["a"] != "1" || m["b"] != "2" {
		t.Errorf("labelsFromStrings = %v err=%v", m, err)
	}
	if _, err := labelsFromStrings([]string{"a=1", "a=2"}); err == nil {
		t.Error("duplicate label key should be rejected")
	}
}

func TestParseServiceMount(t *testing.T) {
	ok := []struct {
		in       string
		typ      mount.Type
		src, tgt string
		ro       bool
	}{
		{"volume:pgdata:/var/lib/pg", mount.TypeVolume, "pgdata", "/var/lib/pg", false},
		{"bind:/opt/data:/data:ro", mount.TypeBind, "/opt/data", "/data", true},
		{" bind:/etc/app:/etc/app ", mount.TypeBind, "/etc/app", "/etc/app", false},
	}
	for _, c := range ok {
		m, err := parseServiceMount(c.in)
		if err != nil {
			t.Errorf("parseServiceMount(%q) error: %v", c.in, err)
			continue
		}
		if m.Type != c.typ || m.Source != c.src || m.Target != c.tgt || m.ReadOnly != c.ro {
			t.Errorf("parseServiceMount(%q) = %+v", c.in, m)
		}
		// round-trip
		if got, _ := parseServiceMount(formatServiceMount(m)); got != m {
			t.Errorf("round-trip failed for %q: %+v", c.in, got)
		}
	}
	for _, bad := range []string{
		"", "volume:onlytwo", "nope:x:/y", // bad type
		"bind:relative:/data", // bind source not absolute
		"volume:pgdata:rel",   // target not absolute
		"volume::/data",       // empty source
	} {
		if _, err := parseServiceMount(bad); err == nil {
			t.Errorf("parseServiceMount(%q) should have errored", bad)
		}
	}
}

func TestMountsFromStrings_DuplicateTarget(t *testing.T) {
	if _, err := mountsFromStrings([]string{"volume:a:/data", "bind:/x:/data"}); err == nil {
		t.Error("duplicate mount target should be rejected")
	}
	if _, err := mountsFromStrings([]string{"volume:a:/data", "bind:/x:/other"}); err != nil {
		t.Errorf("distinct targets should be fine: %v", err)
	}
}

func TestNodeMatchesConstraint(t *testing.T) {
	var n swarm.Node
	n.ID = "nodeid1"
	n.Spec.Role = swarm.NodeRoleWorker
	n.Spec.Labels = map[string]string{"zone": "eu"}
	n.Description.Hostname = "host-a"
	n.Description.Platform.OS = "linux"
	n.Description.Engine.Labels = map[string]string{"storage": "ssd"}

	cases := []struct {
		c              string
		matches, known bool
	}{
		{"node.role==worker", true, true},
		{"node.role==manager", false, true},
		{"node.hostname==host-a", true, true},
		{"node.hostname!=host-b", true, true},
		{"node.hostname!=host-a", false, true},
		{"node.labels.zone==eu", true, true},
		{"node.labels.zone==us", false, true},
		{"node.platform.os==linux", true, true},
		{"engine.labels.storage==ssd", true, true},
		{"node.something==x", false, false}, // unknown key
		{"node.role", false, false},         // no operator
	}
	for _, tc := range cases {
		m, k := nodeMatchesConstraint(n, tc.c)
		if m != tc.matches || k != tc.known {
			t.Errorf("nodeMatchesConstraint(%q) = (%v,%v), want (%v,%v)", tc.c, m, k, tc.matches, tc.known)
		}
	}
}
