// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/swarmfake"
	"swarmexec/internal/pb"
)

// The log views driven end to end: the stream comes from a fake agent over
// gRPC, the candidates from a fake swarm through the real resolver, and the
// keys go through the view's own input capture.

// logSwarm is shopSwarm plus a crash-looping service (its last container
// exited on node-a) and one that never got a container.
func logSwarm() *swarmfake.Swarm {
	fs := shopSwarm()
	fs.Services = append(fs.Services, stackedService("s4", "crashy", "", 1), stackedService("s5", "pending", "", 1))
	dead := runningTask("s4", "crash1eeeeeeeeee", "n1", 1)
	dead.Status.State, dead.DesiredState = swarm.TaskStateFailed, swarm.TaskStateShutdown
	dead.Status.Err = "task: non-zero exit (1)"
	stuck := swarm.Task{ID: "task-pending", ServiceID: "s5", Slot: 1, DesiredState: swarm.TaskStateRunning}
	stuck.Status.State, stuck.Status.Err = swarm.TaskStatePending, "no suitable node (scheduling constraints not satisfied)"
	stuck.Status.Timestamp = time.Now()
	fs.Tasks = append(fs.Tasks, dead, stuck)
	return fs
}

// logsUI is a running UI on the log swarm whose two nodes stream the given
// lines, with the tree loaded.
func logsUI(t *testing.T, a, b *fakeAgent) *ui {
	t.Helper()
	u := treeHarness(t, map[string]http.Handler{"a": logSwarm()})
	cfg := startAgents(t, map[string]pb.AgentServer{"node-a": a, "node-b": b})
	onLoop(t, u, func() {
		u.status = tview.NewTextView()
		u.cfg = cfg
		u.agents = agentcall.New(cfg)
		u.f = &uiFlags{connectTimeout: 3 * time.Second}
		u.pages.AddPage(pageMain, u.ctree, true, true)
		u.treeTab().load()
	})
	t.Cleanup(func() { u.agents.Close() })
	waitForTree(t, u, "shop_web", "crashy")
	return u
}

// logView is the open log view's text view.
func logView(t *testing.T, u *ui) *tview.TextView {
	t.Helper()
	var tv *tview.TextView
	onLoop(t, u, func() {
		if f, ok := u.pages.GetPage(pageLogs).(*tview.Flex); ok {
			tv, _ = f.GetItem(0).(*tview.TextView)
		}
	})
	if tv == nil {
		t.Fatal("no log view open")
	}
	return tv
}

// focusLog gives the keyboard back to the log view.
func focusLog(t *testing.T, u *ui) {
	t.Helper()
	tv := logView(t, u)
	onLoop(t, u, func() { u.app.SetFocus(tv) })
}

func logTitle(t *testing.T, u *ui) string {
	t.Helper()
	tv := logView(t, u)
	var s string
	onLoop(t, u, func() { s = tv.GetTitle() })
	return s
}

// waitLogText waits until the log view's text holds every want.
func waitLogText(t *testing.T, u *ui, want ...string) string {
	t.Helper()
	tv := logView(t, u)
	var text string
	waitUntil(t, fmt.Sprintf("log text %q", want), func() bool {
		onLoop(t, u, func() { text = tv.GetText(true) })
		for _, w := range want {
			if !strings.Contains(text, w) {
				return false
			}
		}
		return true
	})
	return text
}

func webCandidate() resolve.Candidate {
	return resolve.Candidate{Service: "shop_web", Slot: 1, NodeID: "n1", NodeName: "node-a", DialHost: "node-a", ContainerID: "web1aaaaaaaaaaaa"}
}

// One container's logs: the lines arrive, and every key of the view does
// what its footer says.
func TestContainerLogViewKeys(t *testing.T) {
	a := &fakeAgent{logs: []string{"starting up", "listening on :80", "GET /health 200"}, stderr: []string{"warning: slow disk"}}
	u := logsUI(t, a, &fakeAgent{})

	onLoop(t, u, func() { u.showLogs(webCandidate()) })
	waitLogText(t, u, "starting up", "listening on :80", "warning: slow disk")
	if got := a.logCalls(); got != 1 {
		t.Errorf("%d log streams opened, want 1", got)
	}
	a.mu.Lock()
	req := a.logReqs[0]
	a.mu.Unlock()
	if req.GetContainerId() != "web1aaaaaaaaaaaa" || !req.GetFollow() || req.GetTail() != 1000 {
		t.Errorf("logs request %+v", req)
	}
	if title := logTitle(t, u); !strings.Contains(title, "logs web1aaaaaaaa on node-a — follow:on") {
		t.Errorf("title %q", title)
	}
	page := pageText(t, u, pageLogs)
	if !strings.Contains(page, "f follow") || !strings.Contains(page, "mouse") {
		t.Errorf("footer hint missing:\n%s", page)
	}

	sendRune(t, u, 'f')
	if title := logTitle(t, u); !strings.Contains(title, "follow:off") {
		t.Errorf("after f: %q", title)
	}
	sendRune(t, u, 'f')
	if title := logTitle(t, u); !strings.Contains(title, "follow:on") {
		t.Errorf("after f again: %q", title)
	}

	before := logTitle(t, u)
	sendRune(t, u, 'F')
	if after := logTitle(t, u); after == before {
		t.Errorf("F left the title at %q", after)
	}
	before = logTitle(t, u)
	sendRune(t, u, 'l')
	if after := logTitle(t, u); after == before {
		t.Errorf("l left the title at %q", after)
	}

	sendRune(t, u, 'a')
	waitPageText(t, u, pageInfo, "log format")
	closeInfo(t, u)

	var mouse bool
	focusLog(t, u)
	onLoop(t, u, func() { mouse = u.mouseEnabled })
	sendRune(t, u, 'm')
	var now bool
	onLoop(t, u, func() { now = u.mouseEnabled })
	if now == mouse {
		t.Error("m did not toggle the mouse")
	}
	wantHint := "mouse: app"
	if !now {
		wantHint = "mouse: off"
	}
	if page := pageText(t, u, pageLogs); !strings.Contains(page, wantHint) {
		t.Errorf("footer does not say %q:\n%s", wantHint, page)
	}

	// "/" filters the messages; a bad regexp says so; empty clears.
	sendRune(t, u, '/')
	waitPageText(t, u, pageLogGrep, "filter logs", "grep:")
	typeText(t, u, "health")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageGone(t, u, pageLogGrep)
	text := waitLogText(t, u, "GET /health 200")
	if strings.Contains(text, "starting up") {
		t.Errorf("grep kept a line it should drop:\n%s", text)
	}
	if title := logTitle(t, u); !strings.Contains(title, "health") {
		t.Errorf("title does not name the grep: %q", title)
	}

	focusLog(t, u)
	sendRune(t, u, '/')
	waitPageText(t, u, pageLogGrep, "grep:")
	typeText(t, u, "(")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, "invalid grep regexp")
	closeInfo(t, u)

	focusLog(t, u)
	sendRune(t, u, '/')
	waitPageText(t, u, pageLogGrep, "grep:")
	sendSpecial(t, u, tcell.KeyEnter) // empty: clears
	waitLogText(t, u, "starting up", "GET /health 200")

	focusLog(t, u)
	sendRune(t, u, '/')
	waitPageText(t, u, pageLogGrep, "grep:")
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageLogGrep)

	// Esc closes the view and ends the stream.
	focusLog(t, u)
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageLogs)
}

// A node without an agent: the view says what went wrong instead of
// staying empty.
func TestContainerLogViewShowsAStreamError(t *testing.T) {
	u := logsUI(t, &fakeAgent{}, &fakeAgent{})
	c := webCandidate()
	c.DialHost, c.NodeName = "node-z", "node-z"
	onLoop(t, u, func() { u.showLogs(c) })
	waitLogText(t, u, "error:")
	sendRune(t, u, 'q')
	waitPageGone(t, u, pageLogs)
}

// The tree's log key: a container leaf opens that container, a service its
// containers (each line tagged with the slot), a stack every container of
// the stack (tagged with service.slot).
func TestLogsForTreeNodes(t *testing.T) {
	a := &fakeAgent{logs: []string{"from node a"}}
	b := &fakeAgent{logs: []string{"from node b"}}
	u := logsUI(t, a, b)

	leaf := tview.NewTreeNode("web1").SetReference(webCandidate())
	onLoop(t, u, func() { u.showLogsForNode(leaf) })
	waitLogText(t, u, "from node a")
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageLogs)

	var svc *tview.TreeNode
	onLoop(t, u, func() {
		st := u.treeTab().st()
		svc = tview.NewTreeNode("▾ shop_web  replicated").SetReference(svcRef{name: "shop_web"})
		for _, c := range st.cands {
			if c.Service == "shop_web" {
				svc.AddChild(tview.NewTreeNode(c.ContainerID).SetReference(c))
			}
		}
	})
	onLoop(t, u, func() { u.showLogsForNode(svc) })
	waitLogText(t, u, "[slot 1] from node a", "[slot 2] from node b")
	if title := logTitle(t, u); !strings.Contains(title, "service logs shop_web (2 containers)") {
		t.Errorf("service title %q", title)
	}
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageLogs)

	onLoop(t, u, func() { u.showLogsForNode(tview.NewTreeNode("shop").SetReference(stackRef{name: "shop"})) })
	waitLogText(t, u, "[shop_db.1] from node b", "[shop_web.1] from node a", "[shop_web.2] from node b")
	if title := logTitle(t, u); !strings.Contains(title, "stack logs shop (3 containers)") {
		t.Errorf("stack title %q", title)
	}
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageLogs)

	// Neither a container, a service nor a stack: nothing opens.
	onLoop(t, u, func() { u.showLogsForNode(tview.NewTreeNode("just text")) })
	if pageOpen(t, u, pageLogs) {
		t.Error("a plain row opened a log view")
	}
}

// A service with no running container falls back to the last one it ran,
// or says why it has none.
func TestLogsOfAServiceWithoutRunningContainers(t *testing.T) {
	a := &fakeAgent{logs: []string{"panic: config missing"}}
	u := logsUI(t, a, &fakeAgent{})

	onLoop(t, u, func() { u.showLogsForNode(tview.NewTreeNode("crashy").SetReference(svcRef{name: "crashy"})) })
	waitPage(t, u, pageLogs) // the last container is looked up first
	text := waitLogText(t, u, "panic: config missing")
	if !strings.Contains(text, "crashy") {
		t.Errorf("the last-logs note does not name the service:\n%s", text)
	}
	if title := logTitle(t, u); !strings.Contains(title, "last logs crashy") {
		t.Errorf("title %q", title)
	}
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageLogs)

	onLoop(t, u, func() { u.showLogsForNode(tview.NewTreeNode("pending").SetReference(svcRef{name: "pending"})) })
	waitPageText(t, u, pageInfo, "service pending has no container to read logs from", "no suitable node")
	closeInfo(t, u)

	onLoop(t, u, func() { u.showLastExitedLogs("loose") })
	text = waitPageText(t, u, pageInfo, "service loose has no container to read logs from.")
	if strings.Contains(text, "Newest task") {
		t.Errorf("a service without tasks has no newest task:\n%s", text)
	}
	closeInfo(t, u)

	onLoop(t, u, func() { u.showLastExitedLogs("ghost") })
	waitPageText(t, u, pageInfo, "logs ghost:")
	closeInfo(t, u)
}

// The stack log view refuses the display-only group, an empty stack and one
// too big to follow.
func TestStackLogsRefusals(t *testing.T) {
	u := logsUI(t, &fakeAgent{}, &fakeAgent{})

	onLoop(t, u, func() { u.showStackLogs(noStackLabel) })
	waitPageText(t, u, pageInfo, "is a display grouping, not a stack")
	closeInfo(t, u)

	onLoop(t, u, func() { u.showStackLogs("nothing") })
	waitPageText(t, u, pageInfo, "stack nothing has no running container to follow.")
	closeInfo(t, u)

	onLoop(t, u, func() {
		st := u.treeTab().st()
		for i := 0; i < stackLogLimit+1; i++ {
			name := fmt.Sprintf("big_s%02d", i)
			st.svcs = append(st.svcs, resolve.Service{Name: name, Stack: "big"})
			st.cands = append(st.cands, resolve.Candidate{Service: name, Slot: 1})
		}
	})
	onLoop(t, u, func() { u.showStackLogs("big") })
	waitPageText(t, u, pageInfo, fmt.Sprintf("stack big has %d running containers", stackLogLimit+1))
	closeInfo(t, u)
}

// logs.format: auto in the config arms detection in a new view; an invalid
// config falls back to the defaults.
func TestLogDefaultsFromConfig(t *testing.T) {
	u := chromeUI(t)
	onLoop(t, u, func() {
		u.cfg.Logs.Format = "auto"
		if _, _, auto := u.logDefaults(); !auto {
			t.Error("format auto did not arm detection")
		}
		u.cfg.Logs.Format = "no-such-format"
		if _, _, auto := u.logDefaults(); auto {
			t.Error("an invalid format armed detection")
		}
	})
}
