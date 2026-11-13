// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"reflect"
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
	for name, got := range map[string][]rmResult{
		"remove": removeOnNodes(ctx, config.Config{}, nodes, "data", false, time.Second),
		"create": createVolumeOnNodes(ctx, config.Config{}, nodes, nil, time.Second),
	} {
		if len(got) != len(nodes) {
			t.Fatalf("%s: %d results for %d nodes", name, len(got), len(nodes))
		}
		for i, r := range got {
			if r.node.Name != nodes[i].Name || !errors.Is(r.err, fanout.ErrSkipped) {
				t.Errorf("%s: result %d = %+v, want node %s with ErrSkipped", name, i, r, nodes[i].Name)
			}
		}
	}
	vs, errs := indexVolumes(ctx, config.Config{}, nodes, time.Second)
	if len(vs) != 0 || !errors.Is(errs["n1"], fanout.ErrSkipped) || !errors.Is(errs["n2"], fanout.ErrSkipped) {
		t.Fatalf("indexVolumes: vols=%v errs=%v, want none and both nodes skipped", vs, errs)
	}
}

func TestBuildVolumeCreateReq(t *testing.T) {
	// Happy path: name trimmed, driver + labels carried through.
	req, err := buildVolumeCreateReq(newVolumeOpts{
		Name:   "  data ",
		Driver: " local ",
		Labels: map[string]string{"team": "infra"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.GetName() != "data" {
		t.Errorf("name = %q, want trimmed %q", req.GetName(), "data")
	}
	if req.GetDriver() != "local" {
		t.Errorf("driver = %q, want trimmed %q", req.GetDriver(), "local")
	}
	if !reflect.DeepEqual(req.GetLabels(), map[string]string{"team": "infra"}) {
		t.Errorf("labels = %v", req.GetLabels())
	}

	// Empty driver is left empty (the agent/daemon defaults it to "local").
	req, err = buildVolumeCreateReq(newVolumeOpts{Name: "d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.GetDriver() != "" {
		t.Errorf("driver = %q, want empty", req.GetDriver())
	}

	// Missing name is an error.
	if _, err := buildVolumeCreateReq(newVolumeOpts{Name: "   "}); err == nil {
		t.Error("expected error for empty name")
	}
}
