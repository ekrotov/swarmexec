// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/resolve"
)

func TestKillAndInspectContainer(t *testing.T) {
	fc := newFakeCluster(t, "n1")
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background()
	ep := resolve.Endpoint{DialHost: "n1", ContainerID: "abc"}

	if err := ac.KillContainer(ctx, ep, testTimeout); err != nil {
		t.Fatalf("KillContainer: %v", err)
	}
	a := fc.agents["n1"]
	a.removedMu.Lock()
	killed := slices.Clone(a.removed)
	a.removedMu.Unlock()
	if !slices.Equal(killed, []string{"kill:abc"}) {
		t.Errorf("agent saw %v, want a kill of abc", killed)
	}

	raw, err := ac.InspectContainer(ctx, ep, testTimeout)
	if err != nil || string(raw) != `{"Id":"abc"}` {
		t.Errorf("InspectContainer = %s, %v", raw, err)
	}

	// The agent's refusal reaches the caller with its code intact.
	a.setFail("KillContainer", status.Error(codes.FailedPrecondition, "container abc is not a swarm task"))
	if err := ac.KillContainer(ctx, ep, testTimeout); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("refusal = %v, want FailedPrecondition", err)
	}
}

// An agent that predates the RPCs reads as too old, every time it is asked: an
// explicit operator action is never benched.
func TestKillAndInspectOnAnOldAgent(t *testing.T) {
	fc := newFakeCluster(t, "old")
	fc.agents["old"].setUnimplemented("KillContainer", "InspectContainer")
	ac := New(fc.cfg)
	defer ac.Close()
	ctx := context.Background()
	ep := resolve.Endpoint{DialHost: "old", ContainerID: "abc"}

	for range 2 {
		if err := ac.KillContainer(ctx, ep, testTimeout); !errors.Is(err, agenterr.ErrAgentTooOld) {
			t.Errorf("kill on old agent = %v, want ErrAgentTooOld", err)
		}
		if _, err := ac.InspectContainer(ctx, ep, testTimeout); !errors.Is(err, agenterr.ErrAgentTooOld) {
			t.Errorf("inspect on old agent = %v, want ErrAgentTooOld", err)
		}
	}
	if got := fc.agents["old"].count("KillContainer", 0); got != 2 {
		t.Errorf("old agent asked %d times, want every time", got)
	}

	fc.setDown("old", true)
	if err := ac.KillContainer(ctx, resolve.Endpoint{DialHost: "gone", ContainerID: "abc"}, testTimeout); err == nil {
		t.Error("an unreachable node must fail the kill")
	}
}
