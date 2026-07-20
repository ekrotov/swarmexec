// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

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
			local, remote, err := parsePortSpec(tc.spec)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parsePortSpec(%q) = (%d,%d), want error", tc.spec, local, remote)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortSpec(%q): %v", tc.spec, err)
			}
			if local != tc.wantLocal || remote != tc.wantRem {
				t.Errorf("= (%d,%d), want (%d,%d)", local, remote, tc.wantLocal, tc.wantRem)
			}
		})
	}
}

// The default bind address must stay loopback: a forward lifts an internal
// cluster port onto the operator's machine, and binding every interface would
// re-expose it to their local network.
func TestPortForwardCmd_DefaultsToLoopback(t *testing.T) {
	cmd := newPortForwardCmd(&globalFlags{})
	got, err := cmd.Flags().GetString("address")
	if err != nil {
		t.Fatalf("address flag: %v", err)
	}
	if got != "127.0.0.1" {
		t.Errorf("default --address = %q, want 127.0.0.1", got)
	}
}

func TestPortForwardCmd_RequiresTargetAndPort(t *testing.T) {
	cmd := newPortForwardCmd(&globalFlags{})
	for _, args := range [][]string{{}, {"svc"}, {"svc", "8080", "extra"}} {
		if err := cmd.Args(cmd, args); err == nil {
			t.Errorf("args %v accepted, want rejection", args)
		}
	}
	if err := cmd.Args(cmd, []string{"svc", "8080"}); err != nil {
		t.Errorf("args [svc 8080] rejected: %v", err)
	}
}
