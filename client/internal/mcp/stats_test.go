// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"swarmexec/client/internal/swarmops"
)

// Characterization of the stats tool beyond the warm-up tests: the
// all-services view, an unknown service, and a memory-only reading.
func TestMCPStatsScopeAndPartialReadings(t *testing.T) {
	isolateDockerConfig(t)
	fx := newMCPFixture(t, "none", false)
	fx.agent.coldCalls = 1 // readings without CPU first
	fx.srv.statsWait = 0   // ask once
	cs := mcpSession(t, fx)

	if got := callTool(t, cs, mcpCall{"stats", map[string]any{"service": "no_such_svc"}}); !got.isError {
		t.Errorf("unknown service = %q, want an error", got.text)
	}

	got := callTool(t, cs, mcpCall{"stats", map[string]any{"service": "shop_web"}})
	var rows []mcpStatsRow
	if err := json.Unmarshal([]byte(got.text), &rows); got.isError || err != nil {
		t.Fatalf("%v:\n%s", err, got.text)
	}
	// Memory is there, CPU (a delta) is not yet: the row shows the memory
	// and says the CPU is warming up.
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, r := range rows {
		if r.CPU != swarmops.StatsWarmingUp || r.Note != swarmops.StatsWarmingUpNote || !strings.HasPrefix(r.Memory, "64MiB / 256MiB") || r.Health != "healthy" {
			t.Errorf("row %+v: want a memory reading with CPU warming up", r)
		}
	}

	all := callTool(t, cs, mcpCall{"stats", map[string]any{}})
	if all.isError || !strings.Contains(all.text, `"shop_web"`) || !strings.Contains(all.text, `"shop_api"`) {
		t.Errorf("all services = %s", all.text)
	}
	for _, hidden := range []string{"shop_hidden", "shop_off", "ghost_x"} {
		if strings.Contains(all.text, hidden) {
			t.Errorf("all services shows %s:\n%s", hidden, all.text)
		}
	}
}
