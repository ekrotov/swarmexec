// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"swarmexec/client/internal/config"
	"swarmexec/client/internal/dockerctx"
)

// setStoredCurrent marks a context current in the test DOCKER_CONFIG, the way
// `docker context use` would.
func setStoredCurrent(t *testing.T, name string) {
	t.Helper()
	cfg := `{"currentContext":"` + name + `"}`
	if err := os.WriteFile(filepath.Join(os.Getenv("DOCKER_CONFIG"), "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The bug: with only $DOCKER_HOST set, `swarmexec ui` dialled DOCKER_HOST's
// daemon but the footer said "ctx <stored current>" — production here — and
// the sidebar marked that context active. The name the ui shows, keys its
// cluster state and forwards under, and marks in the sidebar must be the
// context the connection actually resolves to.
func TestUIContextNameHonoursDockerHost(t *testing.T) {
	writeContexts(t)
	setStoredCurrent(t, "alpha")
	const local = "unix:///tmp/test-docker.sock"
	t.Setenv("DOCKER_HOST", local)

	name := effectiveContextName("")
	if name != "default" {
		t.Fatalf("ui context with DOCKER_HOST = %q, want default (not the stored alpha)", name)
	}
	// The name and the startup connection agree: the first cluster is dialled
	// from ctxOverride "" and remembered under name.
	if got := resolveEndpoint("").Host; got != local {
		t.Errorf("startup dials %q, want DOCKER_HOST %q", got, local)
	}
	if got := resolveEndpoint(name).Host; got != local {
		t.Errorf("context %q dials %q, want DOCKER_HOST %q", name, got, local)
	}

	label := ctxStatusLabel(name)
	if !strings.Contains(label, "$DOCKER_HOST") || strings.Contains(label, "alpha") {
		t.Errorf("footer label = %q, want default ($DOCKER_HOST) and no alpha", label)
	}

	// Sidebar: ▶ on default, not on the stored current alpha.
	u := sidebarUI(name, "alpha", "beta", "default")
	u.renderContexts()
	want := map[int]string{0: "  alpha", 1: "  beta", 2: "▶ default"}
	for row, text := range want {
		if got := u.cxtable.GetCell(row, 0).Text; got != text {
			t.Errorf("sidebar row %d = %q, want %q", row, got, text)
		}
	}

	// The same answer reaches mcp and `config show`.
	if got := servedContextName(""); got != "default" {
		t.Errorf("mcp served context = %q, want default", got)
	}
	if v := buildConfigView(&globalFlags{}, config.Config{}); v.DockerContext != "default" || v.ManagerHost != local {
		t.Errorf("config show = %q @ %q, want default @ %q", v.DockerContext, v.ManagerHost, local)
	}
}

// An explicit --context still wins over DOCKER_HOST, as with docker.
func TestUIContextNameExplicitContextBeatsDockerHost(t *testing.T) {
	writeContexts(t)
	setStoredCurrent(t, "alpha")
	t.Setenv("DOCKER_HOST", "unix:///tmp/test-docker.sock")

	if got := effectiveContextName("beta"); got != "beta" {
		t.Fatalf("--context beta named %q", got)
	}
	if got := resolveEndpoint("beta").Host; got != "ssh://bastion-beta" {
		t.Errorf("--context beta dials %q", got)
	}
	if got := ctxStatusLabel("beta"); got != "beta" {
		t.Errorf("footer label = %q, want plain beta", got)
	}
}

// $DOCKER_CONTEXT is an explicit choice too, and outranks DOCKER_HOST.
func TestUIContextNameDockerContextBeatsDockerHost(t *testing.T) {
	writeContexts(t)
	setStoredCurrent(t, "alpha")
	t.Setenv("DOCKER_HOST", "unix:///tmp/test-docker.sock")
	t.Setenv("DOCKER_CONTEXT", "beta")

	if got := effectiveContextName(""); got != "beta" {
		t.Fatalf("DOCKER_CONTEXT=beta named %q", got)
	}
	if got := resolveEndpoint("").Host; got != "ssh://bastion-beta" {
		t.Errorf("DOCKER_CONTEXT=beta dials %q", got)
	}
}

// Without DOCKER_HOST nothing changes: the stored current context names the
// session, and "default" carries no annotation.
func TestUIContextNameWithoutDockerHost(t *testing.T) {
	writeContexts(t)
	setStoredCurrent(t, "alpha")

	if got := effectiveContextName(""); got != "alpha" {
		t.Fatalf("ui context = %q, want stored alpha", got)
	}
	if got := ctxStatusLabel("default"); got != "default" {
		t.Errorf("default label without DOCKER_HOST = %q", got)
	}
}

// A sidebar switch while DOCKER_HOST is set. The switched-to cluster is
// dialled BY NAME (switchCluster builds it with ctxOverride == name), so
// DOCKER_HOST no longer applies to it; "default" keeps meaning DOCKER_HOST.
// dockerctx.Use still records the choice as docker's current context — here in
// a temp DOCKER_CONFIG, never the operator's — and the flash says a shell that
// exports DOCKER_HOST will not follow.
func TestSidebarSwitchWithDockerHostSet(t *testing.T) {
	writeContexts(t)
	setStoredCurrent(t, "alpha")
	const local = "unix:///tmp/test-docker.sock"
	t.Setenv("DOCKER_HOST", local)

	u := &ui{clusters: map[string]*clusterState{}}
	first := u.cluster(effectiveContextName(""), "")
	if first.name != "default" {
		t.Fatalf("first cluster keyed %q, want default", first.name)
	}
	// What switchCluster("beta") builds and what activateCluster records.
	target := u.cluster("beta", "beta")
	if got := resolveEndpoint(target.ctxOverride).Host; got != "ssh://bastion-beta" {
		t.Errorf("switch to beta dials %q, want its own endpoint despite DOCKER_HOST", got)
	}
	if err := dockerctx.Use(target.name); err != nil {
		t.Fatal(err)
	}
	if got := dockerctx.Current(); got != "beta" {
		t.Errorf("stored current after switch = %q, want beta", got)
	}
	// Switching back to "default" reuses the first cluster: one connection,
	// still DOCKER_HOST's daemon.
	if back := u.cluster("default", "default"); back != first {
		t.Error("switching back to default built a second cluster instead of reusing the first")
	}
	if got := resolveEndpoint("default").Host; got != local {
		t.Errorf("default dials %q after the switch, want DOCKER_HOST %q", got, local)
	}

	msg := switchedFlash("beta", true, nil, true)
	if !strings.Contains(msg, "$DOCKER_HOST") {
		t.Errorf("flash = %q, want the DOCKER_HOST caveat", msg)
	}
	if msg := switchedFlash("default", true, nil, true); strings.Contains(msg, "$DOCKER_HOST") {
		t.Errorf("flash for default = %q: default IS DOCKER_HOST, no caveat", msg)
	}
	if msg := switchedFlash("beta", true, nil, false); strings.Contains(msg, "$DOCKER_HOST") {
		t.Errorf("flash without DOCKER_HOST = %q", msg)
	}
	// A failed `docker context use` used to be flashed and then immediately
	// overwritten by "switched to"; it is now part of that one message.
	if msg := switchedFlash("beta", true, errors.New("disk full"), true); !strings.Contains(msg, "disk full") {
		t.Errorf("flash = %q, want the Use error", msg)
	}
}
