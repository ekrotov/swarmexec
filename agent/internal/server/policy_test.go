// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// End to end through a real RPC: the policy refuses, the client gets
// PermissionDenied, and the audit record carries the rule and its reason —
// "why was I refused" is half of what a policy is for.
func TestPolicyRefusesAndTheAuditSaysWhy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(`
default: allow
rules:
  - deny: [image.prune.all]
    reason: no sweeping prune on production
`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := auth.LoadPolicy(path, false)
	if err != nil {
		t.Fatal(err)
	}
	d := newFakeDocker()
	srv, log := auditedTestServer(d, p)

	_, err = srv.PruneImages(context.Background(), &pb.PruneImagesRequest{All: true})
	if statusCode(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	rec := findAuditEvent(t, log.String(), "auth_decision")
	if rec["allow"] != false || !strings.Contains(rec["reason"].(string), "no sweeping prune on production") {
		t.Errorf("audit = %v", rec)
	}
	if len(d.pruneFilters) != 0 {
		t.Error("a refused prune must not reach Docker")
	}

	// The safe mode is not covered by the rule and goes through.
	if _, err := srv.PruneImages(context.Background(), &pb.PruneImagesRequest{}); err != nil {
		t.Errorf("untagged-only prune should be allowed: %v", err)
	}
}

func TestLoadPolicyRefusesABrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	_ = os.WriteFile(path, []byte("default: allow\nrules:\n  - deny: [exek]\n"), 0o600)
	if _, err := auth.LoadPolicy(path, false); err == nil || !strings.Contains(err.Error(), "not an action") {
		t.Errorf("a typo must stop the agent, got %v", err)
	}
}
