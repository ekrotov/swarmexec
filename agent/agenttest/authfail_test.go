// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agenttest_test

import (
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/agenttest"
	"swarmexec/agent/internal/server"
	"swarmexec/internal/authmeta"
	"swarmexec/internal/pb"
)

type countMetrics struct {
	mu sync.Mutex
	by map[string]int
	server.NopMetrics
}

func (m *countMetrics) AuthFailed(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by == nil {
		m.by = map[string]int{}
	}
	m.by[reason]++
}

func (m *countMetrics) get(reason string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.by[reason]
}

// SEC-04: guessing the shared secret leaves a counted, logged trace; the first
// failure is logged at once and the rest of the burst is one line, not many;
// the credential itself is never printed.
func TestAuth_FailedAuthenticationIsCountedAndLogged(t *testing.T) {
	m := &countMetrics{}
	a := agenttest.Start(t, agenttest.Options{Secret: auditSecret, Metrics: m})
	c, ctx := rawClient(t, a, map[string]string{authmeta.BindingKey: "guess-1"})

	for i := 0; i < 4; i++ {
		if _, err := c.ListVolumes(ctx, &pb.ListVolumesRequest{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("attempt %d = %v, want Unauthenticated", i, err)
		}
	}
	if got := m.get("invalid"); got != 4 {
		t.Errorf("auth failures{invalid} = %d, want 4", got)
	}
	log := a.Log()
	if n := strings.Count(log, "authentication failed"); n != 1 {
		t.Errorf("%d log lines for a burst of 4, want exactly 1 (the first one, not hidden):\n%s", n, log)
	}
	if !strings.Contains(log, "peer=127.0.0.1") || !strings.Contains(log, "reason=invalid") {
		t.Errorf("log line lacks peer or reason:\n%s", log)
	}
	if strings.Contains(log, "guess-1") || strings.Contains(log, auditSecret) {
		t.Errorf("the credential reached the log:\n%s", log)
	}

	// The raw secret against an agent that refuses it is its own reason.
	rc, rctx := rawClient(t, a, legacyCreds("x"))
	if _, err := rc.ListVolumes(rctx, &pb.ListVolumesRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("legacy = %v, want Unauthenticated", err)
	}
	if got := m.get("legacy_refused"); got != 1 {
		t.Errorf("auth failures{legacy_refused} = %d, want 1", got)
	}

	// The agent's own health probe sends no credential from loopback: not an
	// event, or it would drown the counter at one per 10 s.
	pc, pctx := rawClient(t, a, nil)
	if _, err := pc.Version(pctx, &pb.VersionRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("probe = %v, want Unauthenticated", err)
	}
	if got := m.get("missing"); got != 0 {
		t.Errorf("auth failures{missing} = %d from loopback, want 0 (health probe)", got)
	}
}
