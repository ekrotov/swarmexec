// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"

	"swarmexec/client/internal/portfwd"
)

// The default bind address must stay loopback: a forward lifts an internal
// cluster port onto the operator's machine, and binding every interface would
// re-expose it to their local network.
func TestPortForwardCmd_DefaultsToLoopback(t *testing.T) {
	cmd := newPortForwardCmd(&globalFlags{})
	got, err := cmd.Flags().GetString("address")
	if err != nil {
		t.Fatalf("address flag: %v", err)
	}
	if got != portfwd.DefaultAddress {
		t.Errorf("default --address = %q, want %q", got, portfwd.DefaultAddress)
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
