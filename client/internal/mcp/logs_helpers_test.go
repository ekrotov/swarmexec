// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"errors"
	"strings"
	"testing"

	"swarmexec/client/internal/logstream"
	"swarmexec/client/internal/resolve"
)

// Unit tests for the steps toolLogs is built from.

func TestLogTail(t *testing.T) {
	for in, want := range map[int]int{0: mcpLogTailDefault, -3: mcpLogTailDefault, 7: 7, mcpLogTailMax: mcpLogTailMax, mcpLogTailMax + 1: mcpLogTailMax} {
		if got := logTail(in); got != want {
			t.Errorf("logTail(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestLogSince(t *testing.T) {
	if d, err := logSince(""); err != nil || d != 0 {
		t.Errorf("empty = %v, %v", d, err)
	}
	if d, err := logSince("90s"); err != nil || d.Seconds() != 90 {
		t.Errorf("90s = %v, %v", d, err)
	}
	for _, bad := range []string{"soon", "-1m"} {
		if _, err := logSince(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLogSources(t *testing.T) {
	var cands []resolve.Candidate
	for i := 1; i <= mcpLogMaxSources+5; i++ {
		cands = append(cands, resolve.Candidate{Service: "s", Slot: i, ContainerID: "c"})
	}
	src, note, err := logSources(cands, "s", 0)
	if err != nil || len(src) != mcpLogMaxSources || !strings.Contains(note, "showing 20 of 25 replicas") {
		t.Errorf("all slots: %d sources, note %q, err %v", len(src), note, err)
	}
	if src, note, err := logSources(cands, "s", 3); err != nil || len(src) != 1 || note != "" {
		t.Errorf("slot 3: %d sources, note %q, err %v", len(src), note, err)
	}
	if _, _, err := logSources(nil, "s", 0); err == nil || err.Error() != `service "s" has no running container` {
		t.Errorf("none: %v", err)
	}
	if _, _, err := logSources(cands, "s", 99); err == nil || !strings.Contains(err.Error(), "in slot 99") {
		t.Errorf("slot 99: %v", err)
	}
}

func TestLogOutput(t *testing.T) {
	full := &capWriter{max: 4}
	_, _ = full.Write([]byte("abcdefgh"))
	cases := []struct {
		name     string
		note     string
		buf      *capWriter
		timedOut bool
		serr     error
		want     string
	}{
		{"plain", "", bufWith("line\n"), false, nil, "line\n"},
		{"note first", "[n]\n", bufWith("line\n"), false, nil, "[n]\nline\n"},
		{"empty", "", bufWith(""), false, nil, "(no log lines matched)\n"},
		{"timed out", "", bufWith("x\n"), true, nil, "x\n\n[stopped after 30s — narrow with tail, since or grep]\n"},
		{"stream error", "", bufWith("x\n"), false, errors.New("agent down"), "x\n\n[agent down]\n"},
		{"empty with error", "", bufWith(""), false, errors.New("e"), "(no log lines matched)\n\n[e]\n"},
		{"truncated hides error and timeout", "", full, true, errors.New("e"), full.String() + truncationNote(mcpLogMaxBytes)},
	}
	for _, c := range cases {
		if got := logOutput(c.note, c.buf, c.timedOut, c.serr); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func bufWith(s string) *capWriter {
	b := &capWriter{max: 1 << 20}
	_, _ = b.Write([]byte(s))
	return b
}

// logFilterWrap passes the writer through untouched when nothing filters.
func TestLogFilterWrapPassThrough(t *testing.T) {
	format, filter, err := logstream.BuildFilter("raw", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	w, flush := logFilterWrap(format, filter, false)(&b)
	if w != &b {
		t.Error("an inactive filter must not wrap the writer")
	}
	flush()
}
