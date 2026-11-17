// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
)

// reported is what reportError prints for err.
func reported(err error) string {
	var b strings.Builder
	reportError(&b, err, &globalFlags{})
	return b.String()
}

// waitSwarm has shop_web (1/1 running, settled), shop_db (0/1, its task
// failed) and shop_api (its update paused), all in stack shop, on one node.
func waitSwarm() *swarmfake.Swarm {
	web := stackedService("s-web", "shop_web", "shop", 1)
	db := stackedService("s-db", "shop_db", "shop", 1)
	db.ServiceStatus.RunningTasks = 0
	api := stackedService("s-api", "shop_api", "shop", 1)
	api.UpdateStatus = &swarm.UpdateStatus{State: swarm.UpdateStatePaused, Message: "task failed"}
	failed := swarm.Task{ID: "t-db", ServiceID: "s-db", DesiredState: swarm.TaskStateRunning}
	failed.Status.State = swarm.TaskStateFailed
	failed.Status.Err = "no suitable node"
	return &swarmfake.Swarm{
		Services: []swarm.Service{web, db, api},
		Tasks:    []swarm.Task{runningTask("s-web", "c-web", "n1", 1), failed},
		Nodes:    []swarm.Node{readyNode("n1", agentNodeHost)},
	}
}

// `wait` end to end: each way it ends, with what it prints on the way.
func TestWaitRun(t *testing.T) {
	g := cliCluster(t, waitSwarm(), &fakeAgent{})
	run := func(args ...string) (string, error) {
		t.Helper()
		_, errOut, err := runCmd(t, newWaitCmd(g), args...)
		return errOut, err
	}
	exit := func(err error) int {
		var ce *cliError
		if !errors.As(err, &ce) {
			return -1
		}
		return ce.code
	}

	// A failure is the command's error, printed with the swarmexec: prefix
	// like any other, not a progress line of its own.
	out, err := run("nope")
	if exit(err) != 1 || out != "" || reported(err) != "swarmexec: no such service \"nope\"\n" {
		t.Errorf("missing: %q, %v", out, err)
	}

	out, err = run("shop_api")
	if exit(err) != 1 || out != "" || reported(err) != "swarmexec: shop_api: its update paused (task failed) — it runs a mix of old and new tasks; `docker service update shop_api` resumes it\n" {
		t.Errorf("paused: %q, %v", out, err)
	}

	// shop_web converged but has not held it for the settle time: the wait is
	// still waiting for it too, and says so.
	out, err = run("--timeout", "1ms", "shop_db", "shop_web")
	if exit(err) != 1 || reported(err) != "swarmexec: timed out after 1ms waiting for 2 service(s)\n" ||
		out != "  shop_db: 0/1 running — no suitable node\n  shop_web: 1/1 running\n"+
			"✗ shop_db: 0/1 running — no suitable node\n✗ shop_web: converged, settling (1/1 running)\n" {
		t.Errorf("timeout: %q, %v", out, err)
	}

	// --healthy asks the agent, which cannot report health: never healthy.
	out, err = run("--healthy", "--timeout", "1ms", "--connect-timeout", "2s", "shop_web")
	if exit(err) != 1 || out != "  shop_web: health unknown on 127.0.0.1 (agent too old or unreachable)\n✗ shop_web: health unknown on 127.0.0.1 (agent too old or unreachable)\n" {
		t.Errorf("healthy: %q, %v", out, err)
	}

	if _, err = run("--stack", "nostack"); exit(err) != usageExitCode || err.Error() != `no services in stack "nostack"` {
		t.Errorf("empty stack: %v", err)
	}

	// The wait ends with the command's context.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := newWaitCmd(g)
	cmd.SetContext(ctx)
	if _, _, err := runCmd(t, cmd, "shop_db"); exit(err) != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("cancelled: %v", err)
	}

	// A broken client config fails --healthy before anything is asked.
	bad := *g
	bad.configPath = filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad.configPath, []byte("port: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmd(t, newWaitCmd(&bad), "--healthy", "shop_web"); exit(err) != usageExitCode {
		t.Errorf("bad config: %v", err)
	}
}

// A service that holds its verdict through the settle time converges; the
// stack names its services.
func TestWaitRunConverges(t *testing.T) {
	g := cliCluster(t, waitSwarm(), nil)
	_, out, err := runCmd(t, newWaitCmd(g), "--stack", "shop", "shop_web", "--timeout", "1m")
	// shop_api is in the stack, and paused: the stack fails on it.
	if err == nil || out != "" || err.Error() != "shop_api: its update paused (task failed) — it runs a mix of old and new tasks; `docker service update shop_api` resumes it" {
		t.Errorf("stack: %q, %v", out, err)
	}

	_, out, err = runCmd(t, newWaitCmd(g), "shop_web")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^  shop_web: 1/1 running\n  shop_web: converged \(1/1 running\)\n✓ 1 service\(s\) converged in \d+s\n$`).MatchString(out) {
		t.Errorf("converged: %q", out)
	}
}

// On timeout a service that has held converged through the settle time is
// done and left out; one still settling is listed and counted.
func TestWaitTimedOutCountsSettling(t *testing.T) {
	now := time.Now()
	var out strings.Builder
	w := &waitProgress{out: &out, since: now.Add(-time.Minute),
		settledSince: map[string]time.Time{"done": now.Add(-waitSettle), "fresh": now.Add(-time.Second)}}
	snaps := []waitSnapshot{
		{name: "done", have: 1, want: 1},
		{name: "fresh", have: 2, want: 2},
		{name: "short", have: 0, want: 1},
	}
	err := w.timedOut(snaps, now, time.Minute)
	if err == nil || err.Error() != "timed out after 1m0s waiting for 2 service(s)" ||
		out.String() != "✗ fresh: converged, settling (2/2 running)\n✗ short: 0/1 running\n" {
		t.Errorf("%q, %v", out.String(), err)
	}
}
