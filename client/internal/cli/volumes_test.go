// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"testing"
)

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
