// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"errors"
	"testing"
	"time"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/fanout"
	"swarmexec/client/internal/resolve"
)

// A volume operation whose context ended before a node was reached must say so
// for that node: a zero result would read as "removed" (err == nil) on a node
// that was never asked. Nothing is dialled here — the context is already over.
func TestNodeOpsReportUnreachedNodesAsSkipped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	nodes := []resolve.Node{{Name: "n1", DialHost: "n1.invalid"}, {Name: "n2", DialHost: "n2.invalid"}}
	ac := New(config.Config{})
	for name, got := range map[string][]NodeResult{
		"remove": ac.RemoveVolume(ctx, nodes, "data", false, time.Second),
		"create": ac.CreateVolume(ctx, nodes, nil, time.Second),
	} {
		if len(got) != len(nodes) {
			t.Fatalf("%s: %d results for %d nodes", name, len(got), len(nodes))
		}
		for i, r := range got {
			if r.Node.Name != nodes[i].Name || !errors.Is(r.Err, fanout.ErrSkipped) {
				t.Errorf("%s: result %d = %+v, want node %s with ErrSkipped", name, i, r, nodes[i].Name)
			}
		}
	}
	vs, errs := ac.Volumes(ctx, nodes, time.Second)
	if len(vs) != 0 || !errors.Is(errs["n1"], fanout.ErrSkipped) || !errors.Is(errs["n2"], fanout.ErrSkipped) {
		t.Fatalf("Volumes: vols=%v errs=%v, want none and both nodes skipped", vs, errs)
	}
}
