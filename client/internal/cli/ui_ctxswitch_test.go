// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/resolve"
)

// A sidebar switch is the TUI's own business: docker's stored current context
// (what every other terminal's `docker` talks to) stays where it was, unless
// ui.sync_docker_context opts back in. contexts.<name>.confirm_switch puts the
// switch to that context behind a confirm. All against a DOCKER_CONFIG of the
// test's own (chromeUI sets it), never the operator's.

// switchUI is a running UI on cluster "a" with a second, connected cluster
// "ops" and a stored context of that name, the sidebar listing both. cfg is
// the config both clusters resolved (the same file, as in a real session).
func switchUI(t *testing.T, cfg config.Config) *ui {
	t.Helper()
	u := chromeUI(t)
	if err := dockerctx.Create(dockerctx.CreateOptions{Name: "ops", Host: "tcp://ops.example:2376"}); err != nil {
		t.Fatal(err)
	}
	dcli := fakeSwarmClient(t, opsSwarm())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	onLoop(t, u, func() {
		u.runCtx = ctx
		u.cfg = cfg
		ops := newClusterState("ops", "ops")
		ops.cfg = cfg
		ops.dcli, ops.r = dcli, resolve.New(dcli, resolve.AddrHostname)
		ops.ctx, ops.cancel = context.WithCancel(ctx)
		u.clusters["ops"] = ops
		u.loadContexts()
	})
	return u
}

// sidebarSwitch selects ctx in the sidebar and presses Enter there.
func sidebarSwitch(t *testing.T, u *ui, name string) {
	t.Helper()
	selectContext(t, u, name)
	onLoop(t, u, func() {
		u.cxtable.InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(p tview.Primitive) { u.app.SetFocus(p) })
	})
}

func activeName(t *testing.T, u *ui) string {
	t.Helper()
	var name string
	onLoop(t, u, func() { name = u.activeCtx })
	return name
}

func TestSidebarSwitchLeavesDockerContextAlone(t *testing.T) {
	u := switchUI(t, config.Default())
	sidebarSwitch(t, u, "ops")
	waitFooter(t, u, "TUI only")
	if got := activeName(t, u); got != "ops" {
		t.Fatalf("active %q after the switch, want ops", got)
	}
	if got := dockerctx.Current(); got != "default" {
		t.Errorf("docker's current context = %q after a TUI switch, want it untouched (default)", got)
	}
}

func TestSidebarSwitchSyncsDockerContextWhenConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.UI.SyncDockerContext = true
	u := switchUI(t, cfg)
	sidebarSwitch(t, u, "ops")
	waitFooter(t, u, "docker context is now ops (shell too)")
	if got := dockerctx.Current(); got != "ops" {
		t.Errorf("docker's current context = %q with ui.sync_docker_context, want ops", got)
	}
}

func TestSidebarSwitchConfirmsWhenConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Contexts = map[string]config.ContextConfig{"ops": {ConfirmSwitch: true}}
	u := switchUI(t, cfg)

	// Cancel: nothing happens, the operator stays where they were.
	sidebarSwitch(t, u, "ops")
	waitPageText(t, u, pageConfirm, `Switch to context "ops"?`, "tcp://ops.example:2376", "contexts.ops.confirm_switch")
	sendSpecial(t, u, tcell.KeyTab) // Switch -> Cancel
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageGone(t, u, pageConfirm)
	if got := activeName(t, u); got != "a" {
		t.Fatalf("active %q after cancelling, want a", got)
	}

	// Confirm: the switch goes through.
	sidebarSwitch(t, u, "ops")
	waitPageText(t, u, pageConfirm, `Switch to context "ops"?`)
	sendSpecial(t, u, tcell.KeyEnter)
	waitFooter(t, u, "switched to ops")
	if got := activeName(t, u); got != "ops" {
		t.Fatalf("active %q after confirming, want ops", got)
	}
}
