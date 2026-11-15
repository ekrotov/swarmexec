// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// pressRune is press for a typed character.
func pressRune(u *ui, r rune) {
	_, page := u.pages.GetFrontPage()
	page.InputHandler()(rune_(r), func(p tview.Primitive) { u.app.SetFocus(p) })
}

// upperOrBad is a validate: entries are upper-cased, "bad" is refused.
func upperOrBad(s string) (string, error) {
	if s == "bad" {
		return "", errors.New("bad entry")
	}
	return strings.ToUpper(s), nil
}

func listEditUI() *ui {
	u := overlayUI()
	u.km = defaultKeybinds()
	return u
}

// openListEditor opens the editor the way every caller does and returns it.
func openListEditor(t *testing.T, u *ui, cfg editListConfig) *listEditor {
	t.Helper()
	e := u.newListEditor(cfg)
	e.show()
	if name, _ := u.pages.GetFrontPage(); name != pageListEdit {
		t.Fatalf("front page %q, want the list editor", name)
	}
	return e
}

// listRows is what the list shows, colour tags and all.
func listRows(e *listEditor) []string {
	var out []string
	for i := 0; i < e.list.GetItemCount(); i++ {
		main, _ := e.list.GetItemText(i)
		out = append(out, main)
	}
	return out
}

// typeInPrompt answers the open single-line prompt: the wrapping field the
// editors without autocomplete get.
func typeInPrompt(t *testing.T, u *ui, text string) {
	t.Helper()
	if name, _ := u.pages.GetFrontPage(); name != pageListEditPrompt {
		t.Fatalf("front page %q, want the prompt", name)
	}
	ta, ok := u.app.GetFocus().(*tview.TextArea)
	if !ok {
		t.Fatalf("the prompt focuses %T, want a TextArea", u.app.GetFocus())
	}
	ta.SetText(text, true)
	press(u, tcell.KeyEnter)
}

// Add, edit, delete and undo stage locally, and the list marks what is new
// and what is going away.
func TestListEditorStagesAddEditDeleteAndUndo(t *testing.T) {
	u := listEditUI()
	e := openListEditor(t, u, editListConfig{title: "t", items: []string{"A", "B"}, validate: upperOrBad, allowEdit: true})

	pressRune(u, 'a')
	typeInPrompt(t, u, "c")
	if want := []string{"A", "B", "C"}; !slices.Equal(e.cur, want) {
		t.Fatalf("after add: %q, want %q", e.cur, want)
	}
	if rows := listRows(e); !strings.Contains(rows[2], "C (new)") {
		t.Fatalf("an added entry is not marked new: %q", rows)
	}
	if u.app.GetFocus() != e.list {
		t.Fatal("the prompt did not hand focus back to the list")
	}

	e.list.SetCurrentItem(1)
	pressRune(u, 'e')
	if ta := u.app.GetFocus().(*tview.TextArea); ta.GetText() != "B" {
		t.Fatalf("edit starts from %q, want the entry", ta.GetText())
	}
	typeInPrompt(t, u, "b2")
	if want := []string{"A", "B2", "C"}; !slices.Equal(e.cur, want) {
		t.Fatalf("after edit: %q, want %q", e.cur, want)
	}

	e.list.SetCurrentItem(0)
	pressRune(u, 'd')
	if want := []string{"B2", "C"}; !slices.Equal(e.cur, want) {
		t.Fatalf("after delete: %q, want %q", e.cur, want)
	}
	rows := listRows(e)
	if len(rows) != 4 || !strings.Contains(rows[2], "- A (removed)") || !strings.Contains(rows[3], "- B (removed)") {
		t.Fatalf("removed originals are not shown as ghosts: %q", rows)
	}
	// A ghost is not an entry: d and e on it do nothing.
	e.list.SetCurrentItem(3)
	pressRune(u, 'd')
	pressRune(u, 'e')
	if name, _ := u.pages.GetFrontPage(); name != pageListEdit || len(e.cur) != 2 {
		t.Fatalf("a removed ghost was acted on: front %q, cur %q", name, e.cur)
	}

	for _, want := range [][]string{{"A", "B2", "C"}, {"A", "B", "C"}, {"A", "B"}, {"A", "B"}} {
		pressRune(u, 'u')
		if !slices.Equal(e.cur, want) {
			t.Fatalf("after undo: %q, want %q", e.cur, want)
		}
	}
	if e.hasChanges() {
		t.Fatal("undone back to the original, yet it reports changes")
	}
}

// What the prompt refuses stays out, and says why; an empty answer and Esc
// stage nothing.
func TestListEditorPromptRefusals(t *testing.T) {
	u := listEditUI()
	e := openListEditor(t, u, editListConfig{title: "t", items: []string{"A"}, validate: upperOrBad})

	pressRune(u, 'a')
	typeInPrompt(t, u, "bad")
	if len(e.cur) != 1 || !u.pages.HasPage(pageInfo) {
		t.Fatalf("a refused entry was staged (%q) or not explained", e.cur)
	}
	u.pages.RemovePage(pageInfo)

	pressRune(u, 'a')
	typeInPrompt(t, u, "   ")
	pressRune(u, 'a')
	press(u, tcell.KeyEscape)
	if len(e.cur) != 1 || u.pages.HasPage(pageListEditPrompt) {
		t.Fatalf("empty / cancelled prompts: cur %q, prompt still up %v", e.cur, u.pages.HasPage(pageListEditPrompt))
	}
	// Without allowEdit, e is not an editor key.
	pressRune(u, 'e')
	if u.pages.HasPage(pageListEditPrompt) {
		t.Fatal("e opened an edit prompt on an editor that does not allow editing")
	}
}

// The prompt takes the shape the config asks for.
func TestListEditorPromptShapes(t *testing.T) {
	t.Run("multiline saves on Ctrl-S", func(t *testing.T) {
		u := listEditUI()
		e := openListEditor(t, u, editListConfig{title: "t", validate: upperOrBad, multiline: true})
		pressRune(u, 'a')
		ta := u.app.GetFocus().(*tview.TextArea)
		ta.SetText("x\ny", false)
		press(u, tcell.KeyCtrlS)
		if !slices.Equal(e.cur, []string{"X\nY"}) {
			t.Fatalf("cur %q", e.cur)
		}
	})
	t.Run("suggest gets an input field", func(t *testing.T) {
		u := listEditUI()
		e := openListEditor(t, u, editListConfig{title: "t", validate: upperOrBad,
			suggest: func(string) []string { return nil }})
		pressRune(u, 'a')
		in, ok := u.app.GetFocus().(*tview.InputField)
		if !ok {
			t.Fatalf("focus %T, want an InputField", u.app.GetFocus())
		}
		in.SetText("net1")
		press(u, tcell.KeyEnter)
		if !slices.Equal(e.cur, []string{"NET1"}) {
			t.Fatalf("cur %q", e.cur)
		}
	})
	t.Run("a form submits through validate", func(t *testing.T) {
		u := listEditUI()
		var submit func(string) error
		var initial string
		form := tview.NewBox()
		e := openListEditor(t, u, editListConfig{title: "t", items: []string{"A"}, validate: upperOrBad, allowEdit: true,
			formPrompt: func(init string, s func(string) error, _ func()) tview.Primitive {
				initial, submit = init, s
				return form
			}})
		pressRune(u, 'e')
		if initial != "A" || u.app.GetFocus() != form {
			t.Fatalf("form opened with %q, focus %T", initial, u.app.GetFocus())
		}
		if err := submit("bad"); err == nil || !u.pages.HasPage(pageListEditPrompt) {
			t.Fatal("a refused form entry must return the error and keep the form up")
		}
		if err := submit(" z "); err != nil || u.pages.HasPage(pageListEditPrompt) {
			t.Fatalf("an accepted form entry must close the form (err %v)", err)
		}
		if !slices.Equal(e.cur, []string{"Z"}) {
			t.Fatalf("cur %q", e.cur)
		}
	})
}

// The footer names exactly the keys this editor has.
func TestListEditorFooterKeys(t *testing.T) {
	u := listEditUI()
	openListEditor(t, u, editListConfig{title: "t", validate: upperOrBad})
	if f := u.help.GetText(true); strings.Contains(f, "edit") || !strings.Contains(f, "apply") {
		t.Fatalf("footer without allowEdit: %q", f)
	}
	u = listEditUI()
	openListEditor(t, u, editListConfig{title: "t", validate: upperOrBad, allowEdit: true,
		entryAction: &listEntryAction{key: 'x', label: "aliases"}})
	if f := u.help.GetText(true); !strings.Contains(f, "edit") || !strings.Contains(f, "aliases") {
		t.Fatalf("footer with edit and an entry action: %q", f)
	}
}

// The entry action closes the editor and hands over the entry under the
// cursor; on a ghost row it does nothing.
func TestListEditorEntryAction(t *testing.T) {
	u := listEditUI()
	var got []string
	e := openListEditor(t, u, editListConfig{title: "t", items: []string{"A", "B"}, validate: upperOrBad,
		entryAction: &listEntryAction{key: 'x', label: "act", run: func(s string) { got = append(got, s) }}})
	pressRune(u, 'd') // A becomes a ghost at row 1
	e.list.SetCurrentItem(1)
	pressRune(u, 'x')
	if got != nil {
		t.Fatalf("the action ran on a ghost: %q", got)
	}
	e.list.SetCurrentItem(0)
	pressRune(u, 'x')
	if !slices.Equal(got, []string{"B"}) || u.pages.HasPage(pageListEdit) {
		t.Fatalf("action got %q, editor still up %v", got, u.pages.HasPage(pageListEdit))
	}
}

// Esc leaves at once when nothing is staged. With staged edits it asks;
// "Keep editing" stays, Esc on the question discards and leaves (it must not
// trap the operator), and "Apply" goes on to the confirmation.
func TestListEditorLeaveGuard(t *testing.T) {
	cfg := editListConfig{title: "t", items: []string{"A"}, validate: upperOrBad, applyVerb: "Update it"}

	u := listEditUI()
	openListEditor(t, u, cfg)
	press(u, tcell.KeyEscape)
	if name, _ := u.pages.GetFrontPage(); name != pageMain {
		t.Fatalf("a clean editor did not close on Esc (front %q)", name)
	}

	u = listEditUI()
	e := openListEditor(t, u, cfg)
	pressRune(u, 'd')
	press(u, tcell.KeyEscape)
	if name, _ := u.pages.GetFrontPage(); name != pageListEditLeave {
		t.Fatalf("staged edits did not raise the guard (front %q)", name)
	}
	press(u, tcell.KeyTab) // Apply → Discard
	press(u, tcell.KeyTab) // → Keep editing
	press(u, tcell.KeyEnter)
	if name, _ := u.pages.GetFrontPage(); name != pageListEdit || u.app.GetFocus() != e.list {
		t.Fatalf("Keep editing: front %q, focus %T", name, u.app.GetFocus())
	}

	press(u, tcell.KeyEscape)
	press(u, tcell.KeyEscape) // on the question: discard and leave
	if name, _ := u.pages.GetFrontPage(); name != pageMain {
		t.Fatalf("Esc on the guard did not leave (front %q)", name)
	}

	u = listEditUI()
	openListEditor(t, u, cfg)
	pressRune(u, 'd')
	press(u, tcell.KeyEscape)
	press(u, tcell.KeyEnter) // Apply
	if name, _ := u.pages.GetFrontPage(); name != pageConfirm {
		t.Fatalf("Apply on the guard did not ask to confirm (front %q)", name)
	}
}

// w confirms, then applies the staged list off the UI goroutine; success
// closes the editor and runs after, a failure keeps it open to try again.
func TestListEditorApply(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "succeeds"
		if fail {
			name = "fails"
		}
		t.Run(name, func(t *testing.T) {
			u := runningUI(t)
			u.km = defaultKeybinds()
			applied := make(chan []string, 1)
			afterRan := make(chan struct{}, 1)
			var note []string
			cfg := editListConfig{title: "t", items: []string{"A", "B"}, validate: upperOrBad, applyVerb: "Update it",
				confirmNote: func(cur []string) string { note = slices.Clone(cur); return "careful" },
				onApply: func(_ context.Context, cur []string) error {
					applied <- cur
					if fail {
						return errors.New("boom")
					}
					return nil
				},
				after: func() { afterRan <- struct{}{} }}
			var e *listEditor
			onLoop(t, u, func() {
				e = u.newListEditor(cfg)
				e.show()
				pressRune(u, 'd')
				pressRune(u, 'w')
			})
			if !pageOpen(t, u, pageConfirm) {
				t.Fatal("w did not ask to confirm")
			}
			if !slices.Equal(note, []string{"B"}) {
				t.Fatalf("confirmNote saw %q", note)
			}
			onLoop(t, u, func() { press(u, tcell.KeyEnter) })
			select {
			case got := <-applied:
				if !slices.Equal(got, []string{"B"}) {
					t.Fatalf("applied %q", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the confirmed apply never ran")
			}
			deadline := time.Now().Add(5 * time.Second)
			for !pageOpen(t, u, pageInfo) {
				if time.Now().After(deadline) {
					t.Fatal("no notice after the apply")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if open := pageOpen(t, u, pageListEdit); open != fail {
				t.Fatalf("editor open after the apply: %v (failed: %v)", open, fail)
			}
			select {
			case <-afterRan:
				if fail {
					t.Fatal("after ran on a failed apply")
				}
			default:
				if !fail {
					t.Fatal("after did not run on success")
				}
			}
		})
	}
}
