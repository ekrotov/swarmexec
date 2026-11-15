// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// listManager is a fakeManager that also answers the list calls of the
// Networks, Secrets and Configs tabs, and counts them.
type listManager struct {
	fakeManager

	lmu      sync.Mutex
	networks []network.Summary
	secrets  []swarm.Secret
	configs  []swarm.Config
	cfgErr   error
	calls    map[string]int
	removed  []string
	tasks    []swarm.Task                  // TaskList (the networks' CONTAINERS count)
	swarmInf swarm.Swarm                   // SwarmInspect (default address pool)
	created  []client.NetworkCreateOptions // NetworkCreate calls, in order
	netNames []string                      // their names
}

func (m *listManager) count(op string) {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[op]++
}

func (m *listManager) callsOf(op string) int {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	return m.calls[op]
}

func (m *listManager) NetworkList(context.Context, client.NetworkListOptions) (client.NetworkListResult, error) {
	m.count("networks")
	m.lmu.Lock()
	defer m.lmu.Unlock()
	return client.NetworkListResult{Items: m.networks}, nil
}

func (m *listManager) SecretList(_ context.Context, o client.SecretListOptions) (client.SecretListResult, error) {
	m.count("secrets")
	m.lmu.Lock()
	defer m.lmu.Unlock()
	return client.SecretListResult{Items: m.secrets}, nil
}

func (m *listManager) SecretRemove(_ context.Context, id string, _ client.SecretRemoveOptions) (client.SecretRemoveResult, error) {
	m.lmu.Lock()
	defer m.lmu.Unlock()
	m.removed = append(m.removed, id)
	kept := m.secrets[:0]
	for _, s := range m.secrets {
		if s.ID != id {
			kept = append(kept, s)
		}
	}
	m.secrets = kept
	return client.SecretRemoveResult{}, nil
}

func (m *listManager) ConfigList(context.Context, client.ConfigListOptions) (client.ConfigListResult, error) {
	m.count("configs")
	m.lmu.Lock()
	defer m.lmu.Unlock()
	return client.ConfigListResult{Items: m.configs}, m.cfgErr
}

func netSummary(name string) network.Summary {
	var n network.Summary
	n.ID, n.Name, n.Driver, n.Scope = "id-"+name, name, "overlay", "swarm"
	return n
}

func secretObj(name string) swarm.Secret {
	s := swarm.Secret{ID: "id-" + name}
	s.Spec.Name = name
	return s
}

func configObj(name string) swarm.Config {
	c := swarm.Config{ID: "id-" + name}
	c.Spec.Name = name
	return c
}

// listUI is a running UI on cluster "a", talking to mgr.
func listUI(t *testing.T, mgr *listManager) *ui {
	t.Helper()
	u := runningUI(t)
	onLoop(t, u, func() {
		u.km = defaultKeybinds()
		u.dcli = mgr
		u.clusters = map[string]*clusterState{"a": u.clusterState}
	})
	return u
}

// toCluster makes c the visible cluster the way activateCluster does for a
// load in flight: the generation moves, so a result for the old one is dropped.
func toCluster(t *testing.T, u *ui, c *clusterState) {
	t.Helper()
	onLoop(t, u, func() {
		u.gen.Add(1)
		u.clusterState = c
	})
}

// Networks: entering the tab loads the list through the manager, and each
// cluster keeps its own cursor across a switch away and back.
func TestNetworksTabLoadsAndKeepsTheCursorPerCluster(t *testing.T) {
	ma := &listManager{networks: []network.Summary{netSummary("backend"), netSummary("edge"), netSummary("ingress")}}
	mb := &listManager{networks: []network.Summary{netSummary("other"), netSummary("public")}}
	u := listUI(t, ma)
	a := u.clusterState
	nt := func() *networksTab { return u.netTab() }

	onLoop(t, u, func() { nt().enter() })
	waitForTable(t, u, nt().table, "backend", "edge", "ingress")
	onLoop(t, u, func() { nt().table.Select(2, 0) }) // edge

	b := newClusterState("b", "b")
	b.ctx, b.cancel = context.WithCancel(context.Background())
	defer b.cancel()
	b.dcli = mb
	toCluster(t, u, b)
	onLoop(t, u, func() { nt().refresh() })
	waitForTable(t, u, nt().table, "other", "public")
	onLoop(t, u, func() { nt().table.Select(2, 0) }) // public

	toCluster(t, u, a)
	onLoop(t, u, func() { nt().enter() })
	waitForTable(t, u, nt().table, "edge")
	var cur string
	onLoop(t, u, func() {
		if n, ok := nt().selected(); ok {
			cur = n.Name
		}
	})
	if cur != "edge" {
		t.Errorf("back on a, the cursor is on %q, want edge", cur)
	}

	toCluster(t, u, b)
	onLoop(t, u, func() { nt().render() }) // from cache, no load
	onLoop(t, u, func() {
		if n, ok := nt().selected(); ok {
			cur = n.Name
		}
	})
	if cur != "public" {
		t.Errorf("back on b, the cursor is on %q, want public", cur)
	}
	if got := mb.callsOf("networks"); got != 1 {
		t.Errorf("b's networks were listed %d times, want 1 (the second visit renders from cache)", got)
	}
}

// Secrets: the delete key asks first, names the secret, and the confirmed
// delete goes to the manager and reloads the list.
func TestSecretsTabDeleteKeyConfirmsAndReloads(t *testing.T) {
	m := &listManager{secrets: []swarm.Secret{secretObj("api_key"), secretObj("db_pw")}}
	u := listUI(t, m)
	st := func() *secretsTab { return u.secTab() }
	onLoop(t, u, func() { st().enter() })
	waitForTable(t, u, st().table, "api_key", "db_pw")

	onLoop(t, u, func() {
		st().table.Select(2, 0)
		st().keys(rune_(u.km.SecDelete))
	})
	if !pageOpen(t, u, pageConfirm) {
		t.Fatal("the delete key must ask first")
	}
	u.app.QueueEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)) // the focused button: Delete
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.lmu.Lock()
		removed := strings.Join(m.removed, ",")
		m.lmu.Unlock()
		if removed == "id-db_pw" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the confirmed delete never reached the manager: removed=%q", removed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	text := waitForTable(t, u, st().table, "api_key")
	if strings.Contains(text, "db_pw") {
		t.Errorf("the deleted secret is still listed:\n%s", text)
	}
}

// Configs: the refresh key on this tab reloads the configs (it used to
// repaint the forwards table), and a failed reload says so in the table.
func TestConfigsTabRefreshReloadsAndShowsErrors(t *testing.T) {
	m := &listManager{configs: []swarm.Config{configObj("nginx.conf")}}
	u := listUI(t, m)
	onLoop(t, u, func() { u.tab("configs").enter() })
	waitForTable(t, u, u.cfgTab().table, "nginx.conf")

	m.lmu.Lock()
	m.cfgErr = errors.New("manager went away")
	m.lmu.Unlock()
	onLoop(t, u, func() { u.tab("configs").refresh() })
	waitForTable(t, u, u.cfgTab().table, "manager went away")
	if got := m.callsOf("configs"); got != 2 {
		t.Errorf("configs listed %d times, want 2 (enter + refresh)", got)
	}
	var kept int
	onLoop(t, u, func() { kept = len(u.cfgTab().rows()) })
	if kept != 1 {
		t.Errorf("a failed reload must keep the last list, has %d rows", kept)
	}
}
