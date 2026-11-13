// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/clientlog"
)

// runningUI is overlayUI with its event loop actually running, on a simulated
// screen, so QueueUpdateDraw is served the way it is in the real TUI. The
// cluster is a live one with its own context. Stopped at test end.
func runningUI(t *testing.T) *ui {
	t.Helper()
	u := overlayUI()
	u.clusterState = newClusterState("a", "a")
	ctx, cancel := context.WithCancel(context.Background())
	u.clusterState.ctx, u.clusterState.cancel = ctx, cancel
	screen := tcell.NewSimulationScreen("UTF-8")
	u.app.SetScreen(screen)
	u.app.SetRoot(u.pages, true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = u.app.Run()
	}()
	t.Cleanup(func() {
		u.app.Stop()
		<-done
		cancel()
	})
	return u
}

// onLoop runs fn on the UI goroutine and waits for it — the test's way to
// look at widget state without racing the loop.
func onLoop(t *testing.T, u *ui, fn func()) {
	t.Helper()
	done := make(chan struct{})
	u.app.QueueUpdate(func() {
		fn()
		close(done)
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the UI loop did not run the update")
	}
}

// switchAway is the part of activateCluster that matters to a load in
// flight: the outgoing cluster's context is cancelled and the generation
// moves, both on the UI goroutine.
func switchAway(t *testing.T, u *ui, cancel bool) {
	t.Helper()
	onLoop(t, u, func() {
		if cancel {
			u.cancel()
		}
		u.gen.Add(1)
		u.clusterState = newClusterState("b", "b")
	})
}

// captureLog routes the client log into a fresh ring at debug level, so a
// test can wait for a record the code under test writes off its own goroutine.
func captureLog(t *testing.T) *clientlog.Ring {
	t.Helper()
	r, err := clientlog.Init("debug", "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitForLog(t *testing.T, r *clientlog.Ring, parts ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range r.Lines() {
			all := true
			for _, p := range parts {
				all = all && strings.Contains(l, p)
			}
			if all {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no log line with %q in:\n%s", parts, strings.Join(r.Lines(), "\n"))
}

func TestAsyncDeliversOnTheUILoop(t *testing.T) {
	u := runningUI(t)
	got := make(chan string, 1)
	onLoop(t, u, func() {
		async(u, "test.ok", func(ctx context.Context) (string, error) {
			if ctx != u.clusterState.ctx {
				return "", errors.New("work must get the cluster's context")
			}
			return "value", nil
		}, func(v string, err error) {
			if err != nil {
				got <- "error: " + err.Error()
				return
			}
			got <- v
		})
	})
	select {
	case v := <-got:
		if v != "value" {
			t.Fatalf("apply got %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply never ran")
	}
}

// The staleness guard, and the cancellation that comes with it: a switch
// cancels the work's context, and whatever the work returns afterwards is
// not shown on the cluster the operator moved to.
func TestAsyncDropsAResultAfterASwitch(t *testing.T) {
	r := captureLog(t)
	u := runningUI(t)
	started, cancelled := make(chan struct{}), make(chan struct{})
	applied := make(chan struct{}, 1)
	onLoop(t, u, func() {
		async(u, "test.stale", func(ctx context.Context) (int, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return 0, ctx.Err()
		}, func(int, error) { applied <- struct{}{} })
	})
	<-started
	switchAway(t, u, true)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("a switch must cancel the work's context")
	}
	waitForLog(t, r, "stale result dropped", "op=test.stale")
	// A cancellation is not an error worth a red line in the log viewer.
	waitForLog(t, r, "op cancelled", "op=test.stale")
	for _, l := range r.Lines() {
		if strings.Contains(l, "op failed") && strings.Contains(l, "test.stale") {
			t.Errorf("a cancelled op was logged as a failure: %s", l)
		}
	}
	onLoop(t, u, func() {}) // anything queued before this has run
	select {
	case <-applied:
		t.Fatal("a result for the old cluster was applied")
	default:
	}
}

// The generation alone decides: a result that does arrive — work that did not
// honour its context, or finished just before the switch — is still dropped.
func TestAsyncDropsACompletedResultAfterASwitch(t *testing.T) {
	r := captureLog(t)
	u := runningUI(t)
	release := make(chan struct{})
	applied := make(chan struct{}, 1)
	onLoop(t, u, func() {
		async(u, "test.late", func(context.Context) (int, error) {
			<-release
			return 42, nil
		}, func(int, error) { applied <- struct{}{} })
	})
	switchAway(t, u, false)
	close(release)
	waitForLog(t, r, "stale result dropped", "op=test.late")
	onLoop(t, u, func() {})
	if len(applied) != 0 {
		t.Fatal("a result for the old cluster was applied")
	}
}

func TestAsyncExplainsAgentErrors(t *testing.T) {
	u := runningUI(t)
	got := make(chan error, 1)
	onLoop(t, u, func() {
		asyncDo(u, "test.err", func(context.Context) error {
			return status.Error(codes.Unimplemented, "unknown method Stats")
		}, func(err error) { got <- err })
	})
	var err error
	select {
	case err = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("apply never ran")
	}
	if err == nil || err.Error() != errAgentTooOld.Error() {
		t.Fatalf("apply got %v, want the too-old explanation", err)
	}
	if status.Code(err) != codes.Unimplemented || !errors.Is(err, errAgentTooOld) {
		t.Fatalf("the explanation must keep the cause: code %v", status.Code(err))
	}
}

type fakeLister struct {
	agent bool
	err   error
}

func (f fakeLister) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	if f.agent {
		return client.ServiceListResult{Items: []swarm.Service{{}}}, f.err
	}
	return client.ServiceListResult{}, f.err
}

func TestExplainErr(t *testing.T) {
	ctx := context.Background()
	manager := errors.New("Error response from daemon: service web not found")
	unavailable := status.Error(codes.Unavailable, "connection refused")
	for _, tc := range []struct {
		name   string
		lister serviceLister
		err    error
		want   string
	}{
		{"nil", nil, nil, ""},
		{"manager error passes through", fakeLister{}, manager, manager.Error()},
		{"too old", nil, status.Error(codes.Unimplemented, "x"), errAgentTooOld.Error()},
		{"secret", nil, status.Error(codes.Unauthenticated, "invalid secret"), errSecretRejected.Error()},
		{"unavailable, no agent", fakeLister{}, unavailable, errNoAgent.Error()},
		{"unavailable, agent deployed", fakeLister{agent: true}, unavailable, unavailable.Error()},
		{"unavailable, manager unreachable", fakeLister{err: errors.New("down")}, unavailable, unavailable.Error()},
		{"unavailable, no manager client", nil, unavailable, unavailable.Error()},
		{"already explained", nil, errAgentTooOld, errAgentTooOld.Error()},
		{"other agent status", fakeLister{}, status.Error(codes.NotFound, "no such container"), "rpc error: code = NotFound desc = no such container"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := explainErr(ctx, tc.lister, tc.err)
			if (got == nil) != (tc.err == nil) {
				t.Fatalf("got %v for %v", got, tc.err)
			}
			if got != nil && got.Error() != tc.want {
				t.Fatalf("got %q, want %q", got.Error(), tc.want)
			}
			if tc.err != nil && !errors.Is(got, tc.err) {
				t.Fatalf("the explained error must wrap the original")
			}
		})
	}
}
