// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

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
//
// The follow hint names which of the two states is in force. A fixed "follow
// on/off" is ambiguous in the one situation that matters: a view that has
// stopped following looks exactly like a view whose container went quiet, and
// the footer — the thing an operator reads to find out — said the same either
// way. The mouse hint one field along already spells out its current mode for
// the same reason.
func logViewHelp(following bool) string {
	return " [yellow]f[white] follow " + followHint(following) +
		"  [yellow]F[white] cycle format (classic/json/logfmt/gelf/raw)  [yellow]l[white] cycle min level  [yellow]/[white] filter message (text/regex)  [yellow]↑/↓[white] scroll  [yellow]Esc/q[white] close"
}

// followHint marks the active half of the on/off pair. Both halves stay visible
// so the key still reads as a toggle; the active one is underlined AND coloured
// rather than only coloured, because this sits in a dense single-line footer
// where a colour change alone is easy to miss. Green for live, grey for paused —
// the same pairing the rest of the ui uses for "running" and "nothing going on".
// The closing tag turns the two flags off BY NAME ("BU") rather than resetting
// them with "-". They are not equivalent in tview: "-" restores the attribute
// MASK, and tcell keeps the underline outside that mask, so a "-" reset emitted
// "reset everything, then underline" and the rest of the footer came out
// underlined. Verified against the real terminal, which is the only place that
// shows.
func followHint(following bool) string {
	if following {
		return "[green::bu]on[white::BU]/off"
	}
	return "on/[gray::bu]off[white::BU]"
}

type logRow struct {
	prefix string // service view tag, e.g. "[slot 2] "; "" for a single container
	line   string // raw log line, or the note text when note is set
	stderr bool
	note   bool // a status line (e.g. a reconnect notice): always shown, never parsed

	// Parsing and rendering a line is expensive — a rebuild of a full ring cost
	// ~230ms on the MAIN event loop, which is what made a format or filter key
	// feel like the program had hung. Neither result depends on anything but the
	// line and the active format, so both are cached and only recomputed when
	// the format changes. A level or grep change then costs no parsing at all.
	entry     logfmt.Entry
	rendered  string
	cachedFor string // format name the two above belong to; "" = not yet computed
}

// resolve fills the row's cached parse and rendering for f, if they are not
// already there. Returns them.
func (r *logRow) resolve(f logfmt.Format) (logfmt.Entry, string) {
	if r.cachedFor != f.Name() {
		r.entry = f.Parse(r.line)
		r.rendered = renderLogRow(f, *r, r.entry)
		r.cachedFor = f.Name()
	}
	return r.entry, r.rendered
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

	// pending holds rendered output waiting for the next flush. Stream
	// goroutines write here; only the flusher touches the widget.
	pending bytes.Buffer
}

func newLogViewer(app *tview.Application, tv *tview.TextView, follow *atomic.Bool, format logfmt.Format, filter logfmt.Filter) *logViewer {
	// Cap the TextView's own buffer, not just our raw-line ring: without this the
	// TextView grows unbounded while streaming, so every redraw (and ScrollToEnd
	// while following) gets progressively slower and starves keyboard input — you
	// could no longer toggle follow on a long-running view. SetMaxLines trims old
	// rendered lines to match the ring cap, keeping redraws constant-time.
	tv.SetMaxLines(logBufferCap)
	return &logViewer{app: app, tv: tv, follow: follow, format: format, filter: filter}
}

// logFlushInterval bounds how often the view redraws while streaming.
//
// Batching per network chunk was not enough: a chatty container delivers many
// small chunks a second, every redraw goes through the main event loop — the
// same loop that handles keystrokes — and QueueUpdateDraw blocks until that
// loop runs it. The result was a UI so sluggish it looked hung. Ten redraws a
// second is faster than anyone reads and leaves the loop free for input.
const logFlushInterval = 100 * time.Millisecond

// pendingCap bounds the rendered output one flush can carry, so a pathological
// burst cannot hand the widget a huge string at once. Anything above the ring
// cap would be trimmed by SetMaxLines on arrival anyway.
const pendingCap = 1 << 20

// addLines buffers a batch of raw lines and queues the ones that pass the
// current filter for the next flush. Called from stream goroutines once per
// network chunk; it never touches the widget itself and never blocks on the
// main loop — see logFlushInterval for why that matters.
func (v *logViewer) addLines(prefix string, lines []string, stderr bool) {
	if len(lines) == 0 {
		return
	}
	v.mu.Lock()
	var b bytes.Buffer
	for _, line := range lines {
		row := logRow{prefix: prefix, line: line, stderr: stderr}
		entry, rendered := row.resolve(v.format)
		v.rows = append(v.rows, row)
		if v.filter.Match(entry) {
			b.WriteString(rendered)
			b.WriteByte('\n')
		}
	}
	if len(v.rows) > logBufferCap {
		v.rows = v.rows[len(v.rows)-logBufferCap:]
	}
	v.queueLocked(b.String())
	v.mu.Unlock()
}

// queueLocked appends rendered output to the pending buffer. Caller holds mu.
func (v *logViewer) queueLocked(out string) {
	if out == "" {
		return
	}
	v.pending.WriteString(out)
	if v.pending.Len() > pendingCap {
		// Keep the tail: on a burst this large the head is what SetMaxLines
		// would drop the moment it arrived.
		tail := v.pending.Bytes()[v.pending.Len()-pendingCap:]
		kept := make([]byte, len(tail))
		copy(kept, tail)
		v.pending.Reset()
		v.pending.Write(kept)
	}
}

// start runs the flusher until ctx ends. It is the only writer to the widget
// while streaming, which is what keeps the redraw rate bounded.
func (v *logViewer) start(ctx context.Context) {
	go func() {
		t := time.NewTicker(logFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				v.flush() // one last flush, so the tail is not lost on close
				return
			case <-t.C:
				v.flush()
			}
		}
	}()
}

func (v *logViewer) flush() {
	v.mu.Lock()
	if v.pending.Len() == 0 {
		v.mu.Unlock()
		return
	}
	out := v.pending.String()
	v.pending.Reset()
	v.mu.Unlock()
	v.app.QueueUpdateDraw(func() {
		fmt.Fprint(v.tv, out)
		if v.follow == nil || v.follow.Load() {
			v.tv.ScrollToEnd()
		}
	})
}

// addNote appends a dim status line (e.g. a reconnect notice). It is always
// shown regardless of the active filter and survives a re-render.
func (v *logViewer) addNote(text string) {
	v.mu.Lock()
	row := logRow{note: true, line: text}
	v.rows = append(v.rows, row)
	if len(v.rows) > logBufferCap {
		v.rows = v.rows[len(v.rows)-logBufferCap:]
	}
	// Through the same buffer as the lines, or a note could overtake output that
	// arrived before it.
	v.queueLocked(renderLogRow(v.format, row, logfmt.Entry{}) + "\n")
	v.mu.Unlock()
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
	for i := range v.rows {
		if v.rows[i].note {
			b.WriteString(renderLogRow(v.format, v.rows[i], logfmt.Entry{}))
			b.WriteByte('\n')
			continue
		}
		entry, rendered := v.rows[i].resolve(v.format)
		if v.filter.Match(entry) {
			b.WriteString(rendered)
			b.WriteByte('\n')
		}
	}
	out := b.String()
	follow := v.follow == nil || v.follow.Load()
	// rows already contains everything queued, so anything pending would be
	// appended a second time after this re-render.
	v.pending.Reset()
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
// e is the already-parsed entry; renderLogRow does not parse again. It used to,
// which meant every line was parsed TWICE on every pass — once for the filter
// and once here.
func renderLogRow(f logfmt.Format, r logRow, e logfmt.Entry) string {
	if r.note {
		return "[gray]" + tview.Escape(r.line) + "[-]"
	}
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
