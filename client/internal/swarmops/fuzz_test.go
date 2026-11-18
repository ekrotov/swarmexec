// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import "testing"

// Every string the service editors take is typed by an operator and ends up in
// a ServiceSpec that is sent to the manager. A bad one must be an error here,
// not a panic and not a half-filled spec.
func FuzzServiceEditParsers(f *testing.F) {
	for _, s := range []string{
		"", "8080:80", "8080:80/udp host", "0:80", "80:0", "80:80/icmp", "a:b", "/", ":",
		"KEY=VALUE", "=v", "k=", "k=v=w", "512m", "1.5", "0", "-1", "9999999999999999999g", "2.5g",
		"type=volume,source=data,target=/data", "type=bind,source=/a,target=/b,readonly", "a=b,c", ",,,",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if p, err := ParseServicePort(s); err == nil && (p.PublishedPort == 0 || p.TargetPort == 0) {
			t.Fatalf("ParseServicePort(%q) = %+v without an error", s, p)
		}
		if k, _, err := ParseEnv(s); err == nil && k == "" {
			t.Fatalf("ParseEnv(%q) accepted an empty key", s)
		}
		if k, _, err := ParseLabel(s); err == nil && k == "" {
			t.Fatalf("ParseLabel(%q) accepted an empty key", s)
		}
		if n, err := ParseMemBytes(s); err == nil && n < 0 {
			t.Fatalf("ParseMemBytes(%q) = %d", s, n)
		}
		if n, err := ParseCPUCores(s); err == nil && n < 0 {
			t.Fatalf("ParseCPUCores(%q) = %d", s, n)
		}
		_, _ = ParseServiceMount(s)
		_, _ = ParseKVList(s)
	})
}
