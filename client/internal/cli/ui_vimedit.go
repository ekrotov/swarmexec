// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// vimMode is the editor's modal state, as in vim: Normal moves and edits by
// command, Insert types text, Command collects a ":" or "/" line.
type vimMode int

const (
	vimNormal vimMode = iota
	vimInsert
	vimCommand
)

// lineError is an error that belongs to one line of an edited text. The vim
// editor moves the cursor to that line when a save fails with one.
type lineError struct {
	line int // 1-based
	msg  string
}

func (e *lineError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

// vimEditor is a modal text editor in the manner of vim, built on tview's
// TextArea. The TextArea keeps the text, the cursor, the scrolling and the undo
// history; the editor decides what a key means in the current mode and turns
// Normal-mode commands into cursor moves (Select) and edits (Replace, which the
// TextArea's own undo records).
//
// It is deliberately a subset — the commands that make a list of KEY=VALUE
// lines quick to edit — and everything it understands is listed in vimHelpText
// and, per mode, in the footer, so nobody has to know vim to use it.
//
// The editor owns no overlay. Its owner supplies what :w, :q and the rest do,
// and shows the help view.
type vimEditor struct {
	ta     *tview.TextArea
	status *tview.TextView // mode, command line or message
	pos    *tview.TextView // "modified  line:col", right-aligned
	root   *tview.Flex

	initial string
	mode    vimMode
	count   int    // pending count prefix (0: none)
	pending string // pending operator / prefix: "d", "y", "c", "g", "r", "Z"
	cmd     string // the command line, including its leading ':' or '/'
	search  string // the last search pattern, for n / N
	wantCol int    // sticky column for j / k; -1 sticks to the line end
	reg     string // the unnamed register
	regLine bool   // the register holds whole lines
	msg     string // one-shot message on the status line
	msgErr  bool
	forward bool // an event is being forwarded to the TextArea untouched

	// onWrite saves (:w, :wq, :x, ZZ, Ctrl-S). A *lineError moves the cursor
	// to its line; any error is shown on the status line.
	onWrite func(text string) error
	// onQuit leaves without saving (:q when nothing changed, :q!, ZQ).
	onQuit func()
	// onMode reports every mode change, for the footer.
	onMode func(vimMode)
	// onHelp opens the help view (? and :help).
	onHelp func()
	// commands are extra ":" commands the owner adds (e.g. "list", "r"),
	// keyed by name. arg is the rest of the line after the name (":r ~/a.pem"
	// runs "r" with "~/a.pem"); a command that takes none should refuse one.
	commands map[string]func(text, arg string) error
	// onTab, when set, receives Tab (back=false) and Shift-Tab (back=true) in
	// Normal mode, to move to a sibling field. Insert mode always types a tab.
	onTab func(back bool)
	// extraDirty adds the owner's own unsaved state (fields beside the text)
	// to dirty, so :q and Esc refuse to drop it silently.
	extraDirty func() bool
	// posInfo, when set, is shown before the cursor position on the right of
	// the status line (e.g. the size of the text).
	posInfo func(text string) string

	bar *tview.Flex
}

func newVimEditor(title, text string) *vimEditor {
	e := &vimEditor{initial: text}
	e.ta = tview.NewTextArea().SetText(text, false)
	e.ta.SetWrap(true).SetWordWrap(false)
	e.status = tview.NewTextView().SetDynamicColors(true)
	e.pos = tview.NewTextView().SetDynamicColors(true).SetTextAlign(tview.AlignRight)
	e.bar = tview.NewFlex().AddItem(e.status, 0, 1, false).AddItem(e.pos, 22, 0, false)
	e.root = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(e.ta, 0, 1, true).
		AddItem(e.bar, 1, 0, false)
	e.root.SetBorder(true).SetTitle(" " + tview.Escape(title) + " ")
	e.ta.SetInputCapture(e.capture)
	e.ta.SetMovedFunc(e.render)
	e.ta.SetChangedFunc(e.render)
	e.render()
	return e
}

// dirty reports whether the text differs from what the editor opened with, or
// the owner has unsaved state of its own.
func (e *vimEditor) dirty() bool {
	return e.ta.GetText() != e.initial || e.extraDirty != nil && e.extraDirty()
}

// cursor returns the cursor's byte offset into the text.
func (e *vimEditor) cursor() int {
	_, start, _ := e.ta.GetSelection()
	return start
}

func (e *vimEditor) moveTo(pos int) { e.ta.Select(pos, pos) }

func (e *vimEditor) setMode(m vimMode) {
	e.mode = m
	e.count, e.pending = 0, ""
	if m != vimCommand {
		e.cmd = ""
	}
	if e.onMode != nil {
		e.onMode(m)
	}
	e.render()
}

func (e *vimEditor) setMsg(msg string, isErr bool) {
	e.msg, e.msgErr = msg, isErr
	e.render()
}

// render redraws the status line: the mode (or the command being typed, or the
// last message) on the left, the cursor position on the right.
func (e *vimEditor) render() {
	text := e.ta.GetText()
	pos := e.cursor()
	ls, _ := lineBounds(text, pos)
	line := strings.Count(text[:pos], "\n") + 1
	col := utf8.RuneCountInString(text[ls:pos]) + 1
	left := ""
	switch {
	case e.mode == vimCommand:
		left = tview.Escape(e.cmd) + "[::r] [::-]"
	case e.msg != "" && e.msgErr:
		left = "[red::b]" + tview.Escape(e.msg) + "[-::-]"
	case e.msg != "":
		left = tview.Escape(e.msg)
	case e.mode == vimInsert:
		left = "[green::b]-- INSERT --[-::-]"
	default:
		left = "[::b]-- NORMAL --[::-]"
		if e.pending != "" || e.count > 0 {
			left += "  " + tview.Escape(e.pendingText())
		}
	}
	mod := ""
	if e.dirty() {
		mod = "[yellow]modified[-]  "
	}
	right := fmt.Sprintf("%s%d:%d ", mod, line, col)
	if e.posInfo != nil {
		right = e.posInfo(text) + "  " + right
		// Grow the right-hand cell to fit, so the size never pushes the
		// cursor position out of view.
		e.bar.ResizeItem(e.pos, max(22, tview.TaggedStringWidth(right)+1), 0)
	}
	e.pos.SetText(right)
	e.status.SetText(left)
}

func (e *vimEditor) pendingText() string {
	s := ""
	if e.count > 0 {
		s = strconv.Itoa(e.count)
	}
	return s + e.pending
}

// capture is the TextArea's input capture: it routes a key by mode. Returning
// the event lets the TextArea handle it (typing in Insert mode, paging).
func (e *vimEditor) capture(ev *tcell.EventKey) *tcell.EventKey {
	if e.forward {
		return ev
	}
	if ev.Key() == tcell.KeyCtrlS { // save from any mode, as the old editor did
		e.write()
		return nil
	}
	switch e.mode {
	case vimInsert:
		if ev.Key() == tcell.KeyEscape {
			e.leaveInsert()
			return nil
		}
		if ev.Key() == tcell.KeyRune && ev.Modifiers()&tcell.ModAlt != 0 {
			// A terminal sends Esc and a quickly following key as one
			// Alt-key; vim reads it as both, and so does this.
			e.leaveInsert()
			e.msg = ""
			e.normalKey(tcell.NewEventKey(tcell.KeyRune, ev.Rune(), tcell.ModNone))
			return nil
		}
		e.msg = ""
		return ev
	case vimCommand:
		e.commandKey(ev)
		return nil
	}
	e.msg = ""
	e.normalKey(ev)
	return nil
}

// send forwards a key to the TextArea's own handler, bypassing capture.
func (e *vimEditor) send(k tcell.Key) {
	e.forward = true
	defer func() { e.forward = false }()
	if h := e.ta.InputHandler(); h != nil {
		h(tcell.NewEventKey(k, 0, tcell.ModNone), func(tview.Primitive) {})
	}
}

// leaveInsert returns to Normal mode; like vim, the cursor steps back onto
// the character it was after.
func (e *vimEditor) leaveInsert() {
	text, pos := e.ta.GetText(), e.cursor()
	e.moveCol(text, prevInLine(text, pos)) // also makes it the column for j / k
	e.setMode(vimNormal)
}

// normalKey interprets one key in Normal mode: first the key a pending prefix
// waits for, then a count, then an operator's motion, and otherwise the
// command bound to the key in normalRunes / normalKeys.
func (e *vimEditor) normalKey(ev *tcell.EventKey) {
	r := ev.Rune()
	if ev.Key() != tcell.KeyRune {
		r = 0
	}
	if e.prefixKey(r) || e.countKey(r) {
		e.render()
		return
	}
	if op := e.pending; op == "d" || op == "y" || op == "c" {
		e.operator(op, ev, r)
		e.render()
		return
	}
	cmd := normalKeys[ev.Key()]
	if ev.Key() == tcell.KeyRune {
		cmd = normalRunes[r]
	}
	c := normalCtx{ev: ev, r: r, typed: e.count > 0, n: e.countOr(1)}
	e.count = 0
	c.text, c.pos = e.ta.GetText(), e.cursor()
	if cmd != nil {
		cmd(e, c)
	}
	e.render()
}

// prefixKey completes the prefixes that take the next key as their argument
// (r, g, Z); it reports whether one was pending. Like vim, r keeps a count
// typed before it, while g and Z use theirs up.
func (e *vimEditor) prefixKey(r rune) bool {
	switch e.pending {
	case "r":
		e.pending = ""
		if r != 0 {
			e.replaceChar(r)
		}
	case "g":
		e.pending = ""
		if r == 'g' {
			e.gotoLine(e.countOr(1))
		}
		e.count = 0
	case "Z":
		e.pending, e.count = "", 0
		switch r {
		case 'Z':
			e.write()
		case 'Q':
			e.quit(true)
		}
	default:
		return false
	}
	return true
}

// countKey adds a digit to the count prefix: 1-9 start it, 0 continues it (a
// bare 0 is a motion). It reports whether r was taken as one.
func (e *vimEditor) countKey(r rune) bool {
	if !(r >= '1' && r <= '9' || r == '0' && e.count > 0) {
		return false
	}
	e.count = min(e.count*10+int(r-'0'), 99999)
	return true
}

// normalCtx is what a Normal-mode command acts on: the key, the text and the
// cursor as they were when it was pressed, and its count — n is 1 when none
// was typed, and typed tells the two apart (G is not 1G).
type normalCtx struct {
	ev    *tcell.EventKey
	r     rune // 0 for a named key
	text  string
	pos   int
	n     int
	typed bool
}

// normalCmd is one Normal-mode command.
type normalCmd func(e *vimEditor, c normalCtx)

// normalRunes and normalKeys are the Normal-mode bindings: typed characters by
// rune, the rest by tcell key. A command with two spellings (h and Left) is
// listed under both. The prefixes only arm e.pending here; prefixKey and
// operator read the key that completes them. vimHelpText describes the same
// set for the operator — keep the two in step.
var (
	normalRunes = map[rune]normalCmd{
		'h': vimLeft, 'l': vimRight, ' ': vimRight,
		'j': vimDown, 'k': vimUp,
		'+': vimNextLine, '-': vimPrevLine,
		'0': vimLineStart, '^': func(e *vimEditor, _ normalCtx) { e.firstNonBlank() }, '$': vimLineEnd,
		'w': vimWordForward, 'b': vimWordBackward, 'e': vimWordEnd,
		'G': vimGoto,
		'g': vimPrefix, 'd': vimPrefix, 'y': vimPrefix, 'c': vimPrefix, 'r': vimPrefix, 'Z': vimPrefix,
		'i': func(e *vimEditor, _ normalCtx) { e.setMode(vimInsert) },
		'a': vimAppend, 'I': vimInsertLineStart, 'A': vimAppendLineEnd,
		'o': vimOpenBelow, 'O': vimOpenAbove,
		'x': vimDeleteChar, 'X': vimDeleteCharBefore,
		'D': vimDeleteToEnd, 'C': vimChangeToEnd,
		'S': func(e *vimEditor, c normalCtx) { e.changeLines(c.pos, c.n) },
		'Y': func(e *vimEditor, c normalCtx) { e.yankLines(c.pos, c.n) },
		'p': func(e *vimEditor, c normalCtx) { e.put(true, c.n) },
		'P': func(e *vimEditor, c normalCtx) { e.put(false, c.n) },
		'J': func(e *vimEditor, c normalCtx) { e.joinLines(c.pos, c.n) },
		'u': func(e *vimEditor, c normalCtx) { e.history(tcell.KeyCtrlZ, c.n) },
		'n': func(e *vimEditor, _ normalCtx) { e.find(e.search, true) },
		'N': func(e *vimEditor, _ normalCtx) { e.find(e.search, false) },
		':': vimCommandLine, '/': vimCommandLine,
		'?': vimHelp,
	}
	normalKeys = map[tcell.Key]normalCmd{
		tcell.KeyLeft: vimLeft, tcell.KeyBackspace: vimLeft, tcell.KeyBackspace2: vimLeft,
		tcell.KeyRight: vimRight,
		tcell.KeyDown:  vimDown, tcell.KeyUp: vimUp,
		tcell.KeyEnter: vimNextLine,
		tcell.KeyHome:  vimLineStart, tcell.KeyEnd: vimLineEnd,
		tcell.KeyCtrlD: vimPageDown, tcell.KeyPgDn: vimPageDown, tcell.KeyCtrlF: vimPageDown,
		tcell.KeyCtrlU: vimPageUp, tcell.KeyPgUp: vimPageUp, tcell.KeyCtrlB: vimPageUp,
		tcell.KeyDelete: vimDeleteChar,
		tcell.KeyCtrlR:  func(e *vimEditor, c normalCtx) { e.history(tcell.KeyCtrlY, c.n) },
		tcell.KeyTab:    vimTab, tcell.KeyBacktab: vimTab,
		tcell.KeyEscape: vimEscape,
	}
)

func vimLeft(e *vimEditor, c normalCtx) {
	pos := c.pos
	for i := 0; i < c.n; i++ {
		pos = prevInLine(c.text, pos)
	}
	e.moveCol(c.text, pos)
}

func vimRight(e *vimEditor, c normalCtx) {
	pos := c.pos
	for i := 0; i < c.n; i++ {
		pos = nextInLine(c.text, pos)
	}
	e.moveCol(c.text, pos)
}

func vimDown(e *vimEditor, c normalCtx) { e.moveLines(c.text, c.pos, c.n) }
func vimUp(e *vimEditor, c normalCtx)   { e.moveLines(c.text, c.pos, -c.n) }

func vimNextLine(e *vimEditor, c normalCtx) {
	e.moveLines(c.text, c.pos, c.n)
	e.firstNonBlank()
}

func vimPrevLine(e *vimEditor, c normalCtx) {
	e.moveLines(c.text, c.pos, -c.n)
	e.firstNonBlank()
}

func vimLineStart(e *vimEditor, c normalCtx) {
	ls, _ := lineBounds(c.text, c.pos)
	e.moveCol(c.text, ls)
}

func vimLineEnd(e *vimEditor, c normalCtx) {
	_, le := lineBounds(c.text, c.pos)
	e.moveTo(clampNormal(c.text, le))
	e.wantCol = -1
}

func vimWordForward(e *vimEditor, c normalCtx) {
	pos := c.pos
	for i := 0; i < c.n; i++ {
		pos = wordForward(c.text, pos)
	}
	e.moveCol(c.text, clampNormal(c.text, pos))
}

func vimWordBackward(e *vimEditor, c normalCtx) {
	pos := c.pos
	for i := 0; i < c.n; i++ {
		pos = wordBackward(c.text, pos)
	}
	e.moveCol(c.text, pos)
}

func vimWordEnd(e *vimEditor, c normalCtx) {
	pos := c.pos
	for i := 0; i < c.n; i++ {
		pos = wordEnd(c.text, pos)
	}
	e.moveCol(c.text, pos)
}

// vimGoto is G: the last line, or line n with a count.
func vimGoto(e *vimEditor, c normalCtx) {
	if c.typed {
		e.gotoLine(c.n)
		return
	}
	e.gotoLine(strings.Count(c.text, "\n") + 1)
}

// vimPrefix arms a prefix or operator; its count waits for it (3dd).
func vimPrefix(e *vimEditor, c normalCtx) {
	e.pending = string(c.r)
	if c.n > 1 {
		e.count = c.n
	}
}

func vimPageDown(e *vimEditor, _ normalCtx) { e.send(tcell.KeyPgDn) }
func vimPageUp(e *vimEditor, _ normalCtx)   { e.send(tcell.KeyPgUp) }

func vimAppend(e *vimEditor, c normalCtx) {
	_, le := lineBounds(c.text, c.pos)
	if c.pos < le {
		_, sz := utf8.DecodeRuneInString(c.text[c.pos:])
		e.moveTo(c.pos + sz)
	}
	e.setMode(vimInsert)
}

func vimInsertLineStart(e *vimEditor, _ normalCtx) {
	e.firstNonBlank()
	e.setMode(vimInsert)
}

func vimAppendLineEnd(e *vimEditor, c normalCtx) {
	_, le := lineBounds(c.text, c.pos)
	e.moveTo(le)
	e.setMode(vimInsert)
}

func vimOpenBelow(e *vimEditor, c normalCtx) {
	_, le := lineBounds(c.text, c.pos)
	e.ta.Replace(le, le, "\n")
	e.setMode(vimInsert)
}

func vimOpenAbove(e *vimEditor, c normalCtx) {
	ls, _ := lineBounds(c.text, c.pos)
	e.ta.Replace(ls, ls, "\n")
	e.moveTo(ls)
	e.setMode(vimInsert)
}

// vimDeleteChar is x: n characters, never past the line end.
func vimDeleteChar(e *vimEditor, c normalCtx) {
	_, le := lineBounds(c.text, c.pos)
	end := c.pos
	for i := 0; i < c.n && end < le; i++ {
		_, sz := utf8.DecodeRuneInString(c.text[end:])
		end += sz
	}
	e.cut(c.pos, end, false)
}

// vimDeleteCharBefore is X: n characters before the cursor, within the line.
func vimDeleteCharBefore(e *vimEditor, c normalCtx) {
	ls, _ := lineBounds(c.text, c.pos)
	start := c.pos
	for i := 0; i < c.n && start > ls; i++ {
		start = prevInLine(c.text, start)
	}
	e.cut(start, c.pos, false)
}

func vimDeleteToEnd(e *vimEditor, c normalCtx) {
	_, le := lineBounds(c.text, c.pos)
	e.cut(c.pos, le, false)
}

func vimChangeToEnd(e *vimEditor, c normalCtx) {
	_, le := lineBounds(c.text, c.pos)
	e.cut(c.pos, le, true)
	e.setMode(vimInsert)
}

func vimCommandLine(e *vimEditor, c normalCtx) {
	e.cmd = string(c.r)
	e.setMode(vimCommand)
}

func vimHelp(e *vimEditor, _ normalCtx) {
	if e.onHelp != nil {
		e.onHelp()
	}
}

func vimTab(e *vimEditor, c normalCtx) {
	if e.onTab != nil {
		e.onTab(c.ev.Key() == tcell.KeyBacktab)
	}
}

// vimEscape: Escape in Normal mode cancels a half-typed command (the prefix
// or operator takes it before this); with nothing pending and nothing changed
// it closes, as Esc does everywhere else in the TUI. With changes it says how
// to leave instead.
func vimEscape(e *vimEditor, _ normalCtx) {
	if !e.dirty() {
		e.quit(false)
		return
	}
	e.setMsg("unsaved changes — :w applies, :q! discards", false)
}

// history undoes (Ctrl-Z) or redoes (Ctrl-Y) n steps with the TextArea's own
// history and, like vim, puts the cursor where the text changed — the
// TextArea leaves it at the end of the restored text instead.
func (e *vimEditor) history(k tcell.Key, n int) {
	before := e.ta.GetText()
	for i := 0; i < n; i++ {
		e.send(k)
	}
	after := e.ta.GetText()
	if before == after {
		e.setMsg("already at the oldest / newest change", false)
		return
	}
	p := 0
	for p < len(before) && p < len(after) && before[p] == after[p] {
		p++
	}
	for p > 0 && p < len(after) && !utf8.RuneStart(after[p]) {
		p--
	}
	e.moveTo(clampNormal(after, p))
}

// countOr returns the pending count, or def when none was typed.
func (e *vimEditor) countOr(def int) int {
	if e.count > 0 {
		return e.count
	}
	return def
}

// operator completes d / y / c with its motion: the same letter repeated for
// whole lines (dd, yy, cc), w for to the next word, $ for to the line end.
func (e *vimEditor) operator(op string, ev *tcell.EventKey, r rune) {
	n := e.countOr(1)
	e.pending, e.count = "", 0
	text, pos := e.ta.GetText(), e.cursor()
	switch {
	case r == rune(op[0]):
		switch op {
		case "d":
			e.deleteLines(pos, n)
		case "y":
			e.yankLines(pos, n)
		case "c":
			e.changeLines(pos, n)
		}
	case r == 'w' || r == 'e':
		_, le := lineBounds(text, pos)
		end := pos
		for i := 0; i < n; i++ {
			if r == 'e' || op == "c" { // cw changes to the end of the word, as in vim
				end = wordEnd(text, end)
				if end < le {
					_, sz := utf8.DecodeRuneInString(text[end:])
					end += sz
				}
			} else {
				end = wordForward(text, end)
			}
		}
		end = min(end, le)
		e.applyRange(op, pos, end)
	case r == '$' || ev.Key() == tcell.KeyEnd:
		_, le := lineBounds(text, pos)
		e.applyRange(op, pos, le)
	case r == '0' || ev.Key() == tcell.KeyHome:
		ls, _ := lineBounds(text, pos)
		e.applyRange(op, ls, pos)
	}
}

// applyRange runs a charwise operator over [start,end).
func (e *vimEditor) applyRange(op string, start, end int) {
	switch op {
	case "y":
		e.reg, e.regLine = e.ta.GetText()[start:end], false
		e.moveTo(start)
	case "d":
		e.cut(start, end, false)
	case "c":
		e.cut(start, end, true)
		e.setMode(vimInsert)
	}
}

// cut deletes [start,end) into the register. keepEnd leaves the cursor where
// insertion continues (for c); otherwise it is clamped onto a character.
func (e *vimEditor) cut(start, end int, keepEnd bool) {
	if start >= end {
		return
	}
	text := e.ta.GetText()
	e.reg, e.regLine = text[start:end], false
	e.ta.Replace(start, end, "")
	if keepEnd {
		e.moveTo(start)
		return
	}
	e.moveTo(clampNormal(e.ta.GetText(), start))
}

// lineSpan returns the byte range of n whole lines starting at pos's line,
// including their trailing newline when there is one.
func lineSpan(text string, pos, n int) (start, end int) {
	start, end = lineBounds(text, pos)
	for i := 1; i < n && end < len(text); i++ {
		_, end = lineBounds(text, end+1)
	}
	if end < len(text) {
		end++ // the newline
	}
	return start, end
}

func (e *vimEditor) yankLines(pos, n int) {
	text := e.ta.GetText()
	s, end := lineSpan(text, pos, n)
	e.reg = strings.TrimSuffix(text[s:end], "\n") + "\n"
	e.regLine = true
	lines := strings.Count(e.reg, "\n")
	if lines > 1 {
		e.setMsg(fmt.Sprintf("%d lines yanked", lines), false)
	}
}

func (e *vimEditor) deleteLines(pos, n int) {
	text := e.ta.GetText()
	s, end := lineSpan(text, pos, n)
	e.reg = strings.TrimSuffix(text[s:end], "\n") + "\n"
	e.regLine = true
	if end == len(text) && !strings.HasSuffix(text[s:end], "\n") && s > 0 {
		s-- // the last line: take the newline before it instead
	}
	e.ta.Replace(s, end, "")
	text = e.ta.GetText()
	e.moveTo(min(s, len(text)))
	e.firstNonBlank()
}

func (e *vimEditor) changeLines(pos, n int) {
	text := e.ta.GetText()
	s, end := lineSpan(text, pos, n)
	if end > s && end <= len(text) && text[end-1] == '\n' {
		end-- // keep the line itself, empty
	}
	e.reg, e.regLine = text[s:end]+"\n", true
	e.ta.Replace(s, end, "")
	e.moveTo(s)
	e.setMode(vimInsert)
}

// put inserts the register after (p) or before (P) the cursor; whole lines go
// below / above the current line.
func (e *vimEditor) put(after bool, n int) {
	if e.reg == "" {
		return
	}
	text, pos := e.ta.GetText(), e.cursor()
	ins := strings.Repeat(e.reg, n)
	if e.regLine {
		ls, le := lineBounds(text, pos)
		if after {
			if le == len(text) {
				e.ta.Replace(le, le, "\n"+strings.TrimSuffix(ins, "\n"))
				e.moveTo(le + 1)
			} else {
				e.ta.Replace(le+1, le+1, ins)
				e.moveTo(le + 1)
			}
		} else {
			e.ta.Replace(ls, ls, ins)
			e.moveTo(ls)
		}
		e.firstNonBlank()
		return
	}
	at := pos
	if after {
		if _, le := lineBounds(text, pos); pos < le {
			_, sz := utf8.DecodeRuneInString(text[pos:])
			at += sz
		}
	}
	e.ta.Replace(at, at, ins)
	e.moveTo(clampNormal(e.ta.GetText(), at+len(ins)-1))
}

func (e *vimEditor) replaceChar(r rune) {
	text, pos := e.ta.GetText(), e.cursor()
	_, le := lineBounds(text, pos)
	if pos >= le {
		return
	}
	_, sz := utf8.DecodeRuneInString(text[pos:])
	e.ta.Replace(pos, pos+sz, string(r))
	e.moveTo(pos)
}

func (e *vimEditor) joinLines(pos, n int) {
	for i := 0; i < max(1, n-1); i++ {
		text := e.ta.GetText()
		_, le := lineBounds(text, pos)
		if le >= len(text) {
			return
		}
		next := le + 1
		for next < len(text) && (text[next] == ' ' || text[next] == '\t') {
			next++
		}
		e.ta.Replace(le, next, " ")
		e.moveTo(le)
	}
}

// moveCol moves to pos (clamped for Normal mode) and makes its column the
// sticky one for j / k.
func (e *vimEditor) moveCol(text string, pos int) {
	pos = clampNormal(text, pos)
	ls, _ := lineBounds(text, pos)
	e.wantCol = utf8.RuneCountInString(text[ls:pos])
	e.moveTo(pos)
}

// moveLines moves the cursor delta logical lines, keeping the sticky column.
func (e *vimEditor) moveLines(text string, pos, delta int) {
	ls, _ := lineBounds(text, pos)
	if e.wantCol >= 0 && e.wantCol < utf8.RuneCountInString(text[ls:pos]) {
		e.wantCol = utf8.RuneCountInString(text[ls:pos])
	}
	for ; delta > 0; delta-- {
		_, le := lineBounds(text, ls)
		if le >= len(text) {
			break
		}
		ls = le + 1
	}
	for ; delta < 0; delta++ {
		if ls == 0 {
			break
		}
		ls, _ = lineBounds(text, ls-1)
	}
	e.moveTo(atColumn(text, ls, e.wantCol))
}

func (e *vimEditor) firstNonBlank() {
	text, pos := e.ta.GetText(), e.cursor()
	ls, le := lineBounds(text, pos)
	p := ls
	for p < le && (text[p] == ' ' || text[p] == '\t') {
		p++
	}
	e.moveCol(text, p)
}

// gotoLine moves to the first non-blank of line n (1-based, clamped).
func (e *vimEditor) gotoLine(n int) {
	text := e.ta.GetText()
	pos := 0
	for i := 1; i < n; i++ {
		_, le := lineBounds(text, pos)
		if le >= len(text) {
			break
		}
		pos = le + 1
	}
	e.moveTo(pos)
	e.firstNonBlank()
}

// find searches for pat forward or backward from the cursor, wrapping around.
func (e *vimEditor) find(pat string, fwd bool) {
	if pat == "" {
		e.setMsg("no previous search", true)
		return
	}
	text, pos := e.ta.GetText(), e.cursor()
	var at int
	if fwd {
		from := min(pos+1, len(text))
		if i := strings.Index(text[from:], pat); i >= 0 {
			at = from + i
		} else if i := strings.Index(text, pat); i >= 0 {
			at = i
			e.setMsg("search hit BOTTOM, continuing at TOP", false)
		} else {
			e.setMsg("pattern not found: "+pat, true)
			return
		}
	} else {
		if i := strings.LastIndex(text[:pos], pat); i >= 0 {
			at = i
		} else if i := strings.LastIndex(text, pat); i >= 0 {
			at = i
			e.setMsg("search hit TOP, continuing at BOTTOM", false)
		} else {
			e.setMsg("pattern not found: "+pat, true)
			return
		}
	}
	e.moveCol(text, at)
}

// commandKey edits and runs the ":" / "/" command line.
func (e *vimEditor) commandKey(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEscape:
		e.setMode(vimNormal)
	case tcell.KeyEnter:
		cmd := e.cmd
		e.setMode(vimNormal)
		e.run(cmd)
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		_, sz := utf8.DecodeLastRuneInString(e.cmd)
		e.cmd = e.cmd[:len(e.cmd)-sz]
		if e.cmd == "" {
			e.setMode(vimNormal)
			return
		}
		e.render()
	case tcell.KeyRune:
		e.cmd += string(ev.Rune())
		e.render()
	}
}

// run executes a finished command line.
func (e *vimEditor) run(cmd string) {
	if strings.HasPrefix(cmd, "/") {
		if pat := cmd[1:]; pat != "" {
			e.search = pat
		}
		e.find(e.search, true)
		return
	}
	c := strings.TrimSpace(strings.TrimPrefix(cmd, ":"))
	if n, err := strconv.Atoi(c); err == nil {
		e.gotoLine(n)
		return
	}
	switch c {
	case "":
	case "w", "wq", "x", "w!", "wq!", "x!":
		e.write()
	case "q":
		e.quit(false)
	case "q!":
		e.quit(true)
	case "h", "help":
		if e.onHelp != nil {
			e.onHelp()
		}
	default:
		name, arg, _ := strings.Cut(c, " ")
		if fn, ok := e.commands[name]; ok {
			if err := fn(e.ta.GetText(), strings.TrimSpace(arg)); err != nil {
				e.showErr(err)
			}
			return
		}
		e.setMsg("not an editor command: "+c+"  (? lists the commands)", true)
	}
}

func (e *vimEditor) write() {
	if e.mode == vimInsert {
		e.leaveInsert()
	}
	if e.onWrite == nil {
		return
	}
	if err := e.onWrite(e.ta.GetText()); err != nil {
		e.showErr(err)
	}
}

// showErr puts err on the status line and, for a lineError, moves there.
func (e *vimEditor) showErr(err error) {
	var le *lineError
	if errors.As(err, &le) {
		e.gotoLine(le.line)
	}
	e.setMsg(err.Error(), true)
}

func (e *vimEditor) quit(force bool) {
	if !force && e.dirty() {
		e.setMsg("unsaved changes — :q! discards them, :w applies them", true)
		return
	}
	if e.onQuit != nil {
		e.onQuit()
	}
}

// insertAt inserts s before the cursor, as one undo step, and leaves the
// cursor on the first inserted character, where vim's :r leaves it too.
func (e *vimEditor) insertAt(s string) {
	pos := e.cursor()
	e.ta.Replace(pos, pos, s)
	e.moveCol(e.ta.GetText(), pos)
	e.render()
}

// --- text helpers (byte offsets into the whole text) ---

// lineBounds returns the start and end (the newline, or len) of pos's line.
func lineBounds(text string, pos int) (start, end int) {
	pos = max(0, min(pos, len(text)))
	start = strings.LastIndexByte(text[:pos], '\n') + 1
	if i := strings.IndexByte(text[pos:], '\n'); i >= 0 {
		return start, pos + i
	}
	return start, len(text)
}

// clampNormal keeps a Normal-mode cursor on a character: not past the last
// one of a non-empty line.
func clampNormal(text string, pos int) int {
	ls, le := lineBounds(text, pos)
	if pos >= le && le > ls {
		_, sz := utf8.DecodeLastRuneInString(text[:le])
		return le - sz
	}
	return pos
}

func prevInLine(text string, pos int) int {
	ls, _ := lineBounds(text, pos)
	if pos <= ls {
		return pos
	}
	_, sz := utf8.DecodeLastRuneInString(text[:pos])
	return pos - sz
}

func nextInLine(text string, pos int) int {
	_, le := lineBounds(text, pos)
	if pos >= le {
		return pos
	}
	_, sz := utf8.DecodeRuneInString(text[pos:])
	return pos + sz
}

// atColumn returns the offset of rune column col on the line starting at ls,
// clamped onto the line's last character (col < 0: the last character).
func atColumn(text string, ls, col int) int {
	_, le := lineBounds(text, ls)
	if col < 0 {
		return clampNormal(text, le)
	}
	p := ls
	for i := 0; i < col && p < le; i++ {
		_, sz := utf8.DecodeRuneInString(text[p:])
		p += sz
	}
	return clampNormal(text, p)
}

// runeClass sorts characters the way vim's word motions do: blanks, word
// characters, and everything else (punctuation such as = / : .).
func runeClass(r rune) int {
	switch {
	case unicode.IsSpace(r):
		return 0
	case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
		return 1
	default:
		return 2
	}
}

// wordForward returns the start of the next word (vim's w).
func wordForward(text string, pos int) int {
	if pos >= len(text) {
		return pos
	}
	r, sz := utf8.DecodeRuneInString(text[pos:])
	c := runeClass(r)
	p := pos + sz
	if c != 0 {
		for p < len(text) {
			r, sz = utf8.DecodeRuneInString(text[p:])
			if runeClass(r) != c {
				break
			}
			p += sz
		}
	}
	for p < len(text) {
		r, sz = utf8.DecodeRuneInString(text[p:])
		if r == '\n' && p+1 < len(text) && text[p+1] == '\n' {
			return p + 1 // an empty line is a word of its own
		}
		if runeClass(r) != 0 {
			break
		}
		p += sz
	}
	return p
}

// wordBackward returns the start of the previous word (vim's b).
func wordBackward(text string, pos int) int {
	p := pos
	for p > 0 {
		r, sz := utf8.DecodeLastRuneInString(text[:p])
		if runeClass(r) != 0 {
			break
		}
		p -= sz
	}
	if p == 0 {
		return 0
	}
	r, _ := utf8.DecodeLastRuneInString(text[:p])
	c := runeClass(r)
	for p > 0 {
		r, sz := utf8.DecodeLastRuneInString(text[:p])
		if runeClass(r) != c {
			break
		}
		p -= sz
	}
	return p
}

// wordEnd returns the last character of the current or next word (vim's e).
func wordEnd(text string, pos int) int {
	if pos >= len(text) {
		return pos
	}
	_, sz := utf8.DecodeRuneInString(text[pos:])
	p := pos + sz
	for p < len(text) {
		r, sz := utf8.DecodeRuneInString(text[p:])
		if runeClass(r) != 0 {
			break
		}
		p += sz
	}
	if p >= len(text) {
		return clampNormal(text, pos)
	}
	r, _ := utf8.DecodeRuneInString(text[p:])
	c := runeClass(r)
	for {
		_, sz := utf8.DecodeRuneInString(text[p:])
		if p+sz >= len(text) {
			return p
		}
		r, _ := utf8.DecodeRuneInString(text[p+sz:])
		if runeClass(r) != c {
			return p
		}
		p += sz
	}
}

// vimFooterKeys is the footer for each mode, so the commands that matter now
// are always on screen; ? opens the full list.
func vimFooterKeys(m vimMode) string {
	switch m {
	case vimInsert:
		return footerKeys("Esc", "normal mode", "Ctrl-S", "apply")
	case vimCommand:
		return footerKeys("Enter", "run", "Esc", "cancel", ":w", "apply", ":q", "cancel", ":q!", "discard")
	}
	return footerKeys("i", "insert", "o", "new line", "dd", "delete line", "x", "delete char",
		"u", "undo", "/", "search", ":w", "apply", ":q", "cancel", "?", "help")
}

// vimHelpText is the help view's text (dynamic-colour markup): everything the
// editor understands. extra lists the owner's own ":" commands and notes.
func vimHelpText(extra string) string {
	sec := func(t string) string { return "[yellow::b]" + t + "[-::-]\n" }
	row := func(k, d string) string { return fmt.Sprintf("  [aqua]%-15s[-] %s\n", tview.Escape(k), d) }
	var b strings.Builder
	b.WriteString("The editor works like vim: Normal mode moves and edits by command,\n")
	b.WriteString("Insert mode types text. It starts in Normal mode.\n\n")
	b.WriteString(sec("Modes"))
	b.WriteString(row("i  a", "insert before / after the cursor"))
	b.WriteString(row("I  A", "insert at the start / end of the line"))
	b.WriteString(row("o  O", "open a new line below / above"))
	b.WriteString(row("Esc", "back to Normal mode"))
	b.WriteString("\n" + sec("Move"))
	b.WriteString(row("h j k l", "left, down, up, right (arrow keys work too)"))
	b.WriteString(row("w  b  e", "next word, previous word, end of word"))
	b.WriteString(row("0  ^  $", "line start, first non-blank, line end"))
	b.WriteString(row("gg  G", "first line, last line (5G: line 5)"))
	b.WriteString(row("Ctrl-D  Ctrl-U", "page down / up"))
	b.WriteString(row("/text  n  N", "search, next match, previous match"))
	b.WriteString("\n" + sec("Edit"))
	b.WriteString(row("x  X", "delete the character under / before the cursor"))
	b.WriteString(row("dd  D", "delete the line, delete to the line end"))
	b.WriteString(row("dw  d$", "delete to the next word / the line end"))
	b.WriteString(row("cc  cw  C", "change the line / the word / to the line end"))
	b.WriteString(row("r<char>", "replace the character under the cursor"))
	b.WriteString(row("yy  p  P", "copy the line, paste below / above"))
	b.WriteString(row("J", "join the next line onto this one"))
	b.WriteString(row("u  Ctrl-R", "undo, redo"))
	b.WriteString(row("3dd  5j", "a count repeats a command"))
	b.WriteString("\n" + sec("Commands"))
	b.WriteString(row(":w", "apply (also :wq, :x, ZZ, Ctrl-S in any mode)"))
	b.WriteString(row(":q", "cancel — refused while there are changes"))
	b.WriteString(row(":q!", "cancel and discard the changes (also ZQ)"))
	b.WriteString(row(":42", "go to line 42"))
	b.WriteString(row(":help  ?", "this help"))
	if extra != "" {
		b.WriteString(extra)
	}
	return b.String()
}
