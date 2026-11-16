// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"strings"
	"testing"
)

// Characterization of logs_read's argument handling and output shape,
// through the real tool over the fixture cluster.
func TestMCPLogsReadEdges(t *testing.T) {
	isolateDockerConfig(t)
	fx := newMCPFixture(t, "none", false)
	cs := mcpSession(t, fx)
	call := func(args map[string]any) mcpOutcome {
		args["service"] = "shop_web"
		return callTool(t, cs, mcpCall{"logs_read", args})
	}

	for _, since := range []string{"yesterday", "-5m"} {
		if got := call(map[string]any{"since": since}); !got.isError || !strings.Contains(got.text, "since: want a duration") {
			t.Errorf("since %q = %q (error %v)", since, got.text, got.isError)
		}
	}
	if got := call(map[string]any{"format": "nonsense"}); !got.isError {
		t.Errorf("unknown format accepted: %q", got.text)
	}
	if got := call(map[string]any{"slot": 9}); !got.isError || !strings.Contains(got.text, "no running container in slot 9") {
		t.Errorf("slot 9 = %q (error %v)", got.text, got.isError)
	}
	got := call(map[string]any{"slot": 2, "tail": 999999})
	if got.isError || !strings.Contains(got.text, "hello from c-web2") || strings.Contains(got.text, "c-web1") {
		t.Errorf("slot 2 = %q (error %v)", got.text, got.isError)
	}
	if got := call(map[string]any{"grep": "matches-nothing-at-all"}); got.isError || got.text != "(no log lines matched)\n" {
		t.Errorf("no match = %q (error %v)", got.text, got.isError)
	}
	if got := call(map[string]any{"format": "raw", "tail": -1}); got.isError || !strings.Contains(got.text, "hello from c-web1") {
		t.Errorf("raw = %q (error %v)", got.text, got.isError)
	}
}
