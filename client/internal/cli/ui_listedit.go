// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// editList is a staged list editor: it shows the current entries and lets the
// operator add/edit/delete locally, then apply them all at once (one
// ServiceUpdate). validate normalizes/validates a single entry; onApply gets
// the final list; after runs on success. Used for a service's ports, labels,
// networks and aliases, secrets, env, placement and mounts.
// formPrompt (optional) replaces the default single-line add/edit prompt with
// a custom form: it receives the current entry (empty when adding), a submit
// callback that validates+stages+closes on success (returning an error to show
// otherwise), and a cancel callback; it returns the primitive to display.
func (u *ui) editList(cfg editListConfig) {
	u.newListEditor(cfg).show()
}

// listEditor is editList's overlay: the list, the staged entries and the undo
// history.
//
// It was one 280-line function whose render, prompt, undo, leave guard and
// apply were closures over the same locals (review Q-01). As a type the parts
// are separate methods — build (newListEditor, show), view (render), input
// (keys, prompt) and apply (apply, leave) — as in scheduleEditor and
// inspectView.
type listEditor struct {
	u   *ui
	cfg editListConfig
	cur []string // the staged entries; cfg.items is what the service has
	// history stacks snapshots of cur before each staged change, so `u` undoes
	// one step (add/edit/delete) as long as nothing has been applied yet.
	history  [][]string
	keyPairs []string // the footer's key / description pairs

	list *tview.List
	ov   *overlay
}

// newListEditor builds the editor and renders its entries; show puts it on
// screen.
func (u *ui) newListEditor(cfg editListConfig) *listEditor {
	e := &listEditor{u: u, cfg: cfg, cur: append([]string{}, cfg.items...)}
	if cfg.staged != nil {
		e.cur = append([]string{}, cfg.staged...)
	}
	e.list = tview.NewList().ShowSecondaryText(false)
	e.list.SetBorder(true).SetTitle(fmt.Sprintf(" %s ", cfg.title))
	e.keyPairs = []string{"a", "add"}
	if cfg.allowEdit {
		e.keyPairs = append(e.keyPairs, "e", "edit")
	}
	e.keyPairs = append(e.keyPairs, "d", "delete", keyLabel(u.km.Copy), "copy")
	if cfg.entryAction != nil {
		e.keyPairs = append(e.keyPairs, string(cfg.entryAction.key), cfg.entryAction.label)
	}
	e.keyPairs = append(e.keyPairs, "u", "undo", "w", "apply", "j/k", "move", "Esc", "cancel")
	e.render()
	return e
}

func (e *listEditor) show() {
	e.ov = e.u.overlayFor(pageListEdit, e.cfg.back, footerKeys(e.keyPairs...))
	e.list.SetInputCapture(e.keys)
	e.ov.show(centered(e.list, 110, 18), e.list)
}

// render repaints the entries, keeping the cursor row.
func (e *listEditor) render() {
	list, items := e.list, e.cfg.items
	idx := list.GetCurrentItem()
	list.Clear()
	if len(e.cur) == 0 && len(items) == 0 {
		list.AddItem("(none)", "", 0, nil)
	} else {
		// Highlight staged (not-yet-applied) changes: an entry not present in
		// the original set is added/edited (green); originals missing from cur
		// are shown as dim-red "removed" ghosts. Diff is a multiset so it
		// survives undo without any parallel bookkeeping.
		origCount := map[string]int{}
		for _, it := range items {
			origCount[it]++
		}
		for _, it := range e.cur {
			label := tview.Escape(it)
			if origCount[it] > 0 {
				origCount[it]-- // unchanged — matches an original
			} else {
				label = "[green::b]" + label + " (new)[-:-:-]"
			}
			list.AddItem(label, "", 0, nil)
		}
		// origCount now holds the leftovers = removed items; show them (in the
		// original order) after the live rows. They sit beyond len(cur), so
		// selected treats them as inert.
		shown := map[string]int{}
		for _, it := range items {
			if shown[it] < origCount[it] {
				shown[it]++
				list.AddItem("[red::d]- "+tview.Escape(it)+" (removed)[-:-:-]", "", 0, nil)
			}
		}
	}
	if idx >= 0 && idx < list.GetItemCount() {
		list.SetCurrentItem(idx)
	}
}

// selected is the index of the staged entry under the cursor; ok is false on
// the "(none)" row and on removed ghosts.
func (e *listEditor) selected() (i int, ok bool) {
	i = e.list.GetCurrentItem()
	return i, i >= 0 && i < len(e.cur)
}

func (e *listEditor) snapshot() { e.history = append(e.history, append([]string{}, e.cur...)) }

func (e *listEditor) undo() {
	if len(e.history) == 0 {
		return
	}
	e.cur = e.history[len(e.history)-1]
	e.history = e.history[:len(e.history)-1]
	e.render()
}

// hasChanges reports whether anything is staged but not yet applied (cur
// differs from the original set — order included; j/k only navigate here).
func (e *listEditor) hasChanges() bool {
	return !slices.Equal(e.cur, e.cfg.items)
}

// keys is the list's input capture.
func (e *listEditor) keys(ev *tcell.EventKey) *tcell.EventKey {
	if ev.Key() == tcell.KeyEscape {
		e.leave()
		return nil
	}
	if ev.Key() != tcell.KeyRune {
		return vimListKeys(ev)
	}
	act := e.cfg.entryAction
	switch r := ev.Rune(); {
	case r == 'a':
		e.prompt("add: ", "", func(n string) { e.snapshot(); e.cur = append(e.cur, n) })
	case r == 'e' && e.cfg.allowEdit:
		e.editSelected()
	case r == 'd':
		e.deleteSelected()
	case r == e.u.km.Copy:
		e.copySelected()
	case act != nil && r == act.key:
		e.runEntryAction(act)
	case r == 'u':
		e.undo()
	case r == 'w':
		e.apply()
	default:
		return vimListKeys(ev)
	}
	return nil
}

// editSelected prompts for a new value of the entry under the cursor.
func (e *listEditor) editSelected() {
	if i, ok := e.selected(); ok {
		e.prompt("edit: ", e.cur[i], func(n string) { e.snapshot(); e.cur[i] = n })
	}
}

// deleteSelected stages removing the entry under the cursor.
func (e *listEditor) deleteSelected() {
	if i, ok := e.selected(); ok {
		e.snapshot()
		e.cur = append(e.cur[:i], e.cur[i+1:]...)
		e.render()
	}
}

// copySelected puts the entry under the cursor on the clipboard.
func (e *listEditor) copySelected() {
	if i, ok := e.selected(); ok && e.u.screen != nil {
		e.u.screen.SetClipboard([]byte(e.cur[i]))
		e.ov.setHelp(" [green]✓ copied[white] · " + footerKeys(e.keyPairs...))
	}
}

// runEntryAction closes the editor and runs act on the entry under the cursor.
func (e *listEditor) runEntryAction(act *listEntryAction) {
	if i, ok := e.selected(); ok {
		entry := e.cur[i]
		e.ov.Close()
		act.run(entry)
	}
}

// leave closes the editor on Esc — unless something is staged, which it
// does not drop silently: the operator chooses to apply, discard or stay.
func (e *listEditor) leave() {
	if !e.hasChanges() {
		e.ov.Close()
		return
	}
	lov := e.u.overlayFor(pageListEditLeave, nil, "")
	m := tview.NewModal().
		SetText("You have unapplied changes.\n\nApply them now, discard them, or keep editing?\n(Esc discards and leaves.)").
		AddButtons([]string{"Apply", "Discard", "Keep editing"}).
		SetDoneFunc(func(_ int, lbl string) {
			lov.Close()
			switch lbl {
			case "Apply":
				e.apply()
			case "Keep editing":
				e.u.app.SetFocus(e.list)
			default:
				// "Discard" button or Esc (empty label): leave the editor,
				// dropping the staged (never-applied) edits. Esc must be an
				// exit here — mapping it to "keep editing" traps the operator,
				// since Esc on the list only re-opens this same guard, forever.
				e.ov.Close()
			}
		})
	lov.show(newScrim(m), m)
}

// apply runs the rolling-update confirmation and, on Apply, commits cur.
// Shared by `w` and the "unapplied changes" reminder shown when leaving.
func (e *listEditor) apply() {
	u := e.u
	text := fmt.Sprintf("%s?\n\nThis triggers a rolling update of the service.", e.cfg.applyVerb)
	if e.cfg.confirmNote != nil {
		if note := e.cfg.confirmNote(e.cur); note != "" {
			text = note + "\n\n" + text
		}
	}
	u.confirm(text, "Apply", e.list, func() {
		staged := slices.Clone(e.cur) // cur keeps changing on the UI goroutine; work gets its own
		onApply, after := e.cfg.onApply, e.cfg.after
		asyncDo(u, "ui.serviceedit.apply", func(ctx context.Context) error {
			return onApply(ctx, staged)
		}, func(err error) {
			if err != nil {
				u.info("update failed: " + err.Error())
				u.app.SetFocus(e.list)
				return
			}
			e.ov.Close()
			u.info("service updated — rolling update started")
			if after != nil {
				after()
			}
		})
	})
}

// commit validates a typed entry and stages it via done.
func (e *listEditor) commit(raw string, done func(string)) {
	txt := strings.TrimSpace(raw)
	if txt == "" {
		return
	}
	norm, err := e.cfg.validate(txt)
	if err != nil {
		e.u.info(err.Error())
		return
	}
	done(norm)
	e.render()
}

// prompt asks for one entry — the caller's form, a multi-line area, a
// wrapping field or an autocompleting one, in that order of preference — and
// stages it via done. One prompt overlay per invocation, whichever shape it
// takes; all of them go back to the list.
func (e *listEditor) prompt(label, initial string, done func(string)) {
	pov := e.u.overlayFor(pageListEditPrompt, e.list, "")
	switch {
	case e.cfg.formPrompt != nil:
		e.promptForm(pov, initial, done)
	case e.cfg.multiline:
		e.promptMultiline(pov, label, initial, done)
	case e.cfg.suggest == nil:
		e.promptWrapped(pov, label, initial, done)
	default:
		e.promptSuggest(pov, label, initial, done)
	}
}

// promptForm shows the caller's form. Its submit validates+stages+closes on
// success; the form keeps focus on failure (it surfaces the error itself).
func (e *listEditor) promptForm(pov *overlay, initial string, done func(string)) {
	submit := func(raw string) error {
		norm, err := e.cfg.validate(strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		done(norm)
		e.render()
		pov.Close()
		return nil
	}
	form := e.cfg.formPrompt(initial, submit, pov.Close)
	h := e.cfg.formHeight
	if h == 0 {
		h = 19
	}
	pov.show(centered(form, 96, h), form)
}

// promptMultiline edits long / multi-line values (e.g. GITLAB_OMNIBUS_CONFIG),
// which are painful in a one-line field, in a scrollable TextArea. The cursor
// goes to the START (not the end): cursor-at-end scrolls a freshly-built,
// not-yet-sized TextArea to its bottom, so the operator only sees the tail of
// the value with the cursor pinned to the last row. Start-of-text shows the
// value from the top.
func (e *listEditor) promptMultiline(pov *overlay, label, initial string, done func(string)) {
	ta := tview.NewTextArea().SetText(initial, false)
	ta.SetBorder(true).SetTitle(" " + label + " — Ctrl-S save · Esc cancel ")
	ta.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEscape:
			pov.Close()
			return nil
		case tcell.KeyCtrlS:
			txt := ta.GetText()
			pov.Close()
			e.commit(txt, done)
			return nil
		}
		return ev
	})
	pov.show(centered(ta, 96, 22), ta)
}

// promptWrapped is for editors with no autocomplete to keep: a wide field
// that wraps, so a long label value or port spec stays readable as a whole.
func (e *listEditor) promptWrapped(pov *overlay, label, initial string, done func(string)) {
	ta := wrapField(label, initial, "", 3)
	ta.SetBorder(true).SetTitle(" Enter save · Esc cancel ")
	ta.SetInputCapture(func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyEscape:
			pov.Close()
			return nil
		case tcell.KeyEnter:
			txt := fieldText(ta)
			pov.Close()
			e.commit(txt, done)
			return nil
		}
		return ev
	})
	pov.show(centered(ta, 96, 5), ta)
}

// promptSuggest is a one-line field with the editor's autocomplete.
func (e *listEditor) promptSuggest(pov *overlay, label, initial string, done func(string)) {
	in := tview.NewInputField().SetLabel(label).SetText(initial).SetFieldWidth(0)
	in.SetAutocompleteFunc(e.cfg.suggest)
	in.SetDoneFunc(func(k tcell.Key) {
		pov.Close()
		if k != tcell.KeyEnter {
			return
		}
		e.commit(in.GetText(), done)
	})
	in.SetBorder(true)
	pov.show(centeredPrompt(in, 96), in)
}
