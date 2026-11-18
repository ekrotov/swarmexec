// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"strings"
	"testing"
)

// logs_read when the agents cannot be reached: every stream failing is an
// error result (it used to be a success holding only error notes), a partial
// failure still returns the lines that did arrive.
func TestMCPLogsReadAllStreamsFailIsAnError(t *testing.T) {
	isolateDockerConfig(t)
	read := func(down ...string) mcpOutcome {
		fx := newMCPFixture(t, "none", false)
		for _, d := range down {
			fx.down[d] = true
		}
		cs := mcpSession(t, fx)
		return callTool(t, cs, mcpCall{"logs_read", map[string]any{"service": "shop_web"}})
	}

	if got := read("node-b:7000"); got.isError || !strings.Contains(got.text, "hello from c-web1") {
		t.Errorf("one of two down = %q (error %v), want the lines that arrived", got.text, got.isError)
	}
	got := read("node-a:7000", "node-b:7000")
	if !got.isError || !strings.Contains(got.text, "2 of 2 log streams failed") {
		t.Errorf("all down = %q (error %v), want an error result", got.text, got.isError)
	}
}
