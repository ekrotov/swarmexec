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

type createCall struct {
	name   string
	data   []byte
	labels map[string]string
}

// secretHarness opens the dialog on a bare ui with a fake create. Results of
// the background create come back on queued and are run by the test, standing
// in for QueueUpdateDraw on the UI goroutine.
type secretHarness struct {
	t       *testing.T
	u       *ui
	d       *secretDialog
	calls   chan createCall
	queued  chan func()
	created []string
	err     error // what the fake create returns
}

func openSecretHarness(t *testing.T) *secretHarness {
	t.Helper()
	h := &secretHarness{t: t, u: overlayUI(), calls: make(chan createCall, 4), queued: make(chan func(), 4)}
	h.d = h.u.openSecretEditor(tview.NewBox(),
		func(name string, data []byte, labels map[string]string) error {
			h.calls <- createCall{name, data, labels}
			return h.err
		},
		func(fn func()) { h.queued <- fn },
		func(name string) { h.created = append(h.created, name) })
	return h
}

// keys sends "dd<Esc>:w<CR>"-style input (plus <Tab>, <S-Tab>) to whatever
// has focus, through the front page as the running app would.
func (h *secretHarness) keys(s string) {
	h.t.Helper()
	_, page := h.u.pages.GetFrontPage()
	for _, ev := range parseKeys(h.t, s) {
		page.InputHandler()(ev, func(p tview.Primitive) { h.u.app.SetFocus(p) })
	}
}

func (h *secretHarness) focused() string {
	switch h.u.app.GetFocus() {
	case h.d.name:
		return "name"
	case h.d.labels:
		return "labels"
	case h.d.ed.ta:
		return "value"
	}
	return "other"
}

// settle waits for the fake create and runs what it queued.
func (h *secretHarness) settle() createCall {
	h.t.Helper()
	select {
	case c := <-h.calls:
		select {
		case fn := <-h.queued:
			fn()
		case <-time.After(5 * time.Second):
			h.t.Fatal("create never reported back")
		}
		return c
	case <-time.After(5 * time.Second):
		h.t.Fatal("create was never called")
	}
	return createCall{}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidateSecret(t *testing.T) {
	big := strings.Repeat("x", maxSecretSize+1)
	for _, tc := range []struct {
		name, labels, value string
		field               int // -1: not a field error
		ok                  bool
	}{
		{"db", "", "v", 0, true},
		{"  db  ", "a=b, c=d", "v\n", 0, true},
		{"db", "", strings.Repeat("x", maxSecretSize), 0, true},
		{"", "", "v", secretFieldName, false},
		{"   ", "", "v", secretFieldName, false},
		{"db", "nokey", "v", secretFieldLabels, false},
		{"db", "", "", -1, false},
		{"db", "", big, -1, false},
	} {
		nm, lbls, err := validateSecret(tc.name, tc.labels, tc.value)
		if tc.ok {
			if err != nil {
				t.Errorf("%q/%q: unexpected %v", tc.name, tc.labels, err)
			}
			if nm != strings.TrimSpace(tc.name) {
				t.Errorf("name = %q", nm)
			}
			if tc.labels != "" && lbls["c"] != "d" {
				t.Errorf("labels = %v", lbls)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q/%q/%d bytes: want an error", tc.name, tc.labels, len(tc.value))
			continue
		}
		var fe *secretFieldError
		if got := errors.As(err, &fe); got != (tc.field >= 0) || got && fe.field != tc.field {
			t.Errorf("%q/%q: error %v, want field %d", tc.name, tc.labels, err, tc.field)
		}
	}
}

func TestReadSecretFile(t *testing.T) {
	content := "-----BEGIN KEY-----\r\n\tab\u00e9\r\n-----END KEY-----\n"
	p := writeFile(t, "key.pem", content)
	got, err := readSecretFile(p, 0)
	if err != nil || got != content {
		t.Fatalf("read = %q, %v; want the bytes as they are", got, err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "h.txt"), []byte("home"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readSecretFile("~/h.txt", 0); err != nil || got != "home" {
		t.Errorf("~ expansion: %q, %v", got, err)
	}

	errCases := map[string]struct {
		path string
		have int
		want string
	}{
		"empty":    {"", 0, "usage"},
		"missing":  {filepath.Join(home, "nope"), 0, "no such file"},
		"dir":      {home, 0, "directory"},
		"too big":  {writeFile(t, "big", strings.Repeat("x", maxSecretSize+1)), 0, "limit"},
		"invalid":  {writeFile(t, "bin", "ok\xff\xfe"), 0, "binary"},
		"nul":      {writeFile(t, "nul", "a\x00b"), 0, "binary"},
		"combined": {writeFile(t, "half", strings.Repeat("x", 300*1024)), 300 * 1024, "would make"},
	}
	for name, tc := range errCases {
		_, err := readSecretFile(tc.path, tc.have)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// Focus starts on Name; Tab and Enter walk Name → Labels → value and round;
// in the editor Tab moves only in Normal mode — Insert mode types a tab.
func TestSecretEditorTabCycle(t *testing.T) {
	h := openSecretHarness(t)
	steps := []struct{ keys, want string }{
		{"", "name"},
		{"<Tab>", "labels"},
		{"<Tab>", "value"},
		{"<Tab>", "name"},
		{"<S-Tab>", "value"},
		{"<S-Tab>", "labels"},
		{"<S-Tab>", "name"},
		{"<CR>", "labels"},
		{"<CR>", "value"},
	}
	for _, s := range steps {
		h.keys(s.keys)
		if got := h.focused(); got != s.want {
			t.Fatalf("after %q focus is on %s, want %s", s.keys, got, s.want)
		}
	}
	h.keys("ia<Tab>b")
	if got := h.d.ed.ta.GetText(); got != "a\tb" {
		t.Fatalf("Insert-mode Tab must type a tab, text = %q", got)
	}
	if h.focused() != "value" {
		t.Fatal("Insert-mode Tab must not move focus")
	}
	h.keys("<Esc><S-Tab>")
	if h.focused() != "labels" {
		t.Fatalf("Normal-mode Shift-Tab goes back to Labels, focus on %s", h.focused())
	}
}

// :r puts the file at the cursor, byte for byte, as one undo step.
func TestSecretEditorReadFile(t *testing.T) {
	h := openSecretHarness(t)
	content := "line1\r\n\tline2\n"
	p := writeFile(t, "c.txt", content)
	h.keys("<Tab><Tab>iAB<Esc>") // cursor back on B
	h.keys(":r " + p + "<CR>")
	if got, want := h.d.ed.ta.GetText(), "A"+content+"B"; got != want {
		t.Fatalf("after :r text = %q, want %q", got, want)
	}
	if h.d.ed.msgErr || !strings.Contains(h.d.ed.msg, "2L, 14B read") {
		t.Errorf("status = %q (err=%v)", h.d.ed.msg, h.d.ed.msgErr)
	}
	h.keys("u")
	if got := h.d.ed.ta.GetText(); got != "AB" {
		t.Errorf("u must undo the whole read, text = %q", got)
	}
	h.keys(":read " + filepath.Join(filepath.Dir(p), "missing") + "<CR>")
	if !h.d.ed.msgErr || !strings.Contains(h.d.ed.msg, "no such file") {
		t.Errorf("missing file: status = %q", h.d.ed.msg)
	}
	if got := h.d.ed.ta.GetText(); got != "AB" {
		t.Errorf("a failed read must not change the text, got %q", got)
	}
}

// :w validates first and keeps the dialog open on an error, moving to the
// field that is wrong; a valid dialog sends exactly the editor's bytes.
func TestSecretEditorWriteSendsExactBytes(t *testing.T) {
	h := openSecretHarness(t)
	content := "{\r\n\t\"k\": \"v\"\r\n}\n"
	p := writeFile(t, "s.json", content)
	h.keys("<Tab><Tab>:r " + p + "<CR>:w<CR>")
	if !h.d.ed.msgErr || !strings.Contains(h.d.ed.msg, "name is required") {
		t.Fatalf("empty name: status = %q", h.d.ed.msg)
	}
	if h.focused() != "name" || !h.u.overlayOpen() {
		t.Fatalf("an empty name must keep the dialog open and focus Name (focus %s)", h.focused())
	}
	h.keys("db-config<Tab>bad<C-s>")
	if !strings.Contains(h.d.ed.msg, "invalid labels") || h.focused() != "labels" {
		t.Fatalf("bad labels: status = %q, focus %s", h.d.ed.msg, h.focused())
	}
	h.d.labels.SetText("env=prod")
	h.keys("<Tab>:w<CR>")
	c := h.settle()
	if c.name != "db-config" || string(c.data) != content || c.labels["env"] != "prod" {
		t.Fatalf("create got %q %q %v", c.name, c.data, c.labels)
	}
	if h.u.overlayOpen() || len(h.created) != 1 {
		t.Errorf("a successful create closes the dialog and reports (open=%v created=%v)", h.u.overlayOpen(), h.created)
	}
}

// Typed text goes out as typed: no newline is added.
func TestSecretEditorTypedValueHasNoTrailingNewline(t *testing.T) {
	h := openSecretHarness(t)
	h.keys("pw<Tab><Tab>is3cret<C-s>")
	if c := h.settle(); string(c.data) != "s3cret" {
		t.Fatalf("value = %q", c.data)
	}
}

// A failed create keeps the dialog and the text, with the reason shown.
func TestSecretEditorCreateFailureStaysOpen(t *testing.T) {
	h := openSecretHarness(t)
	h.err = errors.New(`secret "pw" already exists`)
	h.keys("pw<Tab><Tab>ix<Esc>ZZ")
	h.settle()
	if !h.u.overlayOpen() || !h.d.ed.msgErr || !strings.Contains(h.d.ed.msg, "already exists") {
		t.Fatalf("open=%v status=%q", h.u.overlayOpen(), h.d.ed.msg)
	}
	if h.d.ed.ta.GetText() != "x" || len(h.created) != 0 {
		t.Error("the text must survive a failed create")
	}
}

// :q leaves a clean dialog, refuses with anything entered (the fields count
// too), :q! always leaves. Esc in an empty field cancels; with input it goes
// to the editor.
func TestSecretEditorQuit(t *testing.T) {
	h := openSecretHarness(t)
	h.keys("<Esc>")
	if h.u.overlayOpen() {
		t.Fatal("Esc on an untouched dialog cancels it")
	}

	h = openSecretHarness(t)
	h.keys("<Tab><Tab>:q<CR>")
	if h.u.overlayOpen() {
		t.Fatal(":q on an untouched dialog cancels it")
	}

	h = openSecretHarness(t)
	h.keys("pw<Esc>")
	if !h.u.overlayOpen() || h.focused() != "value" {
		t.Fatalf("Esc in a field with input goes to the editor (open=%v focus=%s)", h.u.overlayOpen(), h.focused())
	}
	h.keys(":q<CR>")
	if !h.u.overlayOpen() || !h.d.ed.msgErr {
		t.Fatal(":q must refuse while the name holds input")
	}
	h.keys(":q!<CR>")
	if h.u.overlayOpen() {
		t.Fatal(":q! always cancels")
	}
	select {
	case <-h.calls:
		t.Fatal("nothing may be created on cancel")
	default:
	}
}

// The status line shows the size; over the limit it turns red.
func TestSecretSizeStatus(t *testing.T) {
	if got := sizeText("a\nb"); got != "2 lines · 3 bytes" {
		t.Errorf("sizeText = %q", got)
	}
	if got := sizeText(""); got != "0 lines · 0 bytes" {
		t.Errorf("sizeText empty = %q", got)
	}
	if got := secretSize(strings.Repeat("x", maxSecretSize+1)); !strings.Contains(got, "[red") {
		t.Errorf("over the limit must be red: %q", got)
	}
}

// The dialog sits inside its border with the footer rows free, on a small and
// on a big terminal — and is bigger on the big one.
func TestSecretEditorLayout(t *testing.T) {
	applyTheme(backdropDim)
	heights := map[int]int{}
	for _, size := range [][2]int{{80, 24}, {120, 40}} {
		w, hgt := size[0], size[1]
		h := openSecretHarness(t)
		h.d.ed.ta.SetText(strings.Repeat("a very long line of secret material that wraps past the width of the box ", 4)+"\nend", false)
		// A page full of # beneath: any cell the dialog leaves undrawn shows one.
		h.u.pages.AddPage(pageMain, tview.NewTextView().SetText(strings.Repeat(strings.Repeat("#", w)+"\n", hgt)), true, true)
		h.u.pages.SendToBack(pageMain)
		rows := drawForm(h.u, w, hgt)
		screen := strings.Join(rows, "\n")
		for _, want := range []string{"new secret", "Name", "Labels", "value", "NORMAL", "bytes"} {
			if !strings.Contains(screen, want) {
				t.Errorf("%dx%d: %q not on screen:\n%s", w, hgt, want, screen)
			}
		}
		top, bottom := -1, -1
		for y, r := range rows {
			if top < 0 && strings.Contains(r, "╭") {
				top = y
			}
			if strings.Contains(r, "╰") {
				bottom = y
			}
		}
		if top < 0 || bottom < 0 {
			t.Fatalf("%dx%d: no border:\n%s", w, hgt, screen)
		}
		col := len([]rune(rows[top][:strings.IndexRune(rows[top], '╮')]))
		for y := top; y <= bottom; y++ {
			r := []rune(rows[y])
			if col >= len(r) {
				t.Fatalf("%dx%d row %d too short: %q", w, hgt, y, rows[y])
			}
			if rest := strings.Trim(string(r[col+1:]), " #"); rest != "" {
				t.Errorf("%dx%d row %d: text past the border: %q", w, hgt, y, rows[y])
			}
			left := strings.IndexAny(rows[y], "│╭╰")
			if inside := string(r[len([]rune(rows[y][:left])):col]); strings.Contains(inside, "#") {
				t.Errorf("%dx%d row %d: the backdrop shows through the dialog: %q", w, hgt, y, inside)
			}
		}
		if bottom >= hgt-overlayFooterReserve {
			t.Errorf("%dx%d: the dialog covers the footer (bottom border on row %d)", w, hgt, bottom)
		}
		heights[hgt] = bottom - top + 1
		t.Logf("%dx%d:\n%s", w, hgt, screen)
	}
	if heights[40] <= heights[24] {
		t.Errorf("the dialog must use a bigger terminal: heights %v", heights)
	}
}
