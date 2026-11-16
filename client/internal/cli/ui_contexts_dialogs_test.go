// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"swarmexec/client/internal/dockerctx"
	"swarmexec/client/internal/swarmfake"
)

// The context sidebar and its dialogs against a Docker config directory of
// the test's own: nothing reads or writes ~/.docker, and nothing dials ssh —
// the SSH form only writes the context, and the connection test runs against
// a fake daemon over tcp.

// isolatedDockerConfig points the Docker CLI config at a fresh directory.
func isolatedDockerConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "")
	return dir
}

// contextsUI is covChromeUI with the sidebar table as its screen, on "default".
func contextsUI(t *testing.T) *ui {
	t.Helper()
	u := covChromeUI(t)
	onLoop(t, u, func() {
		u.cxtable = tview.NewTable().SetBorders(false).SetSelectable(true, false)
		u.cxtable.SetInputCapture(u.contextKeys)
		u.pages.AddPage(pageMain, u.cxtable, true, true)
		u.app.SetFocus(u.cxtable)
		u.activeCtx = "default"
		u.loadContexts()
	})
	return u
}

func contextRows(t *testing.T, u *ui) string {
	t.Helper()
	var rows []string
	onLoop(t, u, func() {
		for r := 0; r < u.cxtable.GetRowCount(); r++ {
			rows = append(rows, u.cxtable.GetCell(r, 0).Text)
		}
	})
	return strings.Join(rows, "|")
}

func selectContext(t *testing.T, u *ui, name string) {
	t.Helper()
	onLoop(t, u, func() {
		for i, c := range u.ctxs {
			if c.Name == name {
				u.cxtable.Select(i, 0)
			}
		}
		u.app.SetFocus(u.cxtable)
	})
}

// The list marks the active context, a connected one and a refused one, and
// the detail overlay shows the host.
func TestContextSidebarListAndDetail(t *testing.T) {
	isolatedDockerConfig(t)
	for _, name := range []string{"prod", "staging"} {
		if err := dockerctx.Create(dockerctx.CreateOptions{Name: name, Host: "tcp://" + name + ".example:2376"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := dockerctx.Use("staging"); err != nil {
		t.Fatal(err)
	}
	u := contextsUI(t)
	onLoop(t, u, func() {
		down := newClusterState("prod", "prod")
		down.err = errRefused
		u.clusters["prod"] = down
		up := newClusterState("staging", "staging")
		up.dcli = &fakeManager{}
		u.clusters["staging"] = up
		u.renderContexts()
	})
	if got := contextRows(t, u); got != "▶ default|✗ prod|· staging" {
		t.Errorf("rows %q", got)
	}

	selectContext(t, u, "staging")
	sendRune(t, u, 'i')
	waitPageText(t, u, pageCtxDetail, "context staging", "docker host: tcp://staging.example:2376", "active: no", "stored current docker context")
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageCtxDetail)

	// Re-render keeps the cursor on the same context.
	onLoop(t, u, func() { u.renderContexts() })
	var sel string
	onLoop(t, u, func() {
		if c, ok := u.selectedContext(); ok {
			sel = c.Name
		}
	})
	if sel != "staging" {
		t.Errorf("cursor on %q after re-render", sel)
	}

	// "use" on the active context only says so.
	selectContext(t, u, "default")
	sendRune(t, u, 'i')
	waitPageText(t, u, pageCtxDetail, "active: yes (this session)")
	sendRune(t, u, u.km.CtxUse)
	waitPageGone(t, u, pageCtxDetail)
	waitFooter(t, u, "already on context default")
	sendRune(t, u, u.km.CtxUse)

	// A switch already under way refuses a second one.
	onLoop(t, u, func() { u.switching = true })
	selectContext(t, u, "prod")
	sendRune(t, u, u.km.CtxUse)
	waitFooter(t, u, "one switch at a time")
	onLoop(t, u, func() { u.switching = false })

	// Esc with the sidebar not focused is a no-op; other keys go to the tabs.
	sendSpecial(t, u, tcell.KeyEscape)
	sendRune(t, u, '?')
}

// Remove: "default" is protected; the current context asks with a warning
// and resets the selection.
func TestContextSidebarDelete(t *testing.T) {
	dir := isolatedDockerConfig(t)
	if err := dockerctx.Create(dockerctx.CreateOptions{Name: "prod", Host: "tcp://prod.example:2376"}); err != nil {
		t.Fatal(err)
	}
	if err := dockerctx.Use("prod"); err != nil {
		t.Fatal(err)
	}
	u := contextsUI(t)

	selectContext(t, u, "default")
	sendRune(t, u, u.km.CtxDelete)
	waitPageText(t, u, pageInfo, `the built-in "default" context cannot be removed`)
	closeInfo(t, u)

	selectContext(t, u, "prod")
	sendRune(t, u, u.km.CtxDelete)
	waitPageText(t, u, pageConfirm, `Remove context "prod"?`, "tcp://prod.example:2376", "It is the current context")
	sendSpecial(t, u, tcell.KeyEnter)
	waitFooter(t, u, "removed context prod")
	if got := contextRows(t, u); got != "▶ default" {
		t.Errorf("rows after remove %q", got)
	}
	if dockerctx.Current() != "default" {
		t.Errorf("current context %q, want default", dockerctx.Current())
	}

	// The store vanished under us: the remove fails and says so.
	if err := dockerctx.Create(dockerctx.CreateOptions{Name: "gone", Host: "tcp://gone.example:2376"}); err != nil {
		t.Fatal(err)
	}
	onLoop(t, u, func() { u.loadContexts() })
	selectContext(t, u, "gone")
	sendRune(t, u, u.km.CtxDelete)
	waitPageText(t, u, pageConfirm, `Remove context "gone"?`)
	if err := os.RemoveAll(filepath.Join(dir, "contexts")); err != nil {
		t.Fatal(err)
	}
	sendSpecial(t, u, tcell.KeyEnter)
	waitPageText(t, u, pageInfo, "remove failed")
	closeInfo(t, u)
}

// ctxForm is the open new-context form.
func ctxForm(t *testing.T, u *ui) *tview.Form {
	t.Helper()
	var form *tview.Form
	onLoop(t, u, func() {
		if box, ok := u.pages.GetPage(pageCtxForm).(*centeredBox); ok {
			form, _ = box.inner.(*tview.Form)
		}
	})
	if form == nil {
		t.Fatal("no context form open")
	}
	return form
}

// toggleCheckbox focuses the labelled checkbox and presses space on it.
func toggleCheckbox(t *testing.T, u *ui, label string) {
	t.Helper()
	form := ctxForm(t, u)
	onLoop(t, u, func() {
		cb, ok := form.GetFormItemByLabel(label).(*tview.Checkbox)
		if !ok {
			t.Errorf("no checkbox %q", label)
			return
		}
		u.app.SetFocus(cb)
	})
	sendRune(t, u, ' ')
}

// The guided form: an SSH context through a jump host is written as such; a
// plain tcp one is tested against a daemon first; the errors are reported.
func TestNewContextForm(t *testing.T) {
	isolatedDockerConfig(t)
	u := contextsUI(t)

	sendRune(t, u, u.km.CtxNew)
	waitPageText(t, u, pageCtxForm, "new context", "SSH user", "SSH host", "Use a jump host")
	toggleCheckbox(t, u, "Use a jump host (bastion)")
	waitPageText(t, u, pageCtxForm, "Jump host(s)")
	covSetField(t, u, pageCtxForm, "Name", "edge")
	covSetField(t, u, pageCtxForm, "SSH user", "deploy")
	covSetField(t, u, pageCtxForm, "SSH host", "manager1")
	covSetField(t, u, pageCtxForm, "SSH port (22)", "2222")
	covSetField(t, u, pageCtxForm, "Jump host(s)", "bastion")
	covSetField(t, u, pageCtxForm, "Description", "edge cluster")
	pressFormButton(t, u, pageCtxForm, "Create")
	waitPageGone(t, u, pageCtxForm)
	waitFooter(t, u, "created context edge")
	if got := contextRows(t, u); !strings.Contains(got, "edge") {
		t.Errorf("rows %q lack the new context", got)
	}
	list, _ := dockerctx.List()
	var host string
	for _, c := range list {
		if c.Name == "edge" {
			host = c.Host
		}
	}
	if host != "ssh://deploy@manager1:2222" {
		t.Errorf("edge host %q", host)
	}
	if pj := dockerctx.ResolveProxyJump("edge"); pj != "bastion" {
		t.Errorf("edge proxy jump %q", pj)
	}

	// Plain Docker host: required, tested, and a taken name refused.
	api := httptest.NewServer(&swarmfake.Swarm{})
	t.Cleanup(api.Close)
	sendRune(t, u, u.km.CtxNew)
	waitPageText(t, u, pageCtxForm, "new context")
	toggleCheckbox(t, u, "Use a jump host (bastion)") // on, then SSH off hides it
	toggleCheckbox(t, u, "Connect to Docker over SSH")
	waitPageText(t, u, pageCtxForm, "Docker host")
	pressFormButton(t, u, pageCtxForm, "Test")
	waitPageText(t, u, pageInfo, "docker host is required")
	closeInfo(t, u)
	pressFormButton(t, u, pageCtxForm, "Create")
	waitPageText(t, u, pageInfo, "docker host is required")
	closeInfo(t, u)

	covSetField(t, u, pageCtxForm, "Docker host", "tcp://"+strings.TrimPrefix(api.URL, "http://"))
	pressFormButton(t, u, pageCtxForm, "Test")
	waitFooter(t, u, "connection ok")

	covSetField(t, u, pageCtxForm, "Docker host", "tcp://127.0.0.1:1")
	pressFormButton(t, u, pageCtxForm, "Test")
	waitPageText(t, u, pageInfo, "test failed")
	closeInfo(t, u)

	covSetField(t, u, pageCtxForm, "Name", "edge")
	pressFormButton(t, u, pageCtxForm, "Create")
	waitPageText(t, u, pageInfo, "create failed", "already exists")
	closeInfo(t, u)

	// Back to SSH: the SSH fields return; Esc closes, as does Cancel.
	toggleCheckbox(t, u, "Connect to Docker over SSH")
	waitPageText(t, u, pageCtxForm, "SSH host")
	form := ctxForm(t, u)
	onLoop(t, u, func() { u.app.SetFocus(form) })
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageCtxForm)
	sendRune(t, u, u.km.CtxNew)
	waitPageText(t, u, pageCtxForm, "new context")
	pressFormButton(t, u, pageCtxForm, "Cancel")
	waitPageGone(t, u, pageCtxForm)
}
