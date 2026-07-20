// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// forwardTarget parses args before the real flag set exists, so it carries its
// own tests: getting this wrong would either break sidecar mode or, worse, make
// a normal agent start silently forward instead of serving.
func TestForwardTarget(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantAddr string
		wantOK   bool
	}{
		{"separate value", []string{"-forward-to", "127.0.0.1:8080"}, "127.0.0.1:8080", true},
		{"equals form", []string{"-forward-to=127.0.0.1:8080"}, "127.0.0.1:8080", true},
		{"among other args", []string{"-log-level=debug", "-forward-to", "127.0.0.1:1"}, "127.0.0.1:1", true},
		{"flag without value", []string{"-forward-to"}, "", true},
		{"equals with empty value", []string{"-forward-to="}, "", false},

		{"normal server args", []string{"-port=9443", "-self-signed"}, "", false},
		{"no args", nil, "", false},
		// Must not trip on a flag that merely shares the prefix.
		{"prefix lookalike", []string{"-forward-image=x:1"}, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr, ok := forwardTarget(tc.args)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if addr != tc.wantAddr {
				t.Errorf("addr = %q, want %q", addr, tc.wantAddr)
			}
		})
	}
}

func TestRunForwardSidecar_EmptyAddressFails(t *testing.T) {
	if err := runForwardSidecar(""); err == nil {
		t.Error("empty address accepted, want an error")
	}
}
