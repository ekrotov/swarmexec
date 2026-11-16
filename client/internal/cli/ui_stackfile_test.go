// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/stackfile"
	"swarmexec/client/internal/swarmfake"
)

// The stack-file dialogs of the Stacks tab — export, compare, check-and-deploy
// — driven through the menu against a fake swarm behind a real Docker client.

// deploySwarm is a swarmfake that also takes the network calls a stack deploy
// makes (inspect by name, create), and can refuse service creates.
type deploySwarm struct {
	*swarmfake.Swarm
	mu         sync.Mutex
	nets       map[string]bool
	failCreate atomic.Bool
}

var apiPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func (d *deploySwarm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := apiPrefix.ReplaceAllString(r.URL.Path, "")
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodPost && path == "/networks/create":
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		d.mu.Lock()
		d.nets[body.Name] = true
		d.mu.Unlock()
		reply(http.StatusCreated, map[string]string{"Id": body.Name})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/networks/"):
		name := strings.TrimPrefix(path, "/networks/")
		d.mu.Lock()
		ok := d.nets[name]
		d.mu.Unlock()
		if !ok {
			reply(http.StatusNotFound, map[string]string{"message": "network " + name + " not found"})
			return
		}
		reply(http.StatusOK, map[string]string{"Id": name, "Name": name, "Scope": "swarm", "Driver": "overlay"})
	case r.Method == http.MethodPost && path == "/services/create" && d.failCreate.Load():
		reply(http.StatusInternalServerError, map[string]string{"message": "no space left on manager"})
	default:
		d.Swarm.ServeHTTP(w, r)
	}
}

func (d *deploySwarm) network(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.nets[name]
}

// stackUI is a running UI on the shop swarm with the tree loaded and shown.
func stackUI(t *testing.T) (*ui, *deploySwarm) {
	t.Helper()
	d := &deploySwarm{Swarm: shopSwarm(), nets: map[string]bool{}}
	u := treeHarness(t, map[string]http.Handler{"a": d})
	onLoop(t, u, func() {
		u.status = tview.NewTextView()
		u.pages.AddPage(pageMain, u.ctree, true, true)
		u.treeTab().load()
	})
	waitForTree(t, u, "shop_web")
	return u, d
}

// openStackMenuItem opens the menu on the cursor's stack, picks item i and
// answers the file prompt with path.
func openStackMenuItem(t *testing.T, u *ui, i int, path string) {
	t.Helper()
	onLoop(t, u, func() { u.openStackFileMenu() })
	waitPageText(t, u, pageMenu, "stack shop", "export to a file", "compare with a file", "deploy a file")
	for ; i > 0; i-- {
		sendSpecial(t, u, tcell.KeyDown)
	}
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageStackFile, "file:", "shop.yml") // the preset
	setFocusedText(t, u, path)
	sendSpecial(t, u, tcell.KeyEnter)
}

func writeStackFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Which stack the key acts on: the stack row, a service in it, a container
// of it — and none for an unstacked service.
func TestStackUnderCursor(t *testing.T) {
	u, _ := stackUI(t)
	for row, want := range map[string]string{"shop_db": "shop", "web1aaaaaaaa": "shop", "loose": ""} {
		onLoop(t, u, func() { u.treeTab().keys(rune_(u.km.Unfold)) })
		cursorTo(t, u, row)
		var got string
		onLoop(t, u, func() { got = u.stackUnderCursor() })
		if got != want {
			t.Errorf("cursor on %s: stack %q, want %q", row, got, want)
		}
	}
	cursorTo(t, u, "loose")
	onLoop(t, u, func() { u.openStackFileMenu() })
	waitFooter(t, u, "no stack under the cursor")
	if pageOpen(t, u, pageMenu) {
		t.Error("the menu opened without a stack")
	}
	if got := defaultStackPath("shop"); got != "shop.yml" {
		t.Errorf("default path %q", got)
	}
}

// Export writes the deployed stack; comparing the file with the cluster
// finds nothing, an edited one shows the change; a missing file is an error.
func TestStackExportAndCompare(t *testing.T) {
	u, _ := stackUI(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "shop.yml")
	cursorTo(t, u, "shop_web")

	openStackMenuItem(t, u, 0, file)
	waitPageText(t, u, pageInfo, "wrote "+file+" — 2 service(s)")
	closeInfo(t, u)
	body, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(body), "nginx:1.27") {
		t.Fatalf("exported file (%v):\n%s", err, body)
	}

	cursorTo(t, u, "shop_web")
	openStackMenuItem(t, u, 1, file)
	// Not "No differences": the fake's services sit on no network, and the
	// file read back lands on the stack's default one.
	waitPageText(t, u, pageStackDiff, "vs deployed shop", "+ networks:")
	sendRune(t, u, 'j')
	sendRune(t, u, 'k')
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageStackDiff)

	writeStackFile(t, file, strings.Replace(string(body), "nginx:1.27", "nginx:1.28", 1))
	cursorTo(t, u, "shop_web")
	openStackMenuItem(t, u, 1, file)
	waitPageText(t, u, pageStackDiff, "- deployed shop", "+ "+file, "@@", "nginx:1.28", "nginx:1.27")
	sendRune(t, u, 'q')
	waitPageGone(t, u, pageStackDiff)

	cursorTo(t, u, "shop_web")
	openStackMenuItem(t, u, 1, filepath.Join(dir, "missing.yml"))
	waitPageText(t, u, pageInfo, "compare failed")
	closeInfo(t, u)

	// Export below a regular file fails and says so.
	cursorTo(t, u, "shop_web")
	openStackMenuItem(t, u, 0, filepath.Join(file, "x.yml"))
	waitPageText(t, u, pageInfo, "export failed")
	closeInfo(t, u)

	// The prompt's Esc and the menu's Esc both back out.
	onLoop(t, u, func() { u.openStackFileMenu() })
	waitPageText(t, u, pageMenu, "stack shop")
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageMenu)
	onLoop(t, u, func() { u.openStackFileMenu() })
	waitPageText(t, u, pageMenu, "stack shop")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageStackFile, "file:")
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageStackFile)
}

const cleanStack = `services:
  web:
    image: nginx:1.28
    read_only: true
    user: "1000"
`

const riskyStack = `services:
  web:
    image: nginx:1.28
    privileged: true
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
`

// Deploy is a check first: a clean file deploys on "d" straight away, a
// flagged one asks once more; the result is reported and the tree reloaded.
func TestStackDeployFromFile(t *testing.T) {
	u, d := stackUI(t)
	dir := t.TempDir()
	clean, risky := filepath.Join(dir, "clean.yml"), filepath.Join(dir, "risky.yml")
	writeStackFile(t, clean, cleanStack)
	writeStackFile(t, risky, riskyStack)

	cursorTo(t, u, "shop_web")
	openStackMenuItem(t, u, 2, clean)
	waitPageText(t, u, pageStackPlan, "deploy shop from "+clean, "Nothing above informational", "across 1 service(s)", "Checked:")
	sendRune(t, u, 'j')
	sendRune(t, u, 'k')
	sendRune(t, u, 'd')
	waitPageText(t, u, pageInfo, "deployed shop — ")
	closeInfo(t, u)
	if !d.network("shop_default") {
		t.Error("the stack's default network was not created")
	}

	cursorTo(t, u, "shop_web")
	openStackMenuItem(t, u, 2, risky)
	waitPageText(t, u, pageStackPlan, "swarm ignores these keys:", "privileged", "1 of 1 service(s) flagged", "web", "Docker socket")
	sendRune(t, u, 'd')
	waitPageText(t, u, pageConfirm, "Deploy shop despite")
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, "deployed shop — ")
	closeInfo(t, u)

	// Esc on the plan walks away with nothing applied.
	cursorTo(t, u, "shop_web")
	openStackMenuItem(t, u, 2, clean)
	waitPageText(t, u, pageStackPlan, "Nothing above informational")
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageStackPlan)

	cursorTo(t, u, "shop_web")
	openStackMenuItem(t, u, 2, filepath.Join(dir, "missing.yml"))
	waitPageText(t, u, pageInfo, "cannot read")
	closeInfo(t, u)
}

// A deploy that fails after changing something says what it did change.
func TestStackDeployReportsAPartialApply(t *testing.T) {
	u, d := stackUI(t)
	file := filepath.Join(t.TempDir(), "new.yml")
	writeStackFile(t, file, `services:
  api:
    image: nginx:1.28
    read_only: true
    user: "1000"
networks:
  backend: {}
`)
	d.failCreate.Store(true)
	cursorTo(t, u, "shop_web")
	openStackMenuItem(t, u, 2, file)
	waitPageText(t, u, pageStackPlan, "deploy shop")
	sendRune(t, u, 'd')
	waitPageText(t, u, pageInfo, "deploy failed", "no space left on manager")
	closeInfo(t, u)
}

// A clean comparison says so, with what it did not compare.
func TestStackDiffWithoutDifferences(t *testing.T) {
	u, _ := stackUI(t)
	onLoop(t, u, func() {
		u.showStackDiff("shop", "shop.yml", &stackfile.Diff{Notes: []string{"secret values"}})
	})
	waitPageText(t, u, pageStackDiff, "No differences between shop.yml and the deployed stack.", "not compared: secret values")
}
