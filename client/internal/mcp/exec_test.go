// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"strings"
	"testing"

	"swarmexec/client/internal/resolve"
)

// Characterization of exec_run's argument checks, container pick and errors,
// through the real tool over the fixture cluster.
func TestMCPExecRunEdges(t *testing.T) {
	isolateDockerConfig(t)
	fx := newMCPFixture(t, "none", false)
	cs := mcpSession(t, fx)
	call := func(args map[string]any) mcpOutcome {
		if _, ok := args["service"]; !ok {
			args["service"] = "shop_web"
		}
		return callTool(t, cs, mcpCall{"exec_run", args})
	}
	wantErr := func(what string, got mcpOutcome, want string) {
		t.Helper()
		if !got.isError || got.text != want {
			t.Errorf("%s = %q (error %v), want %q", what, got.text, got.isError, want)
		}
	}

	wantErr("no command", call(map[string]any{"command": []string{}, "slot": 1}), "command is required")
	wantErr("blank command", call(map[string]any{"command": []string{"  "}, "slot": 1}), "command is required")
	wantErr("too many args", call(map[string]any{"command": append([]string{"id"}, make([]string, 64)...), "slot": 1}), "command has 65 arguments; at most 64")
	wantErr("unknown service", call(map[string]any{"service": "nope", "command": []string{"id"}}), `no such service "nope"`)
	wantErr("ambiguous", call(map[string]any{"command": []string{"id"}}),
		`service "shop_web" has 2 running containers; pass slot or node: slot 1 on node-a, slot 2 on node-b`)
	wantErr("no match", call(map[string]any{"command": []string{"id"}, "slot": 9}),
		`service "shop_web" has no running container matching that slot/node`)
	wantErr("slot and node disagree", call(map[string]any{"command": []string{"id"}, "slot": 1, "node": "node-b"}),
		`service "shop_web" has no running container matching that slot/node`)

	got := call(map[string]any{"command": []string{"id", "-u"}, "node": "node-b", "timeout_seconds": 400})
	if got.isError || !strings.Contains(got.text, `"node": "node-b"`) || !strings.Contains(got.text, `ran: id -u`) {
		t.Errorf("by node = %q (error %v)", got.text, got.isError)
	}
	if execs, _, _ := fx.agent.snapshot(); len(execs) != 1 || execs[0] != "c-web2" {
		t.Errorf("exec reached %v, want c-web2", execs)
	}

	// The deadline ends the stream at the agent, so what comes back is the
	// agent's DeadlineExceeded status, passed on as is.
	if got := call(map[string]any{"command": []string{"hang"}, "slot": 1, "timeout_seconds": 1}); !got.isError || !strings.Contains(got.text, "DeadlineExceeded") {
		t.Errorf("timeout = %q (error %v)", got.text, got.isError)
	}
}

// execTarget's ambiguity message names a global service's tasks by node.
func TestExecTargetGlobalChoices(t *testing.T) {
	cands := []resolve.Candidate{{NodeName: "node-a", ContainerID: "c1"}, {NodeName: "", ContainerID: "c2"}}
	_, err := execTarget("agent", cands, 0, "")
	if err == nil || err.Error() != `service "agent" has 2 running containers; pass slot or node: node node-a, node -` {
		t.Errorf("err = %v", err)
	}
	cd, err := execTarget("agent", cands, 0, "node-a")
	if err != nil || cd.ContainerID != "c1" {
		t.Errorf("by node = %+v, %v", cd, err)
	}
}
