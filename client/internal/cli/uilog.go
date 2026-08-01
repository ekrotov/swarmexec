// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/rivo/tview"

	"swarmexec/client/internal/logfmt"
)

// logBufferCap bounds how many raw lines a log view keeps so it can re-render
// when the operator changes the format or filter live.
const logBufferCap = 5000

// logViewHelp is the footer key hint shown under every log view. It lives in the
// footer (not the border title) so the log shortcuts are discoverable in the
// same place as every other tab's shortcuts, and each key spells out what it
// does rather than using a one-word label.
const logViewHelp = " [yellow]f[white] follow on/off  [yellow]F[white] cycle format (classic/json/gelf/raw)  [yellow]l[white] cycle min level  [yellow]/[white] filter message (text/regex)  [yellow]↑/↓[white] scroll  [yellow]Esc/q[white] close"

type logRow struct {
	prefix string // service view tag "[cid@node] "; "" for a single container
	line   string // raw log line
	stderr bool
}

// logViewer renders a format-aware, filtered log stream into a TextView and
// retains the raw lines, so switching format / min-level / grep re-renders the
// existing buffer live. Safe for concurrent Write goroutines.
type logViewer struct {
	app    *tview.Application
	tv     *tview.TextView
	follow *atomic.Bool

	mu     sync.Mutex
	rows   []logRow
	format logfmt.Format
	filter logfmt.Filter
}

func newLogViewer(app *tview.Application, tv *tview.TextView, follow *atomic.Bool, format logfmt.Format, filter logfmt.Filter) *logViewer {
	return &logViewer{app: app, tv: tv, follow: follow, format: format, filter: filter}
}

// addLines buffers a batch of raw lines and appends the ones that pass the
// current filter to the view in a single redraw. Called from stream goroutines
// once per network chunk — QueueUpdateDraw blocks until the main loop runs it
// and forces a full redraw, so doing it per line would starve keyboard input on
// a chatty container. Batching per chunk keeps the UI responsive.
func (v *logViewer) addLines(prefix string, lines []string, stderr bool) {
	if len(lines) == 0 {
		return
	}
	v.mu.Lock()
	var b bytes.Buffer
	for _, line := range lines {
		row := logRow{prefix: prefix, line: line, stderr: stderr}
		v.rows = append(v.rows, row)
		if v.filter.Match(v.format.Parse(line)) {
			b.WriteString(renderLogRow(v.format, row))
			b.WriteByte('\n')
		}
	}
	if len(v.rows) > logBufferCap {
		v.rows = v.rows[len(v.rows)-logBufferCap:]
	}
	out := b.String()
	v.mu.Unlock()
	if out == "" {
		return
	}
	v.app.QueueUpdateDraw(func() {
		fmt.Fprint(v.tv, out)
		if v.follow == nil || v.follow.Load() {
			v.tv.ScrollToEnd()
		}
	})
}

// rebuild re-renders the whole buffer through the current format and filter.
// It is only called from the key handlers (cycleFormat/cycleLevel/setGrep),
// which run on the main event goroutine, so it updates the TextView directly
// and lets tview redraw after the handler returns. It must NOT use
// QueueUpdateDraw: that blocks until the main loop runs it, and calling it from
// the main loop itself deadlocks — which is what froze the view on a format
// cycle.
func (v *logViewer) rebuild() {
	v.mu.Lock()
	var b bytes.Buffer
	for _, r := range v.rows {
		if v.filter.Match(v.format.Parse(r.line)) {
			b.WriteString(renderLogRow(v.format, r))
			b.WriteByte('\n')
		}
	}
	out := b.String()
	follow := v.follow == nil || v.follow.Load()
	v.mu.Unlock()
	v.tv.Clear()
	fmt.Fprint(v.tv, out)
	if follow {
		v.tv.ScrollToEnd()
	}
}

// cycleFormat advances to the next built-in format and re-renders.
func (v *logViewer) cycleFormat() {
	v.mu.Lock()
	fs := logfmt.Formats()
	idx := 0
	for i, f := range fs {
		if f.Name() == v.format.Name() {
			idx = i
			break
		}
	}
	v.format = fs[(idx+1)%len(fs)]
	v.mu.Unlock()
	v.rebuild()
}

var logLevelOrder = []logfmt.Level{
	logfmt.LevelUnknown, logfmt.LevelTrace, logfmt.LevelDebug, logfmt.LevelInfo,
	logfmt.LevelWarn, logfmt.LevelError, logfmt.LevelFatal,
}

// cycleLevel steps the min-level filter: off → trace → … → fatal → off.
func (v *logViewer) cycleLevel() {
	v.mu.Lock()
	idx := 0
	for i, l := range logLevelOrder {
		if l == v.filter.MinLevel {
			idx = i
			break
		}
	}
	v.filter.MinLevel = logLevelOrder[(idx+1)%len(logLevelOrder)]
	v.mu.Unlock()
	v.rebuild()
}

// setGrep sets or clears (nil) the message regexp and re-renders.
func (v *logViewer) setGrep(re *regexp.Regexp) {
	v.mu.Lock()
	v.filter.Grep = re
	v.mu.Unlock()
	v.rebuild()
}

// status is a compact "fmt:… lvl:… grep:…" string for the view title.
func (v *logViewer) status() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	lvl := "all"
	if v.filter.MinLevel != logfmt.LevelUnknown {
		lvl = v.filter.MinLevel.String()
	}
	grep := "off"
	if v.filter.Grep != nil {
		grep = v.filter.Grep.String()
	}
	return fmt.Sprintf("fmt:%s lvl:%s grep:%s", v.format.Name(), lvl, grep)
}

// logIngest splits streamed bytes into lines and feeds them to a logViewer.
// It replaces the raw byte-appending writers so lines can be parsed/filtered.
type logIngest struct {
	v      *logViewer
	prefix string
	stderr bool

	mu  sync.Mutex
	buf []byte
}

func (w *logIngest) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	var lines []string
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	w.mu.Unlock()
	w.v.addLines(w.prefix, lines, w.stderr)
	return len(p), nil
}

// renderLogRow produces the tview-coloured display line for a row under the
// given format. Structured formats show "LEVEL  message"; plain formats keep
// the raw line (stderr red, else tinted by level when one was detected).
func renderLogRow(f logfmt.Format, r logRow) string {
	e := f.Parse(r.line)
	prefix := ""
	if r.prefix != "" {
		prefix = "[gray]" + tview.Escape(r.prefix) + "[-]"
	}
	if f.Structured() {
		lvl := ""
		if s := e.Level.String(); s != "" {
			lvl = "[" + levelColor(e.Level) + "]" + s + "[-]  "
		}
		return prefix + lvl + tview.Escape(e.Message)
	}
	line := tview.Escape(e.Raw)
	switch {
	case r.stderr:
		return prefix + "[red]" + line + "[-]"
	case e.Level != logfmt.LevelUnknown:
		return prefix + "[" + levelColor(e.Level) + "]" + line + "[-]"
	default:
		return prefix + line
	}
}

func levelColor(l logfmt.Level) string {
	switch l {
	case logfmt.LevelError, logfmt.LevelFatal:
		return "red"
	case logfmt.LevelWarn:
		return "yellow"
	case logfmt.LevelDebug, logfmt.LevelTrace:
		return "gray"
	default:
		return "white"
	}
}
