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

	"swarmexec/client/internal/swarmops"
)

// mutationProbe is a write that blocks until released, reporting the context
// its work received and what apply was handed.
type mutationProbe struct {
	ctx     context.Context
	release chan struct{}
	applied chan error
}

func startMutation(t *testing.T, u *ui, workErr error) *mutationProbe {
	t.Helper()
	p := &mutationProbe{release: make(chan struct{}), applied: make(chan error, 1)}
	started := make(chan context.Context, 1)
	onLoop(t, u, func() {
		mutateDo(u, "test.mutate", "stack removal", func(ctx context.Context) error {
			started <- ctx
			<-p.release
			return workErr
		}, func(err error) { p.applied <- err })
	})
	select {
	case p.ctx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the write did not start")
	}
	return p
}

// On the cluster it ran on, a write finishes exactly like an async one.
func TestMutateAppliesOnTheSameCluster(t *testing.T) {
	u := runningUI(t)
	p := startMutation(t, u, errors.New("boom"))
	close(p.release)
	select {
	case err := <-p.applied:
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("apply got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("apply never ran")
	}
	if pageOpen(t, u, pageInfo) {
		t.Error("a write that ended where it started needs no extra notice")
	}
}

// A switch must neither cancel a write nor swallow its outcome: the operator
// confirmed a removal or a deploy and has to learn whether it happened.
func TestMutateSurvivesASwitchAndReportsSuccess(t *testing.T) {
	u := runningUI(t)
	p := startMutation(t, u, nil)
	leave(t, u) // the switch: cancels the cluster's context, moves the generation
	if err := p.ctx.Err(); err != nil {
		t.Fatalf("a cluster switch cancelled the write: %v", err)
	}
	if _, ok := p.ctx.Deadline(); !ok {
		t.Error("a write is bounded by a timeout")
	}
	close(p.release)
	text := waitPageText(t, u, pageInfo, "stack removal", `cluster "a"`, "finished")
	if !strings.Contains(text, "switched") {
		t.Errorf("the notice must say why it appears:\n%s", text)
	}
	if len(p.applied) != 0 {
		t.Error("apply drives the old cluster's widgets and must not run after a switch")
	}
}

func TestMutateReportsAFailureAfterASwitch(t *testing.T) {
	u := runningUI(t)
	p := startMutation(t, u, errors.New("network in use"))
	leave(t, u)
	close(p.release)
	waitPageText(t, u, pageInfo, "stack removal", `cluster "a"`, "failed", "network in use", "look at")
}

// A write is "running" from the call until its work returns.
func TestMutateCountsAsRunning(t *testing.T) {
	u := runningUI(t)
	p := startMutation(t, u, nil)
	if got := u.muts.running(); len(got) != 1 || got[0] != "stack removal on a" {
		t.Fatalf("running = %v", got)
	}
	close(p.release)
	waitUntil(t, "the write to leave the running set", func() bool { return len(u.muts.running()) == 0 })
}

// q with a write in flight asks first, and Cancel keeps the session.
func TestQuitAsksWhileAWriteRuns(t *testing.T) {
	u := runningUI(t)
	p := startMutation(t, u, nil)
	onLoop(t, u, func() { u.quit() })
	text := waitPageText(t, u, pageConfirm, "1 operation(s) still running", "stack removal on a", "Quit anyway", "Cancel")
	if !strings.Contains(text, "midway") {
		t.Errorf("the dialog must say what quitting does:\n%s", text)
	}
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageConfirm)
	onLoop(t, u, func() {}) // the loop is still running: nothing quit
	close(p.release)
}

// The secret rotation is the other write quitting would cut off.
func TestRotationCountsAsRunning(t *testing.T) {
	u := runningUI(t)
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	u.runCtx = runCtx
	started, release := make(chan struct{}, 1), make(chan struct{})
	orig := rotateSecretFn
	rotateSecretFn = func(context.Context, swarmops.SecretRotateAPI, string, []byte, swarmops.RotateOptions) error {
		started <- struct{}{}
		<-release
		return nil
	}
	t.Cleanup(func() { rotateSecretFn = orig })
	onLoop(t, u, func() { u.secTab().runRotation("db_password", []byte("new")) })
	<-started
	if got := u.muts.running(); len(got) != 1 || !strings.Contains(got[0], "secret rotation of db_password") {
		t.Fatalf("running = %v", got)
	}
	leave(t, u) // it ends on another cluster, so no reload of this one's secrets
	close(release)
	waitUntil(t, "the rotation to leave the running set", func() bool { return len(u.muts.running()) == 0 })
}
