// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package clientlog is the swarmexec client's logging + lightweight
// instrumentation layer. It keeps the most recent records in an in-memory ring
// buffer (fed to the interactive TUI's toggleable log viewer) and, optionally,
// mirrors them to a file. The TUI must never log to stdout/stderr — those belong
// to the tview screen — so the ring is the always-on sink and the file is opt-in.
package clientlog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SlowThreshold marks an instrumented operation as slow: at or above it, Timed
// logs a warning instead of a debug record, so sluggishness stands out in the
// viewer without needing debug level.
const SlowThreshold = 300 * time.Millisecond

// ringCap bounds the in-memory buffer feeding the TUI log viewer.
const ringCap = 2000

var (
	std  = slog.New(discardHandler{}) // until Init runs; a no-op so L() is always safe
	ring *Ring
)

// Ring is a bounded, concurrency-safe buffer of the most recent formatted log
// lines. It implements io.Writer so an slog text handler can feed it one line
// per record.
type Ring struct {
	mu    sync.Mutex
	lines []string
	cap   int
}

func newRing(capn int) *Ring { return &Ring{cap: capn} }

// Write appends one formatted log line (slog emits one record per Write).
func (r *Ring) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	r.mu.Lock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.cap {
		r.lines = r.lines[len(r.lines)-r.cap:]
	}
	r.mu.Unlock()
	return len(p), nil
}

// Lines returns a copy of the buffered lines, oldest first.
func (r *Ring) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// Init configures the global logger from a level string ("debug"|"info"|
// "warn"|"error") and an optional file path. It always installs the in-memory
// ring (returned for the TUI viewer); when filePath is non-empty it also appends
// to that file. Safe to call once at startup.
func Init(level, filePath string) (*Ring, error) {
	r := newRing(ringCap)
	writers := []io.Writer{r}
	if filePath != "" {
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		writers = append(writers, f)
	}
	h := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level:       parseLevel(level),
		ReplaceAttr: shortTime,
	})
	std = slog.New(h)
	ring = r
	return r, nil
}

// L returns the global logger — a no-op logger until Init runs, so callers never
// need a nil check.
func L() *slog.Logger { return std }

// Ring returns the in-memory buffer (nil until Init runs).
func RingBuffer() *Ring { return ring }

// DefaultLogPath mirrors the config file location: a swarmexec.log next to
// config.yaml. Callers pass it as the default for --log-file.
func DefaultLogPath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), "swarmexec.log")
}

// Timed records the outcome of an instrumented operation: an error logs at
// error level, a slow (>= SlowThreshold) success at warn, and a normal success
// at debug. start is the time.Now() captured before the operation.
func Timed(op string, start time.Time, err error, attrs ...any) {
	d := time.Since(start)
	args := append([]any{"op", op, "ms", d.Milliseconds()}, attrs...)
	switch {
	case err != nil:
		L().Error("op failed", append(args, "err", err.Error())...)
	case d >= SlowThreshold:
		L().Warn("slow op", args...)
	default:
		L().Debug("op done", args...)
	}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// shortTime trims the top-level time attribute to HH:MM:SS.mmm — the full date
// is noise in a live viewer and a per-session file.
func shortTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		if t, ok := a.Value.Any().(time.Time); ok {
			a.Value = slog.StringValue(t.Format("15:04:05.000"))
		}
	}
	return a
}

// discardHandler is the pre-Init no-op handler.
type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
