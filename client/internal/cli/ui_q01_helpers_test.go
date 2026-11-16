// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/clientlog"
	"swarmexec/client/internal/swarmfake"
	"swarmexec/internal/pb"
)

// Shared helpers of the review-Q-01 hotspot tests (tree keys, prune dialog,
// inspect keys, the service editors, the secret detail): a running UI over a
// fake swarm whose specs a test can read back after an editor wrote them.

// lockedSwarm serves a fake swarm under a lock the test shares, so reading a
// service back after an editor's update does not race the handler.
type lockedSwarm struct {
	mu sync.Mutex
	fs *swarmfake.Swarm
}

func (l *lockedSwarm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fs.ServeHTTP(w, r)
}

// spec is the named service's spec as the fake holds it now.
func (l *lockedSwarm) spec(t *testing.T, name string) swarm.ServiceSpec {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.fs.Services {
		if s.Spec.Name == name {
			return s.Spec
		}
	}
	t.Fatalf("no service %q in the fake", name)
	return swarm.ServiceSpec{}
}

// version is the named service's version index: it moves on every update.
func (l *lockedSwarm) version(name string) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.fs.Services {
		if s.Spec.Name == name {
			return s.Version.Index
		}
	}
	return 0
}

// editSwarm is a cluster with one service of every shape the editors meet:
// "web" (replicated, with a port, a label, env, a volume mount, a secret and
// a network) and "agent" (global). Two nodes, two secrets, two networks.
func editSwarm() *swarmfake.Swarm {
	web := stackedService("s1", "web", "", 2)
	web.Version.Index = 1
	web.Spec.Labels = map[string]string{"team": "core"}
	web.Spec.EndpointSpec = &swarm.EndpointSpec{Ports: []swarm.PortConfig{{
		Protocol: "tcp", PublishedPort: 8080, TargetPort: 80, PublishMode: swarm.PortConfigPublishModeIngress,
	}}}
	cs := web.Spec.TaskTemplate.ContainerSpec
	cs.Env = []string{"A=1"}
	cs.Mounts = []mount.Mount{{Type: mount.TypeVolume, Source: "data", Target: "/data"}}
	cs.Secrets = []*swarm.SecretReference{{
		SecretName: "api_key", SecretID: "sec-api",
		File: &swarm.SecretReferenceFileTarget{Name: "api_key", UID: "0", GID: "0", Mode: 0o444},
	}}
	web.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{{Target: "net-front", Aliases: []string{"www"}}}

	agent := stackedService("s2", "agent", "", 0)
	agent.Version.Index = 1
	agent.Spec.Mode = swarm.ServiceMode{Global: &swarm.GlobalService{}}

	n1, n2 := readyNode("n1", "node-a"), readyNode("n2", "node-b")
	n1.Spec.Role, n2.Spec.Role = swarm.NodeRoleManager, swarm.NodeRoleWorker
	n1.Spec.Availability, n2.Spec.Availability = swarm.NodeAvailabilityActive, swarm.NodeAvailabilityActive
	n1.Spec.Labels = map[string]string{"zone": "a"}
	n2.Spec.Labels = map[string]string{"zone": "b"}
	return &swarmfake.Swarm{
		Services: []swarm.Service{web, agent},
		Tasks:    []swarm.Task{runningTask("s1", "web1aaaaaaaaaaaa", "n1", 1)},
		Nodes:    []swarm.Node{n1, n2},
		Secrets: []swarm.Secret{
			{ID: "sec-api", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "api_key"}}},
			{ID: "sec-db", Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: "db_pass"}}},
		},
		Networks: []network.Summary{
			{Network: network.Network{ID: "net-front", Name: "front", Driver: "overlay", Scope: "swarm"}},
			{Network: network.Network{ID: "net-back", Name: "back", Driver: "overlay", Scope: "swarm"}},
		},
	}
}

// editUI is a running UI on cluster "a" = fs, with node agents behind both
// of its nodes (node-a holds the volumes "data" and "cache"), and the client
// log captured so a test can wait for the notice an editor ends with.
func editUI(t *testing.T, fs *swarmfake.Swarm) (*ui, *lockedSwarm, *clientlog.Ring) {
	t.Helper()
	return editUIWith(t, fs, map[string]pb.AgentServer{
		"node-a": &volumeAgent{vols: []string{"data", "cache"}},
		"node-b": &volumeAgent{},
	})
}

// editUIWith is editUI with the given agent per node hostname.
func editUIWith(t *testing.T, fs *swarmfake.Swarm, agents map[string]pb.AgentServer) (*ui, *lockedSwarm, *clientlog.Ring) {
	t.Helper()
	ring := captureLog(t)
	ls := &lockedSwarm{fs: fs}
	cfg := startAgents(t, agents)
	u := treeHarness(t, map[string]http.Handler{"a": ls})
	onLoop(t, u, func() {
		u.cfg = cfg
		u.agents = agentcall.New(cfg)
		u.f = &uiFlags{connectTimeout: 3 * time.Second}
	})
	t.Cleanup(func() { u.agents.Close() })
	return u, ls, ring
}

// key sends ev to the front page on the UI goroutine, the way the running
// app routes a keystroke: down to whatever has focus.
func key(t *testing.T, u *ui, ev *tcell.EventKey) {
	t.Helper()
	onLoop(t, u, func() {
		_, page := u.pages.GetFrontPage()
		page.InputHandler()(ev, func(p tview.Primitive) { u.app.SetFocus(p) })
	})
}

func keyRune(t *testing.T, u *ui, r rune) { t.Helper(); key(t, u, rune_(r)) }

func keyCode(t *testing.T, u *ui, k tcell.Key) {
	t.Helper()
	key(t, u, tcell.NewEventKey(k, 0, tcell.ModNone))
}

// waitFront waits until the front page is name.
func waitFront(t *testing.T, u *ui, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for frontPage(t, u) != name {
		if time.Now().After(deadline) {
			t.Fatalf("front page is %q, never became %q", frontPage(t, u), name)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// focusedAs is the focused primitive as a T, read on the UI goroutine.
func focusedAs[T tview.Primitive](t *testing.T, u *ui) T {
	t.Helper()
	var p tview.Primitive
	onLoop(t, u, func() { p = u.app.GetFocus() })
	v, ok := p.(T)
	if !ok {
		t.Fatalf("focus is on %T", p)
	}
	return v
}

// frontForm is the form in the front overlay.
func frontForm(t *testing.T, u *ui) *tview.Form {
	t.Helper()
	var f *tview.Form
	onLoop(t, u, func() {
		_, p := u.pages.GetFrontPage()
		f = findForm(p)
	})
	if f == nil {
		t.Fatal("no form in the front overlay")
	}
	return f
}

func findForm(p tview.Primitive) *tview.Form {
	switch w := p.(type) {
	case *tview.Form:
		return w
	case *centeredBox:
		return findForm(w.inner)
	case *tview.Flex:
		for i := 0; i < w.GetItemCount(); i++ {
			if f := findForm(w.GetItem(i)); f != nil {
				return f
			}
		}
	}
	return nil
}

// formTitle is a form's title, read on the UI goroutine.
func formTitle(t *testing.T, u *ui, f *tview.Form) string {
	t.Helper()
	var s string
	onLoop(t, u, func() { s = f.GetTitle() })
	return s
}

// setField types text into the form item labelled label.
func setField(t *testing.T, u *ui, f *tview.Form, label, text string) {
	t.Helper()
	onLoop(t, u, func() {
		switch w := f.GetFormItemByLabel(label).(type) {
		case *tview.InputField:
			w.SetText(text)
		case *tview.TextArea:
			w.SetText(text, true)
		default:
			t.Errorf("form item %q is %T", label, w)
		}
	})
}

// toggleCheck flips the checkbox labelled label the way space does, so its
// changed func runs.
func toggleCheck(t *testing.T, u *ui, f *tview.Form, label string) {
	t.Helper()
	onLoop(t, u, func() {
		cb, ok := f.GetFormItemByLabel(label).(*tview.Checkbox)
		if !ok {
			t.Errorf("form item %q is not a checkbox", label)
			return
		}
		cb.InputHandler()(rune_(' '), func(tview.Primitive) {})
	})
}

// clickButton presses the form's button labelled label.
func clickButton(t *testing.T, u *ui, f *tview.Form, label string) {
	t.Helper()
	onLoop(t, u, func() {
		i := f.GetButtonIndex(label)
		if i < 0 {
			t.Errorf("no button %q", label)
			return
		}
		f.GetButton(i).InputHandler()(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone), func(p tview.Primitive) { u.app.SetFocus(p) })
	})
}

// answer types text into the focused one-line prompt (an InputField or a
// wrapping TextArea) and presses Enter.
func answer(t *testing.T, u *ui, text string) {
	t.Helper()
	onLoop(t, u, func() {
		switch w := u.app.GetFocus().(type) {
		case *tview.InputField:
			w.SetText(text)
		case *tview.TextArea:
			w.SetText(text, true)
		default:
			t.Errorf("the prompt focuses %T", w)
		}
	})
	keyCode(t, u, tcell.KeyEnter)
}

// confirmAndWait presses the confirm's first button and waits for the
// notice the action ends with.
func confirmAndWait(t *testing.T, u *ui, ring *clientlog.Ring, notice string) {
	t.Helper()
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyEnter)
	waitForLog(t, ring, "ui notice", notice)
}

// textOf is a TextView's text without colour tags, read on the UI goroutine.
func textOf(t *testing.T, u *ui, tv *tview.TextView) string {
	t.Helper()
	var s string
	onLoop(t, u, func() { s = tv.GetText(true) })
	return s
}

// drawFront draws the front page onto a w×h simulation screen and returns
// what it shows, one line per row, trailing blanks trimmed.
func drawFront(t *testing.T, u *ui, w, h int) string {
	t.Helper()
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(w, h)
	onLoop(t, u, func() {
		_, p := u.pages.GetFrontPage()
		p.SetRect(0, 0, w, h)
		p.Draw(scr)
	})
	scr.Show()
	cells, cw, _ := scr.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		var line []rune
		for x := 0; x < cw; x++ {
			if c := cells[y*cw+x]; len(c.Runes) > 0 {
				line = append(line, c.Runes[0])
			} else {
				line = append(line, ' ')
			}
		}
		b.WriteString(strings.TrimRight(string(line), " ") + "\n")
	}
	return b.String()
}

// hasAll reports whether s holds every want.
func hasAll(s string, want ...string) bool {
	for _, w := range want {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}
