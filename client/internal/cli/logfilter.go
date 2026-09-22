// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"swarmexec/client/internal/logfmt"
)

// logFormatAuto reports whether a configured format name asks for content
// detection rather than naming a format. It is answered separately from
// buildLogFilter because what "auto" resolves to is not known until lines have
// arrived: until then the stream is parsed by the default, exactly as before.
func logFormatAuto(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), logfmt.AutoName)
}

// buildLogFilter resolves a format name, min-level and grep into a Format and
// Filter, validating each. An empty formatName — or "auto", whose verdict comes
// later from the content — yields the default format.
func buildLogFilter(formatName, minLevel, grep string) (logfmt.Format, logfmt.Filter, error) {
	format := logfmt.DefaultFormat()
	if formatName != "" && !logFormatAuto(formatName) {
		f, ok := logfmt.ByName(formatName)
		if !ok {
			return nil, logfmt.Filter{}, fmt.Errorf("unknown log format %q (want: auto, classic, json, logfmt, gelf, raw)", formatName)
		}
		format = f
	}
	var filter logfmt.Filter
	if minLevel != "" {
		lvl := logfmt.ParseLevel(minLevel)
		if lvl == logfmt.LevelUnknown {
			return nil, logfmt.Filter{}, fmt.Errorf("unknown min-level %q (want: trace, debug, info, warn, error, fatal)", minLevel)
		}
		filter.MinLevel = lvl
	}
	if grep != "" {
		re, err := regexp.Compile(grep)
		if err != nil {
			return nil, logfmt.Filter{}, fmt.Errorf("invalid grep regexp: %w", err)
		}
		filter.Grep = re
	}
	return format, filter, nil
}

// filteringActive reports whether the format/filter would change the output, so
// the caller can skip wrapping the writer when it would be a no-op.
func filteringActive(format logfmt.Format, filter logfmt.Filter) bool {
	return format.Structured() || filter.MinLevel != logfmt.LevelUnknown || filter.Grep != nil
}

// filterWriter parses each complete line it receives with a log Format, drops
// the lines the Filter rejects, and writes the rendered survivors to dst. It
// buffers partial lines across writes. Shared by the `logs` command and the TUI
// log view.
// Auto-detection holds the first lines back until the content says what they
// are. Emitting them through the default format and switching afterwards would
// print one stream in two shapes — and the lines most worth reading correctly
// are the first ones, which is where the error that made someone open the log
// usually is.
const (
	// autoHoldLines is the most lines held while undecided. Reaching it means
	// the stream is not one the detector recognises; holding more would only
	// delay a log it is never going to explain.
	autoHoldLines = 40
)

// autoHoldFor bounds the wait in time as well as in lines, because a stream
// that goes quiet after two lines would otherwise hold them forever. A log
// nobody can see is worse than one parsed plainly. A var so a test can stop
// waiting a real second for it.
var autoHoldFor = time.Second

type filterWriter struct {
	dst    io.Writer
	format logfmt.Format
	filter logfmt.Filter
	render func(logfmt.Format, logfmt.Entry) string

	mu  sync.Mutex
	buf []byte

	// auto is set while the format is still being detected; held carries the
	// lines that arrived meanwhile, and timer bounds how long they wait.
	auto  bool
	held  []string
	timer *time.Timer
}

// detectFormat arms content detection: the writer holds its first lines, picks
// the format they are in, and emits everything through it.
func (w *filterWriter) detectFormat() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.auto = true
}

func newFilterWriter(dst io.Writer, f logfmt.Format, flt logfmt.Filter, render func(logfmt.Format, logfmt.Entry) string) *filterWriter {
	return &filterWriter{dst: dst, format: f, filter: flt, render: render}
}

func (w *filterWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		if err := w.emit(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// Flush renders any buffered partial line (a final line without a trailing
// newline) and releases anything detection is still holding. Call it once after
// the stream ends.
func (w *filterWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		line := string(w.buf)
		w.buf = nil
		_ = w.emit(line)
	}
	if w.auto {
		w.decideLocked()
	}
}

// emit renders one line, or holds it while the format is still being detected.
// Caller holds mu.
func (w *filterWriter) emit(line string) error {
	if w.auto {
		w.held = append(w.held, line)
		if _, ok := logfmt.Detect(w.held); ok || len(w.held) >= autoHoldLines {
			return w.decideLocked()
		}
		if w.timer == nil {
			w.timer = time.AfterFunc(autoHoldFor, w.decideAfterWait)
		}
		return nil
	}
	return w.write(line)
}

// decideLocked settles the format from whatever has arrived and releases the
// held lines. An undecided sample keeps the default, which is what the stream
// would have been parsed as anyway. Caller holds mu.
func (w *filterWriter) decideLocked() error {
	f, _ := logfmt.Detect(w.held)
	w.format = f
	w.auto = false
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	held := w.held
	w.held = nil
	for _, l := range held {
		if err := w.write(l); err != nil {
			return err
		}
	}
	return nil
}

func (w *filterWriter) decideAfterWait() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.auto {
		_ = w.decideLocked()
	}
}

func (w *filterWriter) write(line string) error {
	e := w.format.Parse(line)
	if !w.filter.Match(e) {
		return nil
	}
	_, err := io.WriteString(w.dst, w.render(w.format, e)+"\n")
	return err
}

// renderPlain is the pipe-safe renderer: a structured format collapses to
// "LEVEL  message" (or just the message when no level was detected); a plain
// format keeps the raw line unchanged.
func renderPlain(f logfmt.Format, e logfmt.Entry) string {
	if f.Structured() {
		if l := e.Level.String(); l != "" {
			return l + "  " + e.Message
		}
		return e.Message
	}
	return e.Raw
}
