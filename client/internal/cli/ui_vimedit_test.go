// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// newTestVim builds an editor laid out on a simulation screen, so the
// TextArea knows its width as it does on a real terminal.
func newTestVim(t *testing.T, text string) (*vimEditor, func(keys string)) {
	t.Helper()
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scr.Fini)
	scr.SetSize(80, 20)
	e := newVimEditor("test", text)
	e.root.SetRect(0, 0, 80, 20)
	e.root.Draw(scr)
	h := e.ta.InputHandler()
	press := func(keys string) {
		for _, ev := range parseKeys(t, keys) {
			h(ev, func(tview.Primitive) {})
			e.root.Draw(scr)
		}
	}
	return e, press
}

// parseKeys turns "dd<Esc>:w<CR>" into key events: runes, plus <Esc>, <CR>,
// <BS>, <C-r> and <C-s>.
func parseKeys(t *testing.T, keys string) []*tcell.EventKey {
	t.Helper()
	special := map[string]tcell.Key{"<Esc>": tcell.KeyEscape, "<CR>": tcell.KeyEnter,
		"<BS>": tcell.KeyBackspace2, "<C-r>": tcell.KeyCtrlR, "<C-s>": tcell.KeyCtrlS}
	var out []*tcell.EventKey
	for keys != "" {
		matched := false
		for name, k := range special {
			if strings.HasPrefix(keys, name) {
				out = append(out, tcell.NewEventKey(k, 0, tcell.ModNone))
				keys = keys[len(name):]
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		r := []rune(keys)[0]
		out = append(out, tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		keys = keys[len(string(r)):]
	}
	return out
}

// cur returns the text with a | at the cursor, which makes expectations easy
// to read.
func cur(e *vimEditor) string {
	t, p := e.ta.GetText(), e.cursor()
	return t[:p] + "|" + t[p:]
}

func TestVimMotions(t *testing.T) {
	e, press := newTestVim(t, "KEY=some/path value\nB=2")
	steps := []struct{ keys, want string }{
		{"w", "KEY|=some/path value\nB=2"},
		{"w", "KEY=|some/path value\nB=2"},
		{"e", "KEY=som|e/path value\nB=2"},
		{"b", "KEY=|some/path value\nB=2"},
		{"$", "KEY=some/path valu|e\nB=2"},
		{"0", "|KEY=some/path value\nB=2"},
		{"3l", "KEY|=some/path value\nB=2"},
		{"j", "KEY=some/path value\nB=|2"},
		{"k", "KEY|=some/path value\nB=2"}, // back to the column it came from
		{"G", "KEY=some/path value\n|B=2"},
		{"gg", "|KEY=some/path value\nB=2"},
		{"2G", "KEY=some/path value\n|B=2"},
		{"h", "KEY=some/path value\n|B=2"}, // h stops at the line start
	}
	for _, s := range steps {
		press(s.keys)
		if got := cur(e); got != s.want {
			t.Fatalf("after %q: got %q, want %q", s.keys, got, s.want)
		}
	}
}

// j / k keep the column the cursor came from, even across a shorter line.
func TestVimStickyColumn(t *testing.T) {
	e, press := newTestVim(t, "ABCDEFG\nAB\nABCDEFG")
	press("5l")
	press("j")
	if got := cur(e); got != "ABCDEFG\nA|B\nABCDEFG" {
		t.Fatalf("onto the short line: %q", got)
	}
	press("j")
	if got := cur(e); got != "ABCDEFG\nAB\nABCDE|FG" {
		t.Fatalf("back to the column: %q", got)
	}
}

func TestVimEditing(t *testing.T) {
	cases := []struct{ name, text, keys, want string }{
		{"x", "ABC", "lx", "A|C"},
		{"count x", "ABCD", "2x", "|CD"},
		{"dd", "A=1\nB=2\nC=3", "jdd", "A=1\n|C=3"},
		{"dd last line", "A=1\nB=2", "jdd", "|A=1"},
		{"2dd", "A=1\nB=2\nC=3", "2dd", "|C=3"},
		{"yy p", "A=1\nB=2", "yyp", "A=1\n|A=1\nB=2"},
		{"yy P", "A=1\nB=2", "jyyP", "A=1\n|B=2\nB=2"},
		{"dd p moves a line", "A=1\nB=2\nC=3", "ddp", "B=2\n|A=1\nC=3"},
		{"yy p on last line", "A=1", "yyp", "A=1\n|A=1"},
		{"o", "A=1\nB=2", "oX=9<Esc>", "A=1\nX=|9\nB=2"},
		{"O", "A=1\nB=2", "jOX=9<Esc>", "A=1\nX=|9\nB=2"},
		{"A", "A=1", "AX<Esc>", "A=1|X"},
		{"I", "  A=1", "$IX<Esc>", "  |XA=1"},
		{"a", "AB", "aX<Esc>", "A|XB"},
		{"D", "KEY=value", "4lD", "KEY|="},
		{"C", "KEY=value", "4lCnew<Esc>", "KEY=ne|w"},
		{"cw", "KEY=old rest", "4lcwnew<Esc>", "KEY=ne|w rest"},
		{"dw", "KEY=old rest", "4ldw", "KEY=|rest"},
		{"d$", "KEY=value", "3ld$", "KE|Y"},
		{"cc", "A=1\nB=2", "ccX=0<Esc>", "X=|0\nB=2"},
		{"r", "A=1", "2lr2", "A=|2"},
		{"J", "A=1\n  B=2", "J", "A=1| B=2"},
		{"u", "A=1\nB=2", "ddu", "|A=1\nB=2"},
		{"u redo", "A=1\nB=2", "ddu<C-r>", "|B=2"},
		{"normal ignores text", "A=1", "zq", "|A=1"},
		{"/ search", "A=1\nB=2\nC=2", "/=2<CR>", "A=1\nB|=2\nC=2"},
		{"n", "A=1\nB=2\nC=2", "/=2<CR>n", "A=1\nB=2\nC|=2"},
		{"n wraps", "A=1\nB=2\nC=2", "/=2<CR>nn", "A=1\nB|=2\nC=2"},
		{":N", "A=1\nB=2\nC=3", ":3<CR>", "A=1\nB=2\n|C=3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, press := newTestVim(t, c.text)
			press(c.keys)
			if got := cur(e); got != c.want {
				t.Fatalf("%q on %q: got %q, want %q", c.keys, c.text, got, c.want)
			}
			if e.mode != vimNormal {
				t.Fatalf("ended in mode %d, want Normal", e.mode)
			}
		})
	}
}

func TestVimModes(t *testing.T) {
	var modes []vimMode
	e, press := newTestVim(t, "A=1")
	e.onMode = func(m vimMode) { modes = append(modes, m) }
	press("i")
	if e.mode != vimInsert {
		t.Fatal("i does not enter Insert mode")
	}
	press("hj") // typed, not moved
	if e.ta.GetText() != "hjA=1" {
		t.Fatalf("Insert mode text: %q", e.ta.GetText())
	}
	press("<Esc>:")
	if e.mode != vimCommand {
		t.Fatal(": does not enter Command mode")
	}
	press("<Esc>")
	if e.mode != vimNormal || len(modes) != 4 {
		t.Fatalf("mode %d, reported %v", e.mode, modes)
	}
}

func TestVimWriteAndQuit(t *testing.T) {
	t.Run("write passes the text", func(t *testing.T) {
		e, press := newTestVim(t, "A=1")
		var got string
		e.onWrite = func(s string) error { got = s; return nil }
		press("xx:w<CR>")
		if got != "1" {
			t.Fatalf(":w wrote %q", got)
		}
	})
	t.Run("Ctrl-S writes from Insert mode", func(t *testing.T) {
		e, press := newTestVim(t, "A=1")
		var got string
		e.onWrite = func(s string) error { got = s; return nil }
		press("AX<C-s>")
		if got != "A=1X" || e.mode != vimNormal {
			t.Fatalf("Ctrl-S wrote %q, mode %d", got, e.mode)
		}
	})
	t.Run("a line error moves to its line", func(t *testing.T) {
		e, press := newTestVim(t, "A=1\nB=2\nbad")
		e.onWrite = func(string) error { return &lineError{line: 3, msg: "nope"} }
		press(":w<CR>")
		if got := cur(e); got != "A=1\nB=2\n|bad" {
			t.Fatalf("cursor after the error: %q", got)
		}
		if !e.msgErr || !strings.Contains(e.msg, "line 3") {
			t.Fatalf("status message %q", e.msg)
		}
	})
	t.Run("plain errors stay where they are", func(t *testing.T) {
		e, press := newTestVim(t, "A=1")
		e.onWrite = func(string) error { return errors.New("boom") }
		press("l:w<CR>")
		if got := cur(e); got != "A|=1" || e.msg != "boom" {
			t.Fatalf("got %q, message %q", got, e.msg)
		}
	})
	t.Run(":q refuses with changes, :q! leaves", func(t *testing.T) {
		e, press := newTestVim(t, "A=1")
		quit := 0
		e.onQuit = func() { quit++ }
		press("x:q<CR>")
		if quit != 0 || !e.msgErr {
			t.Fatal(":q left despite changes")
		}
		press("<Esc>") // Esc with changes explains instead of leaving
		if quit != 0 {
			t.Fatal("Esc left despite changes")
		}
		press(":q!<CR>")
		if quit != 1 {
			t.Fatal(":q! did not leave")
		}
	})
	t.Run("Esc leaves when nothing changed", func(t *testing.T) {
		e, press := newTestVim(t, "A=1")
		quit := 0
		e.onQuit = func() { quit++ }
		press("l<Esc>")
		if quit != 1 {
			t.Fatal("Esc did not leave a clean editor")
		}
	})
	t.Run("owner commands run", func(t *testing.T) {
		e, press := newTestVim(t, "A=1")
		ran := ""
		e.commands = map[string]func(string) error{"list": func(s string) error { ran = s; return nil }}
		press(":list<CR>")
		if ran != "A=1" {
			t.Fatalf(":list got %q", ran)
		}
		press(":bogus<CR>")
		if !e.msgErr {
			t.Fatal("an unknown command shows no error")
		}
	})
	t.Run("? opens the help", func(t *testing.T) {
		e, press := newTestVim(t, "A=1")
		help := 0
		e.onHelp = func() { help++ }
		press("?:help<CR>")
		if help != 2 {
			t.Fatalf("help opened %d times", help)
		}
	})
}

// The status line shows the mode and the cursor position.
func TestVimStatusLine(t *testing.T) {
	e, press := newTestVim(t, "A=1\nB=2")
	press("jl")
	st := e.status.GetText(true) + e.pos.GetText(true)
	if !strings.Contains(st, "NORMAL") || !strings.Contains(st, "2:2") {
		t.Fatalf("status %q", st)
	}
	press("i")
	if st := e.status.GetText(true); !strings.Contains(st, "INSERT") {
		t.Fatalf("status %q", st)
	}
	press("x")
	if st := e.pos.GetText(true); !strings.Contains(st, "modified") {
		t.Fatalf("status %q", st)
	}
}

// Every footer names how to apply and how to leave.
func TestVimFooterKeys(t *testing.T) {
	for _, m := range []vimMode{vimNormal, vimInsert, vimCommand} {
		f := vimFooterKeys(m)
		if !strings.Contains(f, "apply") {
			t.Errorf("mode %d footer lacks apply: %q", m, f)
		}
	}
	if !strings.Contains(vimFooterKeys(vimNormal), "help") {
		t.Error("the Normal-mode footer does not mention ? help")
	}
}

// Undoing back to an empty text, and a multi-byte character, must not trip
// the cursor placement.
func TestVimUndoEdges(t *testing.T) {
	e, press := newTestVim(t, "")
	press("iä=ö<Esc>u")
	if e.ta.GetText() != "" {
		t.Fatalf("undo left %q", e.ta.GetText())
	}
	press("<C-r>")
	if e.ta.GetText() != "ä=ö" {
		t.Fatalf("redo gave %q", e.ta.GetText())
	}
	press("$x")
	if got := cur(e); got != "ä|=" {
		t.Fatalf("x on a multi-byte character: %q", got)
	}
}

// Esc typed quickly before a command arrives as one Alt-key event; it must
// still leave Insert mode and run the command.
func TestVimAltKeyIsEscapeThenKey(t *testing.T) {
	e, press := newTestVim(t, "A=1\nB=2")
	press("iX")
	e.ta.InputHandler()(tcell.NewEventKey(tcell.KeyRune, 'j', tcell.ModAlt), func(tview.Primitive) {})
	if e.mode != vimNormal {
		t.Fatal("Alt-j did not leave Insert mode")
	}
	if got := cur(e); got != "XA=1\n|B=2" {
		t.Fatalf("got %q", got)
	}
}
