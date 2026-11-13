// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rivo/tview"
)

// leave is the switch as far as the cluster being left is concerned, on the
// UI goroutine, followed by the new cluster becoming the visible one.
func leave(t *testing.T, u *ui) (closed int) {
	t.Helper()
	onLoop(t, u, func() {
		closed = u.leaveCluster()
		u.clusterState = newClusterState("b", "b")
	})
	return closed
}

func pageOpen(t *testing.T, u *ui, name string) bool {
	t.Helper()
	var open bool
	onLoop(t, u, func() { open = u.pages.HasPage(name) })
	return open
}

// The known limit of dropping a stale result: the overlay its apply would have
// closed stayed up over the new cluster with nothing left to take it down. A
// switch now closes the old cluster's overlays itself — and leaves the
// session's own (here the client log viewer) alone.
func TestSwitchClosesTheOldClustersOverlays(t *testing.T) {
	r := captureLog(t)
	u := runningUI(t)
	release := make(chan struct{})
	onLoop(t, u, func() {
		u.overlayFor(pageLogView, nil, "").show(tview.NewBox(), nil)
		prog := u.overlayFor(pageVolProgress, nil, "")
		prog.show(tview.NewBox(), nil)
		async(u, "test.progress", func(context.Context) (int, error) {
			<-release
			return 1, nil
		}, func(int, error) { prog.Close() })
		u.info("a notice about cluster a")
	})
	if n := leave(t, u); n != 2 {
		t.Fatalf("closed %d overlays, want the progress overlay and the notice", n)
	}
	close(release)
	waitForLog(t, r, "stale result dropped", "op=test.progress")
	if pageOpen(t, u, pageVolProgress) || pageOpen(t, u, pageInfo) {
		t.Fatal("an overlay of the old cluster is still up after the switch")
	}
	if !pageOpen(t, u, pageLogView) {
		t.Fatal("the client log viewer belongs to the session and must survive a switch")
	}
	var shown []string
	onLoop(t, u, func() {
		for _, o := range u.shown {
			shown = append(shown, o.name)
		}
	})
	if strings.Join(shown, ",") != pageLogView {
		t.Fatalf("tracked overlays after the switch: %v", shown)
	}
}

// A page shown again under the same name replaces the first on screen, so only
// the newest is tracked — closing the old cluster's overlays closes it once.
func TestTrackedOverlaysFollowThePages(t *testing.T) {
	u := runningUI(t)
	onLoop(t, u, func() {
		u.info("one")
		u.info("two")
		if len(u.shown) != 1 {
			t.Errorf("two notices on one page tracked as %d overlays", len(u.shown))
		}
		u.shown[0].Close()
		if len(u.shown) != 0 {
			t.Errorf("a closed overlay is still tracked")
		}
	})
}

// A secret rotation deletes the old secret before it recreates it, so one
// cancelled halfway leaves the services on the temporary secret. A switch must
// not cancel it, and its outcome is still reported — naming the cluster it ran
// on, since another one is on screen by then.
func TestSecretRotationSurvivesASwitch(t *testing.T) {
	r := captureLog(t)
	u := runningUI(t)
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	u.runCtx = runCtx

	started, release := make(chan context.Context, 1), make(chan struct{})
	orig := rotateSecretFn
	rotateSecretFn = func(ctx context.Context, _ secretRotateAPI, _ string, _ []byte, _ rotateOptions) error {
		started <- ctx
		<-release
		return ctx.Err()
	}
	t.Cleanup(func() { rotateSecretFn = orig })

	onLoop(t, u, func() { u.runRotation("db_password", []byte("new")) })
	var rctx context.Context
	select {
	case rctx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the rotation did not start")
	}
	leave(t, u)
	if err := rctx.Err(); err != nil {
		t.Fatalf("a cluster switch cancelled the rotation: %v", err)
	}
	close(release)
	waitForLog(t, r, "op=ui.secrets.rotate", "cluster=a")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var footer string
		onLoop(t, u, func() { footer = u.help.GetText(false) })
		if strings.Contains(footer, "rotated[white] secret db_password on cluster a") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no report naming the rotation's cluster; footer %q\n%s", footer, strings.Join(r.Lines(), "\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pageOpen(t, u, pageSecRotateRun) {
		t.Fatal("the rotation's progress overlay must not stay over the other cluster")
	}
}
