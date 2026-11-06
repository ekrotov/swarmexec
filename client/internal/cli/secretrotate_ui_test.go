// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rivo/tview"
)

// rotateHarness opens the rotate editor with a fake plan; confirm records the
// question and agrees, start records the bytes.
type rotateHarness struct {
	t       *testing.T
	u       *ui
	d       *rotateDialog
	queued  chan func()
	plan    rotatePlan
	planErr error
	asked   []string
	started [][]byte
}

func openRotateHarness(t *testing.T, labels map[string]string) *rotateHarness {
	t.Helper()
	h := &rotateHarness{t: t, u: overlayUI(), queued: make(chan func(), 4)}
	h.plan = rotatePlan{Name: "db", Temp: "db_rotating", Labels: labels, Stack: labels[stackLabelKey], StartStep: 1,
		Users: []rotateUser{{Name: "web", Targets: []string{"db_password"}}, {Name: "worker", Targets: []string{"/etc/db"}}}}
	h.d = h.u.openRotateEditor(tview.NewBox(), "db", labels, rotateHooks{
		plan:    func() (rotatePlan, error) { return h.plan, h.planErr },
		queue:   func(fn func()) { h.queued <- fn },
		confirm: func(msg string, _ tview.Primitive, ok func()) { h.asked = append(h.asked, msg); ok() },
		start:   func(data []byte) { h.started = append(h.started, data) },
	})
	return h
}

func (h *rotateHarness) keys(s string) {
	h.t.Helper()
	_, page := h.u.pages.GetFrontPage()
	for _, ev := range parseKeys(h.t, s) {
		page.InputHandler()(ev, func(p tview.Primitive) { h.u.app.SetFocus(p) })
	}
}

func (h *rotateHarness) settle() {
	h.t.Helper()
	select {
	case fn := <-h.queued:
		fn()
	case <-time.After(5 * time.Second):
		h.t.Fatal("the plan never reported back")
	}
}

func TestRotateEditor_NameAndLabelsAreShownNotEdited(t *testing.T) {
	h := openRotateHarness(t, map[string]string{stackLabelKey: "shop", "team": "core"})
	if h.u.app.GetFocus() != h.d.ed.ta {
		t.Fatal("focus must start in the value editor — there is nothing else to edit")
	}
	head := h.d.head.GetText(true)
	if !strings.Contains(head, "db") || !strings.Contains(head, "com.docker.stack.namespace=shop,team=core") {
		t.Errorf("head = %q", head)
	}
	if title := h.d.root.GetTitle(); !strings.Contains(title, "rotate secret db") || !strings.Contains(title, "? help") {
		t.Errorf("title = %q", title)
	}
	h.keys("<Tab>")
	if h.u.app.GetFocus() != h.d.ed.ta {
		t.Error("Tab must not leave the editor: there are no fields")
	}
}

func TestRotateEditor_EmptyValueRefused(t *testing.T) {
	h := openRotateHarness(t, nil)
	h.keys(":w<CR>")
	if !h.d.ed.msgErr || !strings.Contains(h.d.ed.msg, "value is empty") {
		t.Fatalf("status = %q", h.d.ed.msg)
	}
	if len(h.asked)+len(h.started) != 0 || !h.u.overlayOpen() {
		t.Error("an empty value must not get as far as the confirm")
	}
}

func TestRotateEditor_ConfirmThenStartWithExactBytes(t *testing.T) {
	h := openRotateHarness(t, map[string]string{stackLabelKey: "shop"})
	content := "line1\r\n\tline2\n"
	p := filepath.Join(t.TempDir(), "v.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	h.keys(":r " + p + "<CR>:w<CR>")
	h.settle()
	if len(h.asked) != 1 {
		t.Fatalf("asked %d times", len(h.asked))
	}
	q := h.asked[0]
	for _, want := range []string{
		"web → /run/secrets/db_password", "worker → /etc/db",
		"restarts twice", "No rollback", `stack "shop"`, "file:",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("confirm lacks %q:\n%s", want, q)
		}
	}
	if len(h.started) != 1 || string(h.started[0]) != content {
		t.Fatalf("started with %q, want %q", h.started, content)
	}
	if h.u.overlayOpen() {
		t.Error("the editor closes once the rotation starts")
	}
}

func TestRotateEditor_PlanErrorStaysOpen(t *testing.T) {
	h := openRotateHarness(t, nil)
	h.planErr = errors.New(`a secret "db_rotating" exists that swarmexec did not create`)
	h.keys("inew<Esc>:w<CR>")
	h.settle()
	if !h.u.overlayOpen() || !h.d.ed.msgErr || !strings.Contains(h.d.ed.msg, "did not create") {
		t.Fatalf("open=%v status=%q", h.u.overlayOpen(), h.d.ed.msg)
	}
	if len(h.started) != 0 || h.d.ed.ta.GetText() != "new" {
		t.Error("a refused plan keeps the text and starts nothing")
	}
}

func TestRotateConfirmText(t *testing.T) {
	q := rotateConfirmText(rotatePlan{Name: "lonely", Temp: "lonely_rotating"})
	if !strings.Contains(q, "No service uses lonely") || strings.Contains(q, "restarts twice") || strings.Contains(q, "Hint") {
		t.Errorf("unused, unstacked:\n%s", q)
	}
	q = rotateConfirmText(rotatePlan{Name: "db", Temp: "db_rotating", Resume: true, StartStep: 5,
		Users: []rotateUser{{Name: "web", Targets: []string{"db"}, OnTemp: true}}})
	for _, want := range []string{"Resuming", "step 5/6", "value given now is not used", "(on db_rotating — restarts once more)"} {
		if !strings.Contains(q, want) {
			t.Errorf("resume text lacks %q:\n%s", want, q)
		}
	}
}

func TestReadRotateValue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(p, []byte("a\x00b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readRotateValue(&secretRotateFlags{fromFile: p}, nil)
	if err != nil || string(got) != "a\x00b\n" {
		t.Fatalf("file: %q %v (bytes must pass unchanged, binary too)", got, err)
	}
	got, err = readRotateValue(&secretRotateFlags{fromStdin: true}, strings.NewReader("s3cret"))
	if err != nil || string(got) != "s3cret" {
		t.Fatalf("stdin: %q %v", got, err)
	}
	if _, err := readRotateValue(&secretRotateFlags{}, nil); err == nil {
		t.Error("no source accepted")
	}
	if _, err := readRotateValue(&secretRotateFlags{fromFile: p, fromStdin: true}, nil); err == nil {
		t.Error("two sources accepted")
	}
	if _, err := readRotateValue(&secretRotateFlags{fromFile: t.TempDir()}, nil); err == nil {
		t.Error("a directory accepted")
	}
}
