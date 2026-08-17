// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"sync"

	"swarmexec/client/internal/logfmt"
)

// buildLogFilter resolves a format name, min-level and grep into a Format and
// Filter, validating each. An empty formatName yields the default format.
func buildLogFilter(formatName, minLevel, grep string) (logfmt.Format, logfmt.Filter, error) {
	format := logfmt.DefaultFormat()
	if formatName != "" {
		f, ok := logfmt.ByName(formatName)
		if !ok {
			return nil, logfmt.Filter{}, fmt.Errorf("unknown log format %q (want: classic, json, logfmt, gelf, raw)", formatName)
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
type filterWriter struct {
	dst    io.Writer
	format logfmt.Format
	filter logfmt.Filter
	render func(logfmt.Format, logfmt.Entry) string

	mu  sync.Mutex
	buf []byte
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
// newline). Call it once after the stream ends.
func (w *filterWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return
	}
	line := string(w.buf)
	w.buf = nil
	_ = w.emit(line)
}

func (w *filterWriter) emit(line string) error {
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
