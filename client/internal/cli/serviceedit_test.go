// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import "testing"

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
