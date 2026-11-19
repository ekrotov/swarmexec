// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logstream

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"swarmexec/agent/agenttest"
	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmfake"
)

// testRun starts a real agent (agenttest: the production handlers, interceptor
// chain and TLS over a fake Docker daemon) and an empty fake manager, and
// returns a Run wired to both, writing into out/errOut. Containers c1 and c2
// each log two stdout lines and one stderr line, then end — a container that
// exited.
func testRun(t *testing.T, p Params, out, errOut io.Writer) (Run, *agenttest.Agent) {
	return testRunSwarm(t, p, &swarmfake.Swarm{}, out, errOut)
}

// testRunSwarm is testRun against the given fake manager state.
func testRunSwarm(t *testing.T, p Params, sw *swarmfake.Swarm, out, errOut io.Writer) (Run, *agenttest.Agent) {
	t.Helper()
	d := agenttest.NewDocker()
	for _, id := range []string{"c1", "c2"} {
		d.AddContainer(agenttest.Container{ID: id, Service: "web"})
		d.SetLogs(id, "hello from "+id+"\nERR oops in "+id+"\nbye from "+id+"\n")
	}
	return testRunWith(t, p, sw, d, out, errOut)
}

// testRunWith is testRunSwarm with the node's fake daemon supplied by the
// caller, for a test that needs its own log streams.
func testRunWith(t *testing.T, p Params, sw *swarmfake.Swarm, d *agenttest.Docker, out, errOut io.Writer) (Run, *agenttest.Agent) {
	t.Helper()
	agent := agenttest.Start(t, agenttest.Options{Secret: "s3cret", Docker: d})

	api := swarmfake.Serve(t, sw)
	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dcli.Close() })

	ac := agentcall.New(config.Config{
		Port:        7000,
		Insecure:    true,
		AgentSecret: "s3cret",
		ProxyDialer: agent.Dial,
	})
	t.Cleanup(ac.Close)
	if p.ConnectTimeout == 0 {
		p.ConnectTimeout = 3 * time.Second
	}
	return Run{Agents: ac, Resolver: resolve.New(dcli, resolve.AddrHostname), Params: p, Stdout: out, Stderr: errOut}, agent
}

func ep(container string) resolve.Endpoint {
	return resolve.Endpoint{DialHost: "node-a", ContainerID: container, NodeName: "node-a"}
}

// One container's stream lands on the two writers it came from, and the
// request carries the run's parameters.
func TestStreamSplitsStdoutAndStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	run, agent := testRun(t, Params{Tail: 50, Timestamps: true, Since: 2 * time.Minute}, &out, &errOut)

	if err := Stream(context.Background(), run, ep("c1")); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "hello from c1\nbye from c1\n" {
		t.Errorf("stdout = %q", got)
	}
	if got := errOut.String(); got != "oops in c1\n" {
		t.Errorf("stderr = %q", got)
	}
	// What the agent asked the daemon for, after translating the request.
	calls := agent.Docker.LogCalls()
	if len(calls) != 1 {
		t.Fatalf("got %d ContainerLogs calls, want 1", len(calls))
	}
	if o := calls[0]; o.Tail != "50" || !o.Timestamps || o.Since == "" || o.Follow {
		t.Errorf("daemon options = %+v, want tail 50, timestamps, a since time, no follow", o)
	}
	if recs := agent.AuditEvents("logs_start"); len(recs) != 1 || recs[0]["container_id"] != "c1" || recs[0]["service"] != "web" {
		t.Errorf("audit logs_start = %v, want one record for c1 of service web", recs)
	}
}

// Following a target whose service is gone ends cleanly, and says so, once
// the container's stream ends.
func TestFollowStopsWhenTheTargetIsGone(t *testing.T) {
	var out bytes.Buffer
	run, agent := testRun(t, Params{Follow: true, Tail: 10}, &out, io.Discard)

	var notes []string
	err := Follow(context.Background(), run, resolve.FollowTarget{Service: "web", Slot: 1}, ep("c1"),
		func(msg string) { notes = append(notes, msg) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hello from c1") {
		t.Errorf("stdout = %q, want the container's lines", out.String())
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "no running container left") {
		t.Errorf("notes = %q, want one saying the target is gone", notes)
	}
	if n := len(agent.Docker.LogCalls()); n != 1 {
		t.Errorf("got %d ContainerLogs calls, want 1 (nothing to reconnect to)", n)
	}
}

// Several sources meet in the shared sinks one whole line at a time, each
// line carrying its source's prefix, through the filter chain wrap builds.
func TestStreamManyPrefixesEverySource(t *testing.T) {
	var out, errOut bytes.Buffer
	run, _ := testRun(t, Params{Tail: 10}, &out, &errOut)
	sources := []Source{
		{Label: "web.1", Endpoint: ep("c1")},
		{Label: "web.2", Endpoint: ep("c2")},
	}
	var wrapped int
	var mu sync.Mutex
	wrap := func(dst io.Writer) (io.Writer, func()) {
		mu.Lock()
		wrapped++
		mu.Unlock()
		return dst, func() {}
	}

	if err := StreamMany(context.Background(), run, sources, false, wrap); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"web.1", "hello from c1", "web.2", "bye from c2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if !strings.Contains(line, "web.1") && !strings.Contains(line, "web.2") {
			t.Errorf("line without a source prefix: %q", line)
		}
	}
	if !strings.Contains(errOut.String(), "oops in c2") {
		t.Errorf("stderr = %q, want the second source's stderr", errOut.String())
	}
	if wrapped != 4 {
		t.Errorf("wrap called %d times, want once per source and stream (4)", wrapped)
	}
}

// A source whose agent cannot be reached is reported, and the error counts it.
func TestStreamManyReportsAFailedSource(t *testing.T) {
	var out, errOut bytes.Buffer
	run, _ := testRun(t, Params{ConnectTimeout: 200 * time.Millisecond}, &out, &errOut)
	run.Agents = agentcall.New(config.Config{
		Port:     7000,
		Insecure: true,
		ProxyDialer: func(context.Context, string) (net.Conn, error) {
			return nil, io.ErrUnexpectedEOF
		},
	})
	t.Cleanup(run.Agents.Close)

	err := StreamMany(context.Background(), run, []Source{{Label: "web.1", Endpoint: ep("c1")}}, false,
		func(dst io.Writer) (io.Writer, func()) { return dst, func() {} })
	if err == nil || !strings.Contains(err.Error(), "1 of 1 log streams failed") {
		t.Fatalf("err = %v, want the failed source counted", err)
	}
	if !strings.Contains(errOut.String(), "stream ended") {
		t.Errorf("stderr = %q, want the failure in the source's own lines", errOut.String())
	}
}
