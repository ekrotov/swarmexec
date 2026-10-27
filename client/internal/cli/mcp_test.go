// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"swarmexec/client/internal/mcpaccess"
)

// mcpSession connects an in-memory MCP client to the fixture's server — the
// protocol end to end, minus only the pipe.
func mcpSession(t *testing.T, fx *mcpFixture) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := fx.srv.build().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

type mcpCall struct {
	tool string
	args map[string]any
}

type mcpOutcome struct {
	text    string
	isError bool
}

func callTool(t *testing.T, cs *mcp.ClientSession, c mcpCall) mcpOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
	if err != nil {
		// A protocol-level error is an outcome too, and must be just as clean.
		return mcpOutcome{text: "protocol error: " + err.Error(), isError: true}
	}
	var b strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.StructuredContent != nil {
		j, _ := json.Marshal(res.StructuredContent)
		b.Write(j)
	}
	return mcpOutcome{text: b.String(), isError: res.IsError}
}

// writeStackFiles writes the compose files the stack tools are called with.
func writeStackFiles(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		// Both services the assistant may see; the deployed stack also has
		// shop_hidden and shop_off, which must not surface in the diff.
		"shop.yml": `services:
  web:
    image: registry.example/shop_web:2
    environment:
      PORT: "8080"
      DB_PASSWORD: changed-in-file
    deploy:
      replicas: 2
      labels:
        swarmexec.mcp.enable: "true"
        swarmexec.mcp.logs: "true"
        swarmexec.mcp.exec: "true"
  api:
    image: registry.example/shop_api:1
    deploy:
      labels:
        swarmexec.mcp.enable: "true"
`,
		// "ghost" is deployed, but only as a hidden service; "nostack" is not
		// deployed at all. The two must be indistinguishable.
		"ghost.yml": `services:
  x:
    image: registry.example/ghost_x:1
    deploy:
      labels:
        swarmexec.mcp.enable: "true"
`,
		"nostack.yml": `services:
  x:
    image: registry.example/ghost_x:1
    deploy:
      labels:
        swarmexec.mcp.enable: "true"
`,
		"unlabelled.yml": `services:
  web:
    image: registry.example/shop_web:2
`,
	}
	out := map[string]string{}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		out[name] = p
	}
	return out
}

// isolateDockerConfig keeps contexts_list from reading the developer's own
// docker contexts.
func isolateDockerConfig(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "unix:///fake/docker.sock")
}

// TestMCPNeverRevealsLabelsOrHiddenServices is the regression test for the
// two promises the MCP server makes about access control: the labels that
// grant access never appear in anything the assistant receives, and a service
// it may not see cannot be told apart from one that does not exist.
//
// It calls EVERY tool, against a fake cluster whose services carry the labels
// in every combination, and checks every result and every error. A new tool
// that is not called here fails the test, so it cannot ship unexamined.
func TestMCPNeverRevealsLabelsOrHiddenServices(t *testing.T) {
	isolateDockerConfig(t)
	fx := newMCPFixture(t, "none", true)
	cs := mcpSession(t, fx)
	stacks := writeStackFiles(t)

	services := []string{"shop_web", "shop_api", "shop_hidden", "shop_off", "sock", "ghost_x", "no_such_svc"}
	var calls []mcpCall
	add := func(tool string, args map[string]any) { calls = append(calls, mcpCall{tool, args}) }

	for _, tool := range []string{"contexts_list", "cluster_overview", "services_list", "stats",
		"volumes_list", "networks_list", "secrets_list", "configs_list", "security_report", "doctor"} {
		add(tool, map[string]any{})
		add(tool, map[string]any{"context": "nope"})
	}
	for _, svc := range services {
		add("services_list", map[string]any{"service": svc})
		add("service_inspect", map[string]any{"service": svc})
		add("logs_read", map[string]any{"service": svc})
		add("logs_read", map[string]any{"service": svc, "grep": "boom", "min_level": "warn", "tail": 5, "since": "1h"})
		add("stats", map[string]any{"service": svc})
		add("exec_run", map[string]any{"service": svc, "command": []string{"id"}, "slot": 1})
	}
	for _, f := range []string{"shop.yml", "ghost.yml", "nostack.yml", "unlabelled.yml"} {
		add("stack_diff", map[string]any{"path": stacks[f]})
		add("stack_check", map[string]any{"path": stacks[f]})
	}
	add("stack_diff", map[string]any{"path": "/etc/passwd"})

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	called := map[string]bool{}
	for _, c := range calls {
		called[c.tool] = true
	}
	for _, tl := range tools.Tools {
		if !called[tl.Name] {
			t.Errorf("tool %s is not exercised by this test — add it", tl.Name)
		}
		desc := strings.ToLower(tl.Description + " " + tl.Name)
		if strings.Contains(desc, "label") || strings.Contains(desc, "swarmexec.mcp") {
			t.Errorf("tool %s's description mentions labels: %q", tl.Name, tl.Description)
		}
		schema, _ := json.Marshal(tl.InputSchema)
		if strings.Contains(strings.ToLower(string(schema)), "label") {
			t.Errorf("tool %s's input schema mentions labels: %s", tl.Name, schema)
		}
	}

	results := map[string]mcpOutcome{}
	for _, c := range calls {
		out := callTool(t, cs, c)
		key := c.tool + " " + mustJSON(c.args)
		results[key] = out

		low := strings.ToLower(out.text)
		if strings.Contains(low, "swarmexec.mcp") {
			t.Errorf("%s revealed an access label:\n%s", key, out.text)
		}
		if strings.Contains(out.text, "hunter2") {
			t.Errorf("%s revealed a credential from the environment:\n%s", key, out.text)
		}
		// A hidden name may come back only where the caller supplied it —
		// "no such service \"shop_hidden\"" echoes the question, not the cluster.
		for _, h := range hiddenNames {
			if strings.Contains(out.text, h) && !strings.Contains(key, h) {
				t.Errorf("%s revealed the hidden %q:\n%s", key, h, out.text)
			}
		}
	}

	// No oracle: a hidden or disabled service answers exactly as a missing one.
	for _, tool := range []string{"services_list", "service_inspect", "logs_read", "stats", "exec_run"} {
		for _, args := range calls {
			if args.tool != tool || args.args["service"] != "no_such_svc" {
				continue
			}
			missing := results[tool+" "+mustJSON(args.args)]
			if !missing.isError {
				t.Errorf("%s on a missing service did not fail: %s", tool, missing.text)
			}
			for _, hidden := range []string{"shop_hidden", "shop_off", "ghost_x"} {
				a := cloneArgs(args.args)
				a["service"] = hidden
				got := results[tool+" "+mustJSON(a)]
				if got.isError != missing.isError ||
					strings.ReplaceAll(got.text, hidden, "X") != strings.ReplaceAll(missing.text, "no_such_svc", "X") {
					t.Errorf("%s: %s is distinguishable from a missing service:\n  hidden:  %q\n  missing: %q",
						tool, hidden, got.text, missing.text)
				}
			}
		}
	}

	// The same for stacks: one deployed only as hidden services, one not at all.
	for _, tool := range []string{"stack_diff", "stack_check"} {
		ghost := results[tool+" "+mustJSON(map[string]any{"path": stacks["ghost.yml"]})]
		none := results[tool+" "+mustJSON(map[string]any{"path": stacks["nostack.yml"]})]
		g := strings.NewReplacer(stacks["ghost.yml"], "F", `"ghost"`, `"S"`, "=ghost", "=S", "ghost_", "S_", " ghost", " S").Replace(ghost.text)
		n := strings.NewReplacer(stacks["nostack.yml"], "F", `"nostack"`, `"S"`, "=nostack", "=S", "nostack_", "S_", " nostack", " S").Replace(none.text)
		if ghost.isError != none.isError || g != n {
			t.Errorf("%s: a stack of hidden services is distinguishable from no stack:\n  hidden: %q\n  none:   %q", tool, ghost.text, none.text)
		}
	}

	// Permitted-but-not-granted is a plain refusal that names no label.
	for _, key := range []string{
		"logs_read " + mustJSON(map[string]any{"service": "shop_api"}),
		"exec_run " + mustJSON(map[string]any{"service": "shop_api", "command": []string{"id"}, "slot": 1}),
	} {
		if got := results[key]; !got.isError || got.text != "not permitted" {
			t.Errorf("%s = %q (error %v), want exactly \"not permitted\"", key, got.text, got.isError)
		}
	}

	// The docker-socket service carries every grant and is still refused.
	sock := results["exec_run "+mustJSON(map[string]any{"service": "sock", "command": []string{"id"}, "slot": 1})]
	if !sock.isError || !strings.Contains(sock.text, "Docker daemon") {
		t.Errorf("exec into the docker-socket service was not refused: %q", sock.text)
	}

	// What actually reached the agent: only the permitted containers.
	execs, logs, operators := fx.agent.snapshot()
	if !slices.Equal(execs, []string{"c-web1"}) {
		t.Errorf("exec reached containers %v, want only c-web1", execs)
	}
	for _, id := range logs {
		if id != "c-web1" && id != "c-web2" && id != "c-sock" {
			t.Errorf("logs were read from %s, which the assistant may not read", id)
		}
	}
	for _, op := range operators {
		if op != "alice+mcp" {
			t.Errorf("agent was told operator %q, want alice+mcp", op)
		}
	}
	if len(operators) == 0 {
		t.Error("no operator identity reached the agent")
	}

	// Spot checks that the permitted paths actually work, so the test above
	// cannot pass by everything failing.
	expect := func(key, want string) {
		t.Helper()
		got := results[key]
		if got.isError || !strings.Contains(got.text, want) {
			t.Errorf("%s = %q (error %v), want it to contain %q", key, got.text, got.isError, want)
		}
	}
	expect("service_inspect "+mustJSON(map[string]any{"service": "shop_web"}), "DB_PASSWORD=<masked>")
	expect("service_inspect "+mustJSON(map[string]any{"service": "shop_web"}), "shop_db_pw")
	expect("logs_read "+mustJSON(map[string]any{"service": "shop_web"}), "hello from c-web1")
	expect("logs_read "+mustJSON(map[string]any{"service": "shop_web", "grep": "boom", "min_level": "warn", "tail": 5, "since": "1h"}), "boom")
	expect("exec_run "+mustJSON(map[string]any{"service": "shop_web", "command": []string{"id"}, "slot": 1}), `"exit_code": 3`)
	expect("exec_run "+mustJSON(map[string]any{"service": "shop_web", "command": []string{"id"}, "slot": 1}), `"stderr_truncated": true`)
	expect("services_list "+mustJSON(map[string]any{}), "shop_api")
	expect("secrets_list "+mustJSON(map[string]any{}), "shop_db_pw")
	expect("networks_list "+mustJSON(map[string]any{}), "shop_net")
	expect("volumes_list "+mustJSON(map[string]any{}), "shop_data")
	expect("configs_list "+mustJSON(map[string]any{}), "shop_cfg")
	expect("stats "+mustJSON(map[string]any{}), "64MiB / 256MiB")
	expect("cluster_overview "+mustJSON(map[string]any{}), "node-b")
	expect("doctor "+mustJSON(map[string]any{}), `"healthy": true`)
	expect("security_report "+mustJSON(map[string]any{}), "Docker socket")
	expect("stack_diff "+mustJSON(map[string]any{"path": stacks["shop.yml"]}), "<masked> (differs from deployed)")
	expect("stack_check "+mustJSON(map[string]any{"path": stacks["shop.yml"]}), "secret in environment variable")

	if got := results["stack_diff "+mustJSON(map[string]any{"path": stacks["unlabelled.yml"]})]; !got.isError || !strings.HasPrefix(got.text, "not permitted") {
		t.Errorf("a stack file with a service the assistant may not see was used: %q", got.text)
	}
	if got := results["stack_diff "+mustJSON(map[string]any{"path": "/etc/passwd"})]; !got.isError {
		t.Errorf("stack_diff read a file that is not a stack file: %q", got.text)
	}
	if u := fx.swarm.unknown; len(u) > 0 {
		t.Logf("fake API was asked for paths it does not serve: %v", u)
	}
}

// TestMCPDefaultRead checks the one switch that widens visibility: unlabelled
// services become readable, an explicit "false" still hides, and neither logs
// nor exec ever come from the default.
func TestMCPDefaultRead(t *testing.T) {
	isolateDockerConfig(t)
	fx := newMCPFixture(t, "read", false)
	cs := mcpSession(t, fx)

	list := callTool(t, cs, mcpCall{"services_list", map[string]any{}})
	if !strings.Contains(list.text, "shop_hidden") || !strings.Contains(list.text, "ghost_x") {
		t.Errorf("--default read did not show unlabelled services:\n%s", list.text)
	}
	if strings.Contains(list.text, "shop_off") {
		t.Errorf("--default read showed a service whose label says false:\n%s", list.text)
	}
	if got := callTool(t, cs, mcpCall{"logs_read", map[string]any{"service": "shop_hidden"}}); got.text != "not permitted" {
		t.Errorf("--default read granted logs: %q", got.text)
	}
	if got := callTool(t, cs, mcpCall{"exec_run", map[string]any{"service": "shop_hidden", "command": []string{"id"}}}); got.text != "not permitted" {
		t.Errorf("--default read granted exec: %q", got.text)
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "security_report" {
			t.Error("security_report is offered without --allow report")
		}
	}
}

func TestMCPExecRefusesDockerSocket(t *testing.T) {
	bind := func(src string) swarm.ServiceSpec {
		var s swarm.ServiceSpec
		s.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "app", Mounts: []mount.Mount{{Type: mount.TypeBind, Source: src, Target: "/x"}}}
		return s
	}
	cases := map[string]swarm.Service{
		"socket in spec": {Spec: bind("/var/run/docker.sock")},
		"host /var/run":  {Spec: bind("/var/run")},
		"socket in previous spec": {Spec: bind("/srv"), PreviousSpec: func() *swarm.ServiceSpec {
			s := bind("/run/docker.sock")
			return &s
		}()},
		"agent image": {Spec: func() swarm.ServiceSpec {
			s := bind("/srv")
			s.TaskTemplate.ContainerSpec.Image = "registry.example/swarmexec-agent:1.22"
			return s
		}()},
	}
	for name, svc := range cases {
		if err := execRefusal(svc); err == nil {
			t.Errorf("%s: exec allowed", name)
		} else if strings.Contains(err.Error(), "swarmexec.mcp") {
			t.Errorf("%s: refusal names a label: %v", name, err)
		}
	}
	if err := execRefusal(swarm.Service{Spec: bind("/srv/data")}); err != nil {
		t.Errorf("an ordinary bind was refused: %v", err)
	}
}

func TestMCPResultScrubsAndCaps(t *testing.T) {
	s := newMCPServer(&globalFlags{}, nil, mcpaccess.Policy{}, false, time.Second)

	big := strings.Repeat("x", mcpMaxOutput+100)
	res := s.result("t", time.Now(), big, nil)
	text := res.Content[0].(*mcp.TextContent).Text
	if len(text) > mcpMaxOutput+200 || !strings.Contains(text, "truncated") {
		t.Errorf("output not capped/annotated: %d bytes", len(text))
	}

	res = s.result("t", time.Now(), "a\nlabel swarmexec.mcp.exec=true\nb\n", nil)
	if got := res.Content[0].(*mcp.TextContent).Text; got != "a\nb\n" {
		t.Errorf("result not scrubbed: %q", got)
	}

	res = s.result("t", time.Now(), "", errors.New("filter swarmexec.mcp.enable=true rejected"))
	if got := res.Content[0].(*mcp.TextContent).Text; !res.IsError || strings.Contains(got, "swarmexec.mcp") || got == "" {
		t.Errorf("error not scrubbed: %q", got)
	}

	res = s.result("t", time.Now(), "", errors.New("ssh: Permission denied (publickey)."))
	if got := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(got, "ssh-agent") {
		t.Errorf("ssh auth failure does not say to use ssh-agent: %q", got)
	}
}

func TestMCPCapWriter(t *testing.T) {
	full := make(chan struct{})
	w := &capWriter{max: 10, onFull: func() { close(full) }}
	_, _ = w.Write([]byte("12345"))
	_, _ = w.Write([]byte("67890abc"))
	_, _ = w.Write([]byte("more"))
	if w.String() != "1234567890" || !w.Truncated() {
		t.Errorf("capWriter = %q truncated=%v", w.String(), w.Truncated())
	}
	select {
	case <-full:
	case <-time.After(time.Second):
		t.Error("onFull never ran")
	}
	if s, cut := capText("héllo", 2); !cut || s != "h" {
		t.Errorf("capText split a rune: %q", s)
	}
}

func TestMCPOperatorAndAllow(t *testing.T) {
	for in, want := range map[string]string{"alice": "alice+mcp", "alice+mcp": "alice+mcp", "": "unknown+mcp"} {
		if got := mcpOperator(in); got != want {
			t.Errorf("mcpOperator(%q) = %q, want %q", in, got, want)
		}
	}
	if r, err := parseMCPAllow([]string{"report"}); err != nil || !r {
		t.Errorf("--allow report: %v %v", r, err)
	}
	if _, err := parseMCPAllow([]string{"reprot"}); err == nil {
		t.Error("--allow accepted a typo")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func cloneArgs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
