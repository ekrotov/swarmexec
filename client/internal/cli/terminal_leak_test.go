// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/agent/agenttest"
	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/config"
	"swarmexec/client/internal/resolve"
)

// waitGoroutines polls until the goroutine count is at most limit. The scheduler
// and gRPC's connection teardown finish asynchronously, so a bare count taken
// right after the session ended would be flaky; a bounded wait is not.
func waitGoroutines(t *testing.T, limit int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n := runtime.NumGoroutine()
		if n <= limit || time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Every terminal the TUI opened used to leave goroutines behind: the resize
// pump waited on a channel that is never closed, and with it kept the whole
// pane (emulator screen, input queue) reachable. Opening and closing terminals
// must return the process to where it started.
func TestTerminalSessionsDoNotLeakGoroutines(t *testing.T) {
	d := agenttest.NewDocker().AddContainer(agenttest.Container{ID: "c1", Service: "web"})
	d.SetExec(func(_ []string, _ io.Reader, stdout, _ io.Writer) int {
		_, _ = stdout.Write([]byte("hi"))
		return 0
	})
	a := agenttest.Start(t, agenttest.Options{Secret: "s3cret", Docker: d})
	ac := agentcall.New(config.Config{Port: 7000, Insecure: true, AgentSecret: "s3cret", ProxyDialer: a.Dial})
	t.Cleanup(ac.Close)
	ep := resolve.Endpoint{DialHost: "node-a", ContainerID: "c1"}

	// The exec output asks the app for a redraw, so it has to be running.
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	app := tview.NewApplication().SetScreen(scr)
	appDone := make(chan struct{})
	go func() { _ = app.Run(); close(appDone) }()
	t.Cleanup(func() { app.Stop(); <-appDone })

	open := func() {
		tv := newTerminalView(app)
		done := make(chan error, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tv.run(ctx, ac, terminalSession{ep: ep, command: []string{"sh"}, tty: true}, 5*time.Second, func(_ int, err error) { done <- err })
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("session: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("session did not end")
		}
	}

	// One session first: it starts whatever the process keeps for good (the
	// agent's handlers, gRPC's shared goroutines), which is not a leak.
	open()
	base := waitGoroutines(t, 0, 500*time.Millisecond) // let it settle; the value is what we want
	const sessions = 10
	for range sessions {
		open()
	}
	// One goroutine of slack for the runtime; ten leaked sessions would be ten.
	if n := waitGoroutines(t, base+1, 5*time.Second); n > base+1 {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Fatalf("%d goroutines after %d terminals, %d before: they leak\n%s", n, sessions, base, buf)
	}
}
