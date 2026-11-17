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

// parseKeys turns "dd<Esc>:w<CR>" into key events: runes, plus the named keys
// in special below (<Esc>, <CR>, <BS>, <C-r>, <Left>, …).
func parseKeys(t *testing.T, keys string) []*tcell.EventKey {
	t.Helper()
	special := map[string]tcell.Key{"<Esc>": tcell.KeyEscape, "<CR>": tcell.KeyEnter,
		"<BS>": tcell.KeyBackspace2, "<C-h>": tcell.KeyBackspace, "<C-r>": tcell.KeyCtrlR,
		"<C-s>": tcell.KeyCtrlS, "<Tab>": tcell.KeyTab, "<S-Tab>": tcell.KeyBacktab,
		"<Left>": tcell.KeyLeft, "<Right>": tcell.KeyRight, "<Up>": tcell.KeyUp,
		"<Down>": tcell.KeyDown, "<Home>": tcell.KeyHome, "<End>": tcell.KeyEnd,
		"<Del>": tcell.KeyDelete, "<C-d>": tcell.KeyCtrlD, "<C-u>": tcell.KeyCtrlU,
		"<C-f>": tcell.KeyCtrlF, "<C-b>": tcell.KeyCtrlB, "<PgDn>": tcell.KeyPgDn,
		"<PgUp>": tcell.KeyPgUp}
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
	// T-12: the apply idiom is Ctrl-S everywhere, but w stays vim's word
	// motion in Normal mode and plain text in Insert mode — it never writes.
	t.Run("w moves and types, never writes", func(t *testing.T) {
		e, press := newTestVim(t, "AB CD")
		wrote := false
		e.onWrite = func(string) error { wrote = true; return nil }
		press("w")
		if got := cur(e); got != "AB |CD" {
			t.Fatalf("Normal w moved to %q", got)
		}
		press("iw<Esc>")
		if wrote || e.ta.GetText() != "AB wCD" {
			t.Fatalf("Insert w: text %q, wrote %v", e.ta.GetText(), wrote)
		}
		if f := vimFooterKeys(vimNormal); !strings.Contains(f, "Ctrl-S") || !strings.Contains(f, "apply") {
			t.Fatalf("Normal footer does not name Ctrl-S apply: %q", f)
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
		e.commands = map[string]func(string, string) error{"list": func(s, _ string) error { ran = s; return nil }}
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

// Every Normal-mode binding, one row each: the key's spellings (rune and named
// key), counts and the multi-key prefixes.
func TestVimNormalKeys(t *testing.T) {
	const two = "KEY=one two\n  B=2\nC=3"
	cases := []struct {
		name, text, keys, want string
		mode                   vimMode
	}{
		// motions, in every spelling
		{"h", "ABC", "$h", "A|BC", vimNormal},
		{"Left", "ABC", "$<Left>", "A|BC", vimNormal},
		{"BS", "ABC", "$<BS>", "A|BC", vimNormal},
		{"C-h", "ABC", "$<C-h>", "A|BC", vimNormal},
		{"count h", "ABCD", "$2h", "A|BCD", vimNormal},
		{"l", "ABC", "l", "A|BC", vimNormal},
		{"space", "ABC", " ", "A|BC", vimNormal},
		{"Right", "ABC", "<Right>", "A|BC", vimNormal},
		{"l stops at the end", "ABC", "9l", "AB|C", vimNormal},
		{"j", "A\nB\nC", "j", "A\n|B\nC", vimNormal},
		{"Down", "A\nB\nC", "<Down>", "A\n|B\nC", vimNormal},
		{"count j", "A\nB\nC", "2j", "A\nB\n|C", vimNormal},
		{"k", "A\nB\nC", "Gk", "A\n|B\nC", vimNormal},
		{"Up", "A\nB\nC", "G<Up>", "A\n|B\nC", vimNormal},
		{"Enter", two, "<CR>", "KEY=one two\n  |B=2\nC=3", vimNormal},
		{"+", two, "+", "KEY=one two\n  |B=2\nC=3", vimNormal},
		{"-", two, "G-", "KEY=one two\n  |B=2\nC=3", vimNormal},
		{"0", "ABC", "$0", "|ABC", vimNormal},
		{"Home", "ABC", "$<Home>", "|ABC", vimNormal},
		{"^", "  AB", "$^", "  |AB", vimNormal},
		{"$", "ABC", "$", "AB|C", vimNormal},
		{"End", "ABC", "<End>", "AB|C", vimNormal},
		{"$ sticks to the end", "ABC\nABCDE", "$j", "ABC\nABCD|E", vimNormal},
		{"w", two, "w", "KEY|=one two\n  B=2\nC=3", vimNormal},
		{"count w", two, "3w", "KEY=one |two\n  B=2\nC=3", vimNormal},
		{"b", two, "$b", "KEY=one |two\n  B=2\nC=3", vimNormal},
		{"count b", two, "$2b", "KEY=|one two\n  B=2\nC=3", vimNormal},
		{"e", two, "e", "KE|Y=one two\n  B=2\nC=3", vimNormal},
		{"count e", two, "2e", "KEY|=one two\n  B=2\nC=3", vimNormal},
		{"G", two, "G", "KEY=one two\n  B=2\n|C=3", vimNormal},
		{"count G", two, "2G", "KEY=one two\n  |B=2\nC=3", vimNormal},
		{"gg", two, "Ggg", "|KEY=one two\n  B=2\nC=3", vimNormal},
		{"count gg", two, "2gg", "KEY=one two\n  |B=2\nC=3", vimNormal},
		{"g then other", two, "gjj", "KEY=one two\n|  B=2\nC=3", vimNormal},
		{"0 continues a count", "ABCDEFGHIJKL", "10l", "ABCDEFGHIJ|KL", vimNormal},
		// paging goes to the TextArea, which moves the cursor by a page
		{"C-d", "A\nB", "<C-d>", "A\nB|", vimNormal},
		{"PgDn", "A\nB", "<PgDn>", "A\nB|", vimNormal},
		{"C-f", "A\nB", "<C-f>", "A\nB|", vimNormal},
		{"C-u", "A\nB", "<C-u>", "|A\nB", vimNormal},
		{"PgUp", "A\nB", "<PgUp>", "|A\nB", vimNormal},
		{"C-b", "A\nB", "<C-b>", "|A\nB", vimNormal},
		// entering Insert mode
		{"i", "ABC", "li", "A|BC", vimInsert},
		{"a", "ABC", "la", "AB|C", vimInsert},
		{"a at the end", "ABC", "$a", "ABC|", vimInsert},
		{"I", "  AB", "$I", "  |AB", vimInsert},
		{"A", "AB\nC", "A", "AB|\nC", vimInsert},
		{"o", "AB\nC", "o", "AB\n|\nC", vimInsert},
		{"O", "AB\nC", "jO", "AB\n|\nC", vimInsert},
		{"S", "AB\nC", "S", "|\nC", vimInsert},
		{"count S", "AB\nC\nD", "2S", "|\nD", vimInsert},
		{"C", "ABC", "lC", "A|", vimInsert},
		// editing
		{"x", "ABC", "x", "|BC", vimNormal},
		{"Del", "ABC", "<Del>", "|BC", vimNormal},
		{"count x stops at the line end", "AB\nC", "5x", "|\nC", vimNormal},
		{"X", "ABC", "$X", "A|C", vimNormal},
		{"count X", "ABCD", "$2X", "A|D", vimNormal},
		{"D", "ABC", "lD", "|A", vimNormal},
		{"Y p", "A\nB", "Yp", "A\n|A\nB", vimNormal},
		{"count Y p", "A\nB\nC", "2YGp", "A\nB\nC\n|A\nB", vimNormal},
		{"count p", "A\nB", "yy2p", "A\n|A\nA\nB", vimNormal},
		{"charwise p", "AB", "xp", "B|A", vimNormal},
		{"charwise P", "AB", "xP", "|AB", vimNormal},
		{"p with an empty register", "AB", "p", "|AB", vimNormal},
		{"count J", "A\nB\nC", "3J", "A B| C", vimNormal},
		{"r", "ABC", "rX", "|XBC", vimNormal},
		{"r then a non-rune", "ABC", "r<Esc>", "|ABC", vimNormal},
		{"count u", "ABC", "xxx2u", "|BC", vimNormal},
		{"count C-r", "ABC", "xxx3u2<C-r>", "|C", vimNormal},
		// operators and their motions
		{"3dd keeps the count", "A\nB\nC\nD", "3dd", "|D", vimNormal},
		{"d3d", "A\nB\nC\nD", "d3d", "|D", vimNormal},
		{"de", two, "de", "|=one two\n  B=2\nC=3", vimNormal},
		{"2dw", two, "2dw", "|one two\n  B=2\nC=3", vimNormal},
		{"dEnd", "ABC", "ld<End>", "|A", vimNormal},
		{"d0", "ABC", "$d0", "|C", vimNormal},
		{"dHome", "ABC", "$d<Home>", "|C", vimNormal},
		{"y$ P", "ABC", "ly$P", "AB|CBC", vimNormal},
		{"yw P", two, "ywP", "KE|YKEY=one two\n  B=2\nC=3", vimNormal},
		{"c$", "ABC", "lc$", "A|", vimInsert},
		{"d then nothing", "ABC", "dj", "|ABC", vimNormal},
		// search
		{"N wraps", "A=1\nB=1\nC=1", "/=1<CR>N", "A=1\nB=1\nC|=1", vimNormal},
		{"n without a search", "ABC", "ln", "A|BC", vimNormal},
		// modes and stray keys
		{":", "ABC", ":", "|ABC", vimCommand},
		{"/", "ABC", "/", "|ABC", vimCommand},
		{"unbound rune", "ABC", "lq", "A|BC", vimNormal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, press := newTestVim(t, c.text)
			press(c.keys)
			if got := cur(e); got != c.want {
				t.Fatalf("%q on %q: got %q, want %q", c.keys, c.text, got, c.want)
			}
			if e.mode != c.mode {
				t.Fatalf("ended in mode %d, want %d", e.mode, c.mode)
			}
			if c.mode == vimNormal && (e.pending != "" || e.count != 0) {
				t.Fatalf("left pending %q count %d", e.pending, e.count)
			}
		})
	}
}

// The Normal-mode keys that reach the owner: ZZ writes, ZQ leaves without
// asking, Tab / Shift-Tab move between fields; and a count is capped.
func TestVimNormalKeysReachTheOwner(t *testing.T) {
	e, press := newTestVim(t, "A=1")
	var wrote string
	quit, tabs, backs := 0, 0, 0
	e.onWrite = func(s string) error { wrote = s; return nil }
	e.onQuit = func() { quit++ }
	e.onTab = func(back bool) {
		if back {
			backs++
		} else {
			tabs++
		}
	}
	press("xZZ")
	if wrote != "=1" {
		t.Fatalf("ZZ wrote %q", wrote)
	}
	press("ZX") // not a Z command: nothing happens, the prefix is gone
	if quit != 0 || e.pending != "" {
		t.Fatalf("ZX: quit %d, pending %q", quit, e.pending)
	}
	press("xZQ") // ZQ leaves even with changes
	if quit != 1 {
		t.Fatal("ZQ did not leave")
	}
	press("<Tab><S-Tab><Tab>")
	if tabs != 2 || backs != 1 {
		t.Fatalf("Tab %d, Shift-Tab %d", tabs, backs)
	}
	press("9999999")
	if e.count != 99999 {
		t.Fatalf("count %d, want it capped at 99999", e.count)
	}
	if !strings.Contains(e.status.GetText(true), "99999") {
		t.Fatalf("the pending count is not shown: %q", e.status.GetText(true))
	}
}
