// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmfake"
)

// The Stacks/Services tab end to end: a running UI whose clusters are fake
// swarms behind a real Docker client, so the fetch, the resolver and the
// tree all run as they do against a cluster.

// fakeSwarmClient is a Docker client talking to h — a swarmfake.Swarm or a
// handler wrapping one — over httptest. Both are closed at test end.
func fakeSwarmClient(t *testing.T, h http.Handler) *client.Client {
	t.Helper()
	api := httptest.NewServer(h)
	t.Cleanup(api.Close)
	dcli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(api.URL, "http://")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dcli.Close() })
	return dcli
}

// liveSwarm serves whichever fake swarm it currently holds, so a test can
// change the cluster under a running UI without racing the handler.
type liveSwarm struct {
	cur atomic.Pointer[swarmfake.Swarm]
}

func newLiveSwarm(fs *swarmfake.Swarm) *liveSwarm {
	l := &liveSwarm{}
	l.cur.Store(fs)
	return l
}

func (l *liveSwarm) ServeHTTP(w http.ResponseWriter, r *http.Request) { l.cur.Load().ServeHTTP(w, r) }

// stackedService is a replicated service in stack ("" for none), with the
// manager's own running/desired counts.
func stackedService(id, name, stack string, replicas uint64) swarm.Service {
	s := swarm.Service{ID: id}
	s.Spec.Name = name
	s.Spec.Mode.Replicated = &swarm.ReplicatedService{Replicas: &replicas}
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:1.27"}
	if stack != "" {
		s.Spec.Labels = map[string]string{"com.docker.stack.namespace": stack}
	}
	s.ServiceStatus = &swarm.ServiceStatus{RunningTasks: replicas, DesiredTasks: replicas}
	return s
}

// runningTask is slot of service svcID, running container cid on nodeID.
func runningTask(svcID, cid, nodeID string, slot int) swarm.Task {
	t := swarm.Task{ID: "task-" + cid, ServiceID: svcID, NodeID: nodeID, Slot: slot, DesiredState: swarm.TaskStateRunning}
	t.Status.State = swarm.TaskStateRunning
	t.Status.Timestamp = time.Now().Add(-time.Hour)
	t.Status.ContainerStatus = &swarm.ContainerStatus{ContainerID: cid}
	return t
}

// shopSwarm: stack "shop" with web (two replicas, one per node) and db, and
// an unstacked "loose" service that runs nothing.
func shopSwarm() *swarmfake.Swarm {
	return &swarmfake.Swarm{
		Services: []swarm.Service{
			stackedService("s1", "shop_web", "shop", 2),
			stackedService("s2", "shop_db", "shop", 1),
			stackedService("s3", "loose", "", 0),
		},
		Tasks: []swarm.Task{
			runningTask("s1", "web1aaaaaaaaaaaa", "n1", 1),
			runningTask("s1", "web2bbbbbbbbbbbb", "n2", 2),
			runningTask("s2", "db1ccccccccccccc", "n2", 1),
		},
		Nodes: []swarm.Node{readyNode("n1", "node-a"), readyNode("n2", "node-b")},
	}
}

// opsSwarm is a second, unrelated cluster: three flat services.
func opsSwarm() *swarmfake.Swarm {
	return &swarmfake.Swarm{
		Services: []swarm.Service{
			stackedService("o1", "grafana", "", 1),
			stackedService("o2", "loki", "", 1),
			stackedService("o3", "prometheus", "", 1),
		},
		Tasks: []swarm.Task{runningTask("o2", "loki1ddddddddddd", "m1", 1)},
		Nodes: []swarm.Node{readyNode("m1", "ops-1")},
	}
}

// treeHarness is a running UI with one cluster per fake swarm, connected
// the way connectCluster leaves one, on cluster "a" (which must be in fakes).
// The tab registry is built; nothing is loaded yet.
func treeHarness(t *testing.T, fakes map[string]http.Handler) *ui {
	t.Helper()
	clients := map[string]*client.Client{}
	for name, h := range fakes {
		clients[name] = fakeSwarmClient(t, h)
	}
	u := runningUI(t)
	onLoop(t, u, func() {
		u.km = defaultKeybinds()
		u.forwards = newForwardRegistry()
		u.clusters = map[string]*clusterState{}
		for name, dcli := range clients {
			c := u.clusterState
			if name != "a" {
				c = newClusterState(name, name)
				c.ctx, c.cancel = context.WithCancel(context.Background())
				t.Cleanup(c.cancel)
			}
			c.dcli, c.r = dcli, resolve.New(dcli, resolve.AddrHostname)
			u.clusters[name] = c
		}
		u.activeCtx = "a"
		u.tab("containers")
	})
	return u
}

// toTreeCluster switches to the named cluster as far as the tree is
// concerned, in activateCluster's order: the generation moves, the tree is
// painted from cache, and an explicit reload is started.
func toTreeCluster(t *testing.T, u *ui, name string) {
	t.Helper()
	onLoop(t, u, func() {
		u.gen.Add(1)
		u.clusterState = u.clusters[name]
		u.activeCtx = name
		u.treeTab().render()
		u.treeTab().load()
	})
}

// treeText is the tree as the copy key copies it, read on the UI goroutine.
func treeText(t *testing.T, u *ui) string {
	t.Helper()
	var b strings.Builder
	onLoop(t, u, func() { u.treeTab().yank(&b) })
	return b.String()
}

// waitForTree waits until the tree's copy text holds every want.
func waitForTree(t *testing.T, u *ui, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		text := treeText(t, u)
		all := true
		for _, w := range want {
			all = all && strings.Contains(text, w)
		}
		if all {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatalf("tree never showed %q; it shows:\n%s", want, text)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// cursorRowRaw is the row under the cursor as drawn, fold marker included.
func cursorRowRaw(t *testing.T, u *ui) string {
	t.Helper()
	var text string
	onLoop(t, u, func() {
		if n := u.ctree.GetCurrentNode(); n != nil {
			text = n.GetText()
		}
	})
	return text
}

// cursorRow is the row under the cursor without its fold marker.
func cursorRow(t *testing.T, u *ui) string { return trimFoldMarker(cursorRowRaw(t, u)) }

// cursorTo puts the cursor on the first row whose text contains s.
func cursorTo(t *testing.T, u *ui, s string) {
	t.Helper()
	var found bool
	onLoop(t, u, func() {
		u.treeTab().root.Walk(func(n, _ *tview.TreeNode) bool {
			if !found && strings.Contains(n.GetText(), s) {
				u.ctree.SetCurrentNode(n)
				found = true
			}
			return !found
		})
	})
	if !found {
		t.Fatalf("no row contains %q", s)
	}
}

// Loading fetches the services and the running containers from the manager
// and hangs them under their stacks: a stack row with its counts, the
// services, the containers with their node and slot, and a service with no
// task still listed.
func TestTreeTabLoadsFromTheManager(t *testing.T) {
	u := treeHarness(t, map[string]http.Handler{"a": shopSwarm()})
	onLoop(t, u, func() { u.treeTab().load() })
	text := waitForTree(t, u, "shop_web")

	for _, want := range []string{"shop  [gray](2 svc · 3/3)", "  shop_db", "  shop_web", "(no stack)", "  loose"} {
		if !strings.Contains(text, want) {
			t.Errorf("tree lacks %q:\n%s", want, text)
		}
	}
	for _, want := range []string{"web1aaaaaaaa  node-a  slot 1", "web2bbbbbbbb  node-b  slot 2", "db1ccccccccc  node-b  slot 1"} {
		if !strings.Contains(text, want) {
			t.Errorf("tree lacks the container row %q:\n%s", want, text)
		}
	}
	// Services start folded; the unfold key opens the one under the cursor.
	cursorTo(t, u, "shop_web")
	if got := cursorRowRaw(t, u); !strings.HasPrefix(got, "▸ ") {
		t.Errorf("web starts as %q, want folded", got)
	}
	onLoop(t, u, func() { u.treeTab().keys(rune_(u.km.Unfold)) })
	if got := cursorRowRaw(t, u); !strings.HasPrefix(got, "▾ ") {
		t.Errorf("web after unfold: %q", got)
	}
	onLoop(t, u, func() {
		st := u.treeTab().st()
		if !st.loaded || st.fetchErr != nil || len(st.svcs) != 3 || len(st.cands) != 3 {
			t.Errorf("state after load: loaded %v, err %v, %d services, %d containers", st.loaded, st.fetchErr, len(st.svcs), len(st.cands))
		}
	})
}

// X on a service asks, then removes it at the manager and reloads the tree
// without it; on a container it offers the owning service.
func TestTreeTabRemovesService(t *testing.T) {
	ls := &lockedSwarm{fs: shopSwarm()}
	u := treeHarness(t, map[string]http.Handler{"a": ls})
	onLoop(t, u, func() { u.treeTab().load() })
	waitForTree(t, u, "shop_db")

	cursorTo(t, u, "shop_db")
	onLoop(t, u, func() { u.treeTab().keys(rune_('X')) })
	waitFront(t, u, pageConfirm)
	if text := drawFront(t, u, 200, 40); !strings.Contains(text, `Remove service "shop_db"?`) {
		t.Errorf("confirm:\n%s", text)
	}
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageInfo)
	if text := drawFront(t, u, 200, 40); !strings.Contains(text, `removed service "shop_db"`) {
		t.Errorf("notice:\n%s", text)
	}
	deadline := time.Now().Add(10 * time.Second)
	for strings.Contains(treeText(t, u), "shop_db") {
		if time.Now().After(deadline) {
			t.Fatalf("the tree still shows shop_db:\n%s", treeText(t, u))
		}
		time.Sleep(5 * time.Millisecond)
	}
	ls.mu.Lock()
	var names []string
	for _, s := range ls.fs.Services {
		names = append(names, s.Spec.Name)
	}
	tasks := len(ls.fs.Tasks)
	ls.mu.Unlock()
	if strings.Join(names, ",") != "shop_web,loose" || tasks != 2 {
		t.Errorf("services left %v, %d tasks", names, tasks)
	}

	// On a container row X names the container's service; Esc keeps it.
	onLoop(t, u, func() { u.pages.RemovePage(pageInfo) })
	cursorTo(t, u, "shop_web")
	onLoop(t, u, func() { u.treeTab().keys(rune_(u.km.Unfold)) })
	cursorTo(t, u, "web1aaaaaaaa")
	onLoop(t, u, func() { u.treeTab().keys(rune_('X')) })
	waitFront(t, u, pageConfirm)
	if text := drawFront(t, u, 200, 40); !strings.Contains(text, `Remove service "shop_web"?`) {
		t.Errorf("confirm on a container:\n%s", text)
	}
}

// Each cluster keeps its own cursor: a switch paints the other cluster's
// tree at its own first row, and a switch back lands where the operator
// stood — from the cache at once, and still there once the reload is in.
func TestTreeTabKeepsTheCursorPerCluster(t *testing.T) {
	shop := newLiveSwarm(shopSwarm())
	u := treeHarness(t, map[string]http.Handler{"a": shop, "b": opsSwarm()})
	onLoop(t, u, func() { u.treeTab().load() })
	waitForTree(t, u, "shop_db")
	cursorTo(t, u, "shop_db")

	toTreeCluster(t, u, "b")
	waitForTree(t, u, "grafana")
	if got := cursorRow(t, u); !strings.HasPrefix(strings.TrimSpace(got), "grafana") {
		t.Fatalf("first visit to b: cursor on %q, want its first service", got)
	}
	cursorTo(t, u, "prometheus")

	// Meanwhile a gains a service, so the test can tell when the reload is in.
	grown := shopSwarm()
	grown.Services = append(grown.Services, stackedService("s4", "shop_cache", "shop", 0))
	shop.cur.Store(grown)

	toTreeCluster(t, u, "a")
	if got := cursorRow(t, u); !strings.Contains(got, "shop_db") {
		t.Errorf("back on a, from cache: cursor on %q, want shop_db", got)
	}
	waitForTree(t, u, "shop_cache")
	if got := cursorRow(t, u); !strings.Contains(got, "shop_db") {
		t.Errorf("back on a, after the reload: cursor on %q, want shop_db", got)
	}

	toTreeCluster(t, u, "b")
	if got := cursorRow(t, u); !strings.Contains(got, "prometheus") {
		t.Errorf("back on b: cursor on %q, want prometheus", got)
	}
}

// The "/" filter narrows the tree locally — no manager call per keystroke —
// belongs to its cluster, and the copy key copies what it shows.
func TestTreeTabFilterIsLocalAndPerCluster(t *testing.T) {
	var calls atomic.Int32
	shop := shopSwarm()
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/services") {
			calls.Add(1)
		}
		shop.ServeHTTP(w, r)
	})
	u := treeHarness(t, map[string]http.Handler{"a": counting, "b": opsSwarm()})
	onLoop(t, u, func() { u.treeTab().load() })
	waitForTree(t, u, "shop_web")
	before := calls.Load()

	var s tabSearcher
	onLoop(t, u, func() {
		s = u.treeTab()
		s.setSearch("node-b") // web's second replica and db, not web's first
		u.treeTab().groupByStack = false
		u.treeTab().render()
	})
	text := treeText(t, u)
	if strings.Contains(text, "loose") || strings.Contains(text, "web1aaaa") {
		t.Errorf("filtered tree still shows what is not on node-b:\n%s", text)
	}
	if !strings.Contains(text, "web2bbbb") || !strings.Contains(text, "db1cccc") {
		t.Errorf("filtered tree lacks node-b's containers:\n%s", text)
	}
	if calls.Load() != before {
		t.Error("filtering asked the manager again")
	}

	toTreeCluster(t, u, "b")
	waitForTree(t, u, "grafana", "loki", "prometheus")
	onLoop(t, u, func() {
		if q := s.searchText(); q != "" {
			t.Errorf("b's filter = %q, want a's not to leak", q)
		}
	})
	toTreeCluster(t, u, "a")
	onLoop(t, u, func() {
		if q := s.searchText(); q != "node-b" {
			t.Errorf("a's filter = %q, want it kept", q)
		}
	})
	if text := treeText(t, u); strings.Contains(text, "loose") {
		t.Errorf("a's tree after the switch back is unfiltered:\n%s", text)
	}
}

// The stack-group key flips between the grouped and the flat tree, and is a
// preference of the session: it holds on the next cluster too.
func TestTreeTabStackGroupKey(t *testing.T) {
	u := treeHarness(t, map[string]http.Handler{"a": shopSwarm(), "b": opsSwarm()})
	onLoop(t, u, func() { u.treeTab().load() })
	waitForTree(t, u, "shop_web")

	onLoop(t, u, func() { u.treeTab().keys(rune_(u.km.StackGroup)) })
	if text := treeText(t, u); strings.Contains(text, "svc ·") || !strings.HasPrefix(text, "loose") {
		t.Errorf("flat tree still grouped:\n%s", text)
	}
	onLoop(t, u, func() {
		if !strings.Contains(u.help.GetText(true), "flat service list") {
			t.Errorf("flash = %q", u.help.GetText(true))
		}
	})

	// b has no stacks: the key says so instead of redrawing the same tree.
	toTreeCluster(t, u, "b")
	waitForTree(t, u, "grafana")
	onLoop(t, u, func() {
		u.treeTab().keys(rune_(u.km.StackGroup))
		if !strings.Contains(u.help.GetText(true), "no service carries a stack label") {
			t.Errorf("flash = %q", u.help.GetText(true))
		}
		if u.treeTab().groupByStack {
			t.Error("the toggle flipped on a cluster with nothing to group")
		}
	})
}

// A failed fetch shows the error in the tree and is recorded as such; the
// background poll does not put one up (a transient error must not clobber a
// good tree), and picks the change up once the manager answers.
func TestTreeTabFetchErrorAndAutoRefresh(t *testing.T) {
	live := newLiveSwarm(shopSwarm())
	var failing atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() && strings.HasSuffix(r.URL.Path, "/services") {
			http.Error(w, `{"message":"manager is restarting"}`, http.StatusServiceUnavailable)
			return
		}
		live.ServeHTTP(w, r)
	})
	u := treeHarness(t, map[string]http.Handler{"a": h})
	onLoop(t, u, func() { u.treeTab().load() })
	waitForTree(t, u, "shop_db")
	cursorTo(t, u, "shop_db")

	failing.Store(true)
	onLoop(t, u, func() { u.treeTab().autoRefresh() }) // on the loop, as pollTick hands it over
	waitFor(t, "the failed poll to finish", func() bool { return !u.autoRefreshBusy.Load() })
	if text := treeText(t, u); !strings.Contains(text, "shop_db") {
		t.Fatalf("a failed poll replaced the tree:\n%s", text)
	}

	onLoop(t, u, func() { u.treeTab().load() })
	waitForTree(t, u, "manager is restarting")
	onLoop(t, u, func() {
		if st := u.treeTab().st(); st.fetchErr == nil || len(st.svcs) != 3 {
			t.Errorf("after a failed reload: err %v, %d services kept", st.fetchErr, len(st.svcs))
		}
	})

	// The manager is back, with a service more. The poll brings it in, and
	// the cursor is back on the row it stood on before the error.
	next := shopSwarm()
	next.Services = append(next.Services, stackedService("s4", "shop_cache", "shop", 0))
	live.cur.Store(next)
	failing.Store(false)
	onLoop(t, u, func() { u.treeTab().autoRefresh() })
	waitForTree(t, u, "shop_cache")
	if got := cursorRow(t, u); !strings.Contains(got, "shop_db") {
		t.Errorf("cursor after the error cleared: %q, want shop_db", got)
	}
}
