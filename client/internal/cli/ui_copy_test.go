// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rivo/tview"
)

// The container menu's two copy entries, driven the way an operator drives
// them: open the menu, fill the form, press Copy. The copy itself is the CLI's
// core (cp.go), so these tests are about the path from the menu to it and back
// — which container was used, which paths were sent, what the operator is told.

// fillCopyForm fills the open copy form and puts the focus on Copy.
func fillCopyForm(t *testing.T, u *ui, fields map[string]string) {
	t.Helper()
	onLoop(t, u, func() {
		box, ok := u.pages.GetPage(pageCopyForm).(*centeredBox)
		if !ok {
			t.Fatalf("the copy form is not on its page")
		}
		form, ok := box.inner.(*tview.Form)
		if !ok {
			t.Fatalf("the copy page holds %T, not a form", box.inner)
		}
		for label, text := range fields {
			item, ok := form.GetFormItemByLabel(label).(*tview.InputField)
			if !ok {
				t.Fatalf("no input field %q in the copy form", label)
			}
			item.SetText(text)
		}
		u.app.SetFocus(form.GetButton(form.GetButtonIndex("Copy")))
	})
	pressEnter(u)
}

func TestContainerMenuCopiesAFileOutOfTheContainer(t *testing.T) {
	u, c := menuUI(t, newCpAgent())
	dst := filepath.Join(t.TempDir(), "out.log")

	pickMenuItem(t, u, c, "copy file out of container")
	waitFront(t, u, pageCopyForm)
	fillCopyForm(t, u, map[string]string{
		"Path in container": "/var/log/app.log",
		"Local destination": dst,
	})

	waitFor(t, "the file to arrive", func() bool {
		b, err := os.ReadFile(dst)
		return err == nil && string(b) == "the log line"
	})
	waitFor(t, "the success flash", func() bool {
		var txt string
		onLoop(t, u, func() { txt = u.help.GetText(true) })
		return strings.Contains(txt, "copied") && strings.Contains(txt, "app.log")
	})
}

func TestContainerMenuCopiesAFileIntoTheContainer(t *testing.T) {
	a := newCpAgent()
	u, c := menuUI(t, a)
	src := filepath.Join(t.TempDir(), "patch.sql")
	if err := os.WriteFile(src, []byte("SELECT 1"), 0o644); err != nil {
		t.Fatal(err)
	}

	pickMenuItem(t, u, c, "copy file into container")
	waitFront(t, u, pageCopyForm)
	fillCopyForm(t, u, map[string]string{
		"Local file or directory":  src,
		"Destination in container": "/tmp",
	})

	waitFor(t, "the archive to reach the agent", func() bool {
		body, dest, _ := a.got()
		return dest == "/tmp" && len(body) > 0
	})
	body, _, _ := a.got()
	h, err := tar.NewReader(bytes.NewReader(body)).Next()
	if err != nil {
		t.Fatalf("read the archive the agent got: %v", err)
	}
	if h.Name != "patch.sql" {
		t.Errorf("entry = %q, want patch.sql (the destination is a directory)", h.Name)
	}
}

// A path the container does not have is a notice, not a flash: the operator
// asked for something that is not there and has to read why.
func TestContainerMenuCopyReportsAMissingPath(t *testing.T) {
	u, c := menuUI(t, newCpAgent())

	pickMenuItem(t, u, c, "copy file out of container")
	waitFront(t, u, pageCopyForm)
	fillCopyForm(t, u, map[string]string{
		"Path in container": "/nope",
		"Local destination": t.TempDir(),
	})

	waitPageText(t, u, pageInfo, "/nope", "no such path")
}

// An empty field is caught in the form: nothing is sent, and the form stays
// open to be corrected.
func TestContainerMenuCopyNeedsBothPaths(t *testing.T) {
	a := newCpAgent()
	u, c := menuUI(t, a)

	pickMenuItem(t, u, c, "copy file into container")
	waitFront(t, u, pageCopyForm)
	fillCopyForm(t, u, map[string]string{"Local file or directory": ""})

	waitFront(t, u, pageInfo)
	if body, _, _ := a.got(); len(body) != 0 {
		t.Errorf("an incomplete form reached the agent: %q", body)
	}
}
