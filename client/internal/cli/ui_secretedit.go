// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// maxSecretSize is swarm's limit on a secret's value: 500 KB, as swarmkit's
// MaxSecretSize counts it (500 × 1024 bytes). Checking it here turns a late
// "secret data must be larger than 0 and less than 512000 bytes" from the
// manager into a message on the status line before anything is sent.
const maxSecretSize = 500 * 1024

// secretFieldError is a validation error that belongs to one of the dialog's
// input rows; the dialog moves focus there.
type secretFieldError struct {
	field int // secretFieldName or secretFieldLabels
	msg   string
}

func (e *secretFieldError) Error() string { return e.msg }

const (
	secretFieldName = iota
	secretFieldLabels
	secretFieldValue
)

// validateSecret checks the dialog's input before anything is sent: a name, a
// well-formed label list, a value that is neither empty nor over the swarm
// limit. The value is checked as given — nothing is trimmed or appended.
func validateSecret(name, labels, value string) (string, map[string]string, error) {
	nm := strings.TrimSpace(name)
	if nm == "" {
		return "", nil, &secretFieldError{secretFieldName, "name is required"}
	}
	lbls, err := parseKVList(labels)
	if err != nil {
		return "", nil, &secretFieldError{secretFieldLabels, "invalid labels: " + err.Error()}
	}
	if value == "" {
		return "", nil, errors.New("value is empty — type it, paste it, or :r <file>")
	}
	if len(value) > maxSecretSize {
		return "", nil, fmt.Errorf("value is %d bytes — over the swarm limit of %d (500 KB)", len(value), maxSecretSize)
	}
	return nm, lbls, nil
}

// readSecretFile reads a local file for :r. have is the size of the text
// already in the editor, so the result can be refused before it pushes the
// value over the limit. "~" and "~/…" expand to the home directory; a relative
// path is relative to where swarmexec was started.
//
// The editor holds text, so only UTF-8 text without NUL bytes is accepted. A
// binary file (a keystore, a DER certificate) is refused with a pointer to
// `docker secret create`, which takes raw bytes; the TextArea could not show
// or keep them intact.
func readSecretFile(path string, have int) (string, error) {
	if path == "" {
		return "", errors.New("usage: :r <file>")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %v", err)
		}
		path = filepath.Join(home, path[1:])
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("no such file: %s", path)
	case err != nil:
		return "", err
	case fi.IsDir():
		return "", fmt.Errorf("%s is a directory", path)
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("%s is not a regular file", path)
	case fi.Size() > maxSecretSize:
		return "", fmt.Errorf("%s is %d bytes — over the swarm limit of %d (500 KB)", path, fi.Size(), maxSecretSize)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSecretSize+1)) // it may have grown since the stat
	if err != nil {
		return "", err
	}
	if len(data) > maxSecretSize {
		return "", fmt.Errorf("%s is over the swarm limit of %d bytes (500 KB)", path, maxSecretSize)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("%s is binary, not text — use docker secret create NAME FILE", path)
	}
	if have+len(data) > maxSecretSize {
		return "", fmt.Errorf("%s (%d bytes) would make the value %d bytes — over the swarm limit of %d (500 KB)", path, len(data), have+len(data), maxSecretSize)
	}
	return string(data), nil
}

// sizeText is "N lines · M bytes" for a text; a last line without a newline
// counts as a line.
func sizeText(text string) string {
	lines := strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		lines++
	}
	unit := "lines"
	if lines == 1 {
		unit = "line"
	}
	return fmt.Sprintf("%d %s · %d bytes", lines, unit, len(text))
}

// secretSize is the status-line size of a value, red once it is over the
// swarm limit.
func secretSize(text string) string {
	s := sizeText(text)
	if len(text) > maxSecretSize {
		return "[red::b]" + s + " > 500 KB[-::-]"
	}
	return "[gray]" + s + "[-]"
}

// secretFooterKeys is the footer while the value editor has focus.
func secretFooterKeys(m vimMode) string {
	switch m {
	case vimInsert:
		return footerKeys("Esc", "normal mode", "Tab", "types a tab", "Ctrl-S", "create")
	case vimCommand:
		return footerKeys("Enter", "run", "Esc", "cancel", ":r file", "read", ":w", "create", ":q!", "discard")
	}
	return footerKeys("i", "insert", "Tab", "fields", ":r file", "read", ":w", "create", ":q", "cancel", "?", "help")
}

// secretFieldFooter is the footer while Name or Labels has focus.
var secretFieldFooter = footerKeys("Tab", "next field", "S-Tab", "previous", "Enter", "next",
	"Ctrl-S", "create", "Esc", "to the value (cancels when empty)")

// secretDialog is the create-secret overlay: Name and Labels rows above the
// vim editor that holds the value.
type secretDialog struct {
	root   *tview.Flex
	name   *tview.InputField
	labels *tview.InputField
	ed     *vimEditor
}

// showCreateSecret opens the create-secret dialog for the Secrets tab.
func (u *ui) showCreateSecret() {
	dcli, ctx, gen := u.dcli, u.ctx, u.generation()
	u.openSecretEditor(u.sectable,
		func(name string, data []byte, labels map[string]string) error {
			return createSecret(ctx, dcli, name, data, labels)
		},
		// The dialog belongs to the cluster it was opened on; a result that
		// arrives after a switch away is dropped (see async).
		func(fn func()) { u.onCluster(gen, fn) },
		func(name string) {
			u.loadSecrets()
			u.flash(" [green]created[white] secret " + name)
		})
}

// openSecretEditor builds the dialog. create runs off the UI goroutine and its
// result comes back through queue (QueueUpdateDraw in the app); done runs after
// a successful create, once the dialog is closed.
//
// Keys: focus starts on Name — a new secret always has an empty one and the
// value is useless without it. Tab / Enter move Name → Labels → value, Shift-Tab
// goes back; in the value editor Tab moves only in Normal mode, so Insert mode
// can type the tabs a file may need. Esc in a field goes to the value editor,
// or closes the dialog while nothing has been entered. :w, :wq, ZZ and Ctrl-S
// (anywhere) create; a validation error lands on the status line and the
// dialog stays open.
//
// The value is sent byte for byte as the editor holds it: no trailing newline
// is added or stripped. Typing "s3cret" gives exactly six bytes; a file read
// with :r keeps its own final newline.
func (u *ui) openSecretEditor(back tview.Primitive, create func(string, []byte, map[string]string) error, queue func(func()), done func(string)) *secretDialog {
	d := &secretDialog{ed: newVimEditor("value", "")}
	ed := d.ed
	d.name = tview.NewInputField().SetLabel("Name   ").SetFieldWidth(0)
	d.labels = tview.NewInputField().SetLabel("Labels ").SetFieldWidth(0).
		SetPlaceholder("optional: k=v,k=v")
	fields := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(d.name, 1, 0, true).
		AddItem(d.labels, 1, 0, false)
	// A one-column margin each side. Boxes, not padding: a Flex does not
	// clear its background, so padding would let the backdrop show through.
	padded := tview.NewFlex().
		AddItem(tview.NewBox(), 1, 0, false).
		AddItem(fields, 0, 1, true).
		AddItem(tview.NewBox(), 1, 0, false)
	d.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(padded, 2, 0, true).
		AddItem(ed.root, 0, 1, false)
	d.root.SetBorder(true).SetTitle(" new secret · ? help ")

	ov := u.overlayFor(pageSecForm, back, secretFieldFooter)
	order := []tview.Primitive{d.name, d.labels, ed.ta}
	cur := 0
	focus := func(i int) {
		cur = (i + len(order)) % len(order)
		u.app.SetFocus(order[cur])
	}
	d.name.SetFocusFunc(func() { cur = secretFieldName; ov.setHelp(secretFieldFooter) })
	d.labels.SetFocusFunc(func() { cur = secretFieldLabels; ov.setHelp(secretFieldFooter) })
	ed.ta.SetFocusFunc(func() { cur = secretFieldValue; ov.setHelp(secretFooterKeys(ed.mode)) })

	ed.extraDirty = func() bool { return d.name.GetText() != "" || d.labels.GetText() != "" }
	ed.posInfo = secretSize
	ed.onMode = func(m vimMode) { ov.setHelp(secretFooterKeys(m)) }
	ed.onQuit = ov.Close
	ed.onTab = func(back bool) {
		if back {
			focus(cur - 1)
		} else {
			focus(cur + 1)
		}
	}
	ed.onHelp = func() {
		u.showVimHelp(ed.ta, sectionHelp("Secret",
			"Tab  Shift-Tab", "Name → Labels → value (Normal mode; Insert types a tab)",
			":r <file>", "insert a local file at the cursor (~ works); text only",
			":w  Ctrl-S", "create the secret — the value is sent byte for byte",
			"500 KB", "swarm's limit (512000 bytes); the status line shows the size",
			"binary", "not editable here — use docker secret create NAME FILE"))
	}
	read := secretReadCommand(ed)
	ed.commands = map[string]func(string, string) error{"r": read, "read": read}

	busy := false
	ed.onWrite = func(text string) error {
		if busy {
			return errors.New("still creating — wait for the result")
		}
		nm, lbls, err := validateSecret(d.name.GetText(), d.labels.GetText(), text)
		if err != nil {
			var fe *secretFieldError
			if errors.As(err, &fe) {
				focus(fe.field)
			}
			return err
		}
		busy = true
		ed.setMsg("creating secret "+nm+" …", false)
		data := []byte(text)
		go func() {
			err := create(nm, data, lbls)
			queue(func() {
				busy = false
				if err != nil {
					ed.setMsg("create failed: "+err.Error(), true)
					return
				}
				ov.Close()
				if done != nil {
					done(nm)
				}
			})
		}()
		return nil
	}

	fieldKeys := func(ev *tcell.EventKey) *tcell.EventKey {
		switch ev.Key() {
		case tcell.KeyTab, tcell.KeyEnter:
			focus(cur + 1)
		case tcell.KeyBacktab:
			focus(cur - 1)
		case tcell.KeyCtrlS:
			ed.write()
		case tcell.KeyEscape:
			if !ed.dirty() {
				ov.Close()
				return nil
			}
			focus(secretFieldValue)
		default:
			return ev
		}
		return nil
	}
	refresh := func(string) { ed.render() } // "modified" follows the fields too
	for _, f := range []*tview.InputField{d.name, d.labels} {
		f.SetChangedFunc(refresh)
		f.SetInputCapture(fieldKeys)
	}

	ed.render() // the size, from the start
	ov.show(centeredGrow(d.root, 110, 30, 0.85), d.name)
	return d
}

// secretReadCommand is :r for a secret value editor: insert a local text file
// at the cursor, refused when it would push the value over the limit.
func secretReadCommand(ed *vimEditor) func(text, arg string) error {
	return func(text, arg string) error {
		data, err := readSecretFile(arg, len(text))
		if err != nil {
			return err
		}
		ed.insertAt(data)
		lines := strings.Count(data, "\n")
		if data != "" && !strings.HasSuffix(data, "\n") {
			lines++
		}
		ed.setMsg(fmt.Sprintf("%q %dL, %dB read", arg, lines, len(data)), false) // as vim says it
		return nil
	}
}

// rotateFooterKeys is the rotate editor's footer.
func rotateFooterKeys(m vimMode) string {
	switch m {
	case vimInsert:
		return footerKeys("Esc", "normal mode", "Tab", "types a tab", "Ctrl-S", "rotate")
	case vimCommand:
		return footerKeys("Enter", "run", "Esc", "cancel", ":r file", "read", ":w", "rotate", ":q!", "discard")
	}
	return footerKeys("i", "insert", ":r file", "read", ":w", "rotate", ":q", "cancel", "?", "help")
}

// rotateHooks are what the rotate editor needs from outside; injected so the
// dialog runs in a test without a cluster or an event loop.
type rotateHooks struct {
	// plan reads what the rotation would touch. It runs off the UI goroutine.
	plan func() (rotatePlan, error)
	// queue runs a function on the UI goroutine (QueueUpdateDraw).
	queue func(func())
	// confirm asks; ok runs when the operator agrees. Cancel returns to back.
	confirm func(msg string, back tview.Primitive, ok func())
	// start runs the rotation, once the editor is closed.
	start func(data []byte)
}

// rotateDialog is the rotate-secret overlay: the fixed name and labels above
// the vim editor that takes the new value.
type rotateDialog struct {
	root *tview.Flex
	head *tview.TextView
	ed   *vimEditor
}

// openRotateEditor opens the editor for a secret's new value. The name and
// labels are shown but not editable — keeping them is the point of rotating.
// :w (or Ctrl-S) checks the value, reads the services that use the secret and
// asks; only a yes closes the editor and starts the rotation. The value is
// sent byte for byte, as in the create dialog.
func (u *ui) openRotateEditor(back tview.Primitive, name string, labels map[string]string, h rotateHooks) *rotateDialog {
	d := &rotateDialog{ed: newVimEditor("new value", "")}
	ed := d.ed
	d.head = tview.NewTextView().SetDynamicColors(true)
	head := fmt.Sprintf("Name    %s  [gray](kept)[-]\nLabels  ", tview.Escape(name))
	if len(labels) == 0 {
		head += "-"
	} else {
		keys := make([]string, 0, len(labels))
		for k := range labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		kv := make([]string, len(keys))
		for i, k := range keys {
			kv[i] = k + "=" + labels[k]
		}
		head += tview.Escape(strings.Join(kv, ",")) + "  [gray](kept)[-]"
	}
	d.head.SetText(head)
	padded := tview.NewFlex().
		AddItem(tview.NewBox(), 1, 0, false).
		AddItem(d.head, 0, 1, false).
		AddItem(tview.NewBox(), 1, 0, false)
	d.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(padded, 2, 0, false).
		AddItem(ed.root, 0, 1, true)
	d.root.SetBorder(true).SetTitle(" rotate secret " + tview.Escape(name) + " · ? help ")

	ov := u.overlayFor(pageSecRotate, back, rotateFooterKeys(vimNormal))
	ed.posInfo = secretSize
	ed.onMode = func(m vimMode) { ov.setHelp(rotateFooterKeys(m)) }
	ed.onQuit = ov.Close
	ed.onHelp = func() {
		u.showVimHelp(ed.ta, sectionHelp("Rotate",
			":r <file>", "insert a local file at the cursor (~ works); text only",
			":w  Ctrl-S", "rotate — lists the services that restart and asks first",
			"name, labels", "kept; the services are parked on "+rotateTempName(name)+" meanwhile",
			"restarts", "every service using the secret restarts twice",
			"rollback", "none — the old secret object is deleted",
			"500 KB", "swarm's limit (512000 bytes); the status line shows the size"))
	}
	read := secretReadCommand(ed)
	ed.commands = map[string]func(string, string) error{"r": read, "read": read}

	busy := false
	ed.onWrite = func(text string) error {
		if busy {
			return errors.New("still reading the services — wait a moment")
		}
		if _, _, err := validateSecret(name, "", text); err != nil {
			return err
		}
		busy = true
		ed.setMsg("reading the services that use "+name+" …", false)
		data := []byte(text)
		go func() {
			p, err := h.plan()
			h.queue(func() {
				busy = false
				if err != nil {
					ed.setMsg(err.Error(), true)
					return
				}
				ed.setMsg("", false)
				h.confirm(rotateConfirmText(p)+"\nRotate "+name+" now?", ed.ta, func() {
					ov.Close()
					h.start(data)
				})
			})
		}()
		return nil
	}
	ed.render()
	ov.show(centeredGrow(d.root, 110, 30, 0.85), ed.ta)
	return d
}
