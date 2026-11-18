// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package portfwd

import (
	"strings"
	"testing"
)

func TestParsePortSpec(t *testing.T) {
	tests := []struct {
		spec               string
		wantLocal, wantRem uint32
		wantErr            string
	}{
		{spec: "8080", wantLocal: 8080, wantRem: 8080},
		{spec: "9090:8080", wantLocal: 9090, wantRem: 8080},
		{spec: "1", wantLocal: 1, wantRem: 1},
		{spec: "65535", wantLocal: 65535, wantRem: 65535},
		{spec: " 9090 : 8080 ", wantLocal: 9090, wantRem: 8080},

		{spec: "0", wantErr: "1-65535"},
		{spec: "65536", wantErr: "1-65535"},
		{spec: "http", wantErr: "1-65535"},
		{spec: "", wantErr: "1-65535"},
		{spec: "9090:0", wantErr: "remote"},
		{spec: "0:8080", wantErr: "local"},
		{spec: "9090:http", wantErr: "remote"},
	}

	for _, tc := range tests {
		t.Run(tc.spec, func(t *testing.T) {
			local, remote, err := ParsePortSpec(tc.spec)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParsePortSpec(%q) = (%d,%d), want error", tc.spec, local, remote)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePortSpec(%q): %v", tc.spec, err)
			}
			if local != tc.wantLocal || remote != tc.wantRem {
				t.Errorf("= (%d,%d), want (%d,%d)", local, remote, tc.wantLocal, tc.wantRem)
			}
		})
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"":            true, // empty means the default
		"127.0.0.1":   true,
		"127.1.2.3":   true, // all of 127.0.0.0/8
		"::1":         true,
		"[::1]":       true,
		"localhost":   true,
		"LocalHost":   true,
		" 127.0.0.1":  true,
		"0.0.0.0":     false,
		"::":          false,
		"192.168.1.5": false,
		"myhost":      false,
	} {
		if got := IsLoopback(addr); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestBrowseHost(t *testing.T) {
	for addr, want := range map[string]string{
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"::":        "::1",
		"127.0.0.1": "127.0.0.1",
		"10.0.0.5":  "10.0.0.5",
	} {
		if got := BrowseHost(addr); got != want {
			t.Errorf("BrowseHost(%q) = %q, want %q", addr, got, want)
		}
	}
}

// The address carries through to everything that used to hard-code 127.0.0.1.
