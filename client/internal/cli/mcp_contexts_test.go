// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"swarmexec/client/internal/dockerctx"
)

// TestMCPContextsRestricted checks that the assistant gets the context the
// server was started with and the ones --contexts names — and that every other
// context, production first among them, is as absent as one that does not
// exist: not listed, no address, the same error, and never connected to.
func TestMCPContextsRestricted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")
	for name, host := range map[string]string{
		"staging": "ssh://ops@staging.example",
		"lab":     "tcp://lab.example:2376",
		"prod":    "ssh://ops@prod-manager.example",
	} {
		if err := dockerctx.Create(dockerctx.CreateOptions{Name: name, Host: host}); err != nil {
			t.Fatal(err)
		}
	}
	// docker's own current context is production: that must not leak either.
	if err := dockerctx.Use("prod"); err != nil {
		t.Fatal(err)
	}

	extra, err := parseMCPContexts([]string{"lab", " lab ", ""})
	if err != nil || !slices.Equal(extra, []string{"lab"}) {
		t.Fatalf("parseMCPContexts = %v, %v", extra, err)
	}
	if _, err := parseMCPContexts([]string{"no-such-context"}); err == nil {
		t.Error("--contexts accepted a context that does not exist")
	}
	if _, err := parseMCPContexts([]string{"default"}); err != nil {
		t.Errorf("--contexts default: %v", err)
	}

	fx := newMCPFixture(t, "none", false)
	fx.srv.g.dockerContext = "staging"
	fx.srv.allowContexts(extra)
	cs := mcpSession(t, fx)

	list := callTool(t, cs, mcpCall{"contexts_list", map[string]any{}})
	if list.isError || !strings.Contains(list.text, `"staging"`) || !strings.Contains(list.text, `"lab"`) {
		t.Fatalf("contexts_list does not list the allowed contexts:\n%s", list.text)
	}
	for _, leak := range []string{"prod", `"default"`, "docker_current"} {
		if strings.Contains(list.text, leak) {
			t.Errorf("contexts_list leaks %q:\n%s", leak, list.text)
		}
	}

	for _, tool := range []string{"cluster_overview", "services_list", "stats", "doctor"} {
		got := map[string]mcpOutcome{}
		for _, name := range []string{"prod", "default", "nope"} {
			got[name] = callTool(t, cs, mcpCall{tool, map[string]any{"context": name}})
		}
		for _, name := range []string{"prod", "default"} {
			want := strings.ReplaceAll(got["nope"].text, `"nope"`, `"`+name+`"`)
			if !got[name].isError || got[name].text != want {
				t.Errorf("%s on context %q = %q, want the unknown-context answer %q", tool, name, got[name].text, want)
			}
		}
		for _, name := range []string{"staging", "lab", ""} {
			if r := callTool(t, cs, mcpCall{tool, map[string]any{"context": name}}); r.isError {
				t.Errorf("%s on allowed context %q failed: %s", tool, name, r.text)
			}
		}
	}
	for _, name := range fx.connected {
		if name != "" && name != "lab" {
			t.Errorf("the server connected to context %q", name)
		}
	}
}

// TestMCPStatsWaitsForWarmup is the regression test for stats that answered
// "unavailable" on every call: an agent's sampler sleeps when nobody asks, so
// the first answer an assistant gets is empty, and CPU needs one more reading
// after that. The tool waits for it.
func TestMCPStatsWaitsForWarmup(t *testing.T) {
	isolateDockerConfig(t)
	fx := newMCPFixture(t, "none", false)
	// shop_web runs on two nodes, so one round is two requests: one round
	// empty, one with memory only, then the full reading.
	fx.agent.coldCalls = 2
	cs := mcpSession(t, fx)

	got := callTool(t, cs, mcpCall{"stats", map[string]any{"service": "shop_web"}})
	if got.isError || !strings.Contains(got.text, "0.12 / 4.00 cores") || !strings.Contains(got.text, "64MiB / 256MiB") {
		t.Fatalf("stats did not wait for the sampler:\n%s", got.text)
	}
	for _, bad := range []string{statsWarmingUp, statsNoAgent} {
		if strings.Contains(got.text, bad) {
			t.Errorf("stats says %q after waiting:\n%s", bad, got.text)
		}
	}
	fx.agent.mu.Lock()
	calls := fx.agent.statsCalls
	fx.agent.mu.Unlock()
	if calls < 5 {
		t.Errorf("stats asked the agent %d times; a cold sampler needs more", calls)
	}
}

// TestMCPStatsWarmingUpVsUnavailable checks that a reading that is not there
// YET is told apart from an agent that did not answer at all.
func TestMCPStatsWarmingUpVsUnavailable(t *testing.T) {
	isolateDockerConfig(t)
	fx := newMCPFixture(t, "none", false)
	fx.agent.coldCalls = 1000 // never warms up within the test
	fx.srv.statsWait = 0      // ask once
	fx.down["node-b:7000"] = true
	cs := mcpSession(t, fx)

	got := callTool(t, cs, mcpCall{"stats", map[string]any{"service": "shop_web"}})
	if got.isError {
		t.Fatal(got.text)
	}
	var rows []mcpStatsRow
	if err := json.Unmarshal([]byte(got.text), &rows); err != nil {
		t.Fatalf("%v:\n%s", err, got.text)
	}
	byNode := map[string]mcpStatsRow{}
	for _, r := range rows {
		byNode[r.Node] = r
	}
	if r := byNode["node-a"]; r.CPU != statsWarmingUp || r.Note != statsWarmingUpNote {
		t.Errorf("node-a (agent answered, sampler cold) = %+v, want warming up", r)
	}
	if r := byNode["node-b"]; r.CPU != statsNoAgent || r.Note != statsNoAgentNote {
		t.Errorf("node-b (agent down) = %+v, want unavailable", r)
	}
}
