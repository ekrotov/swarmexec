// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package logstream

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"swarmexec/client/internal/logfmt"
)

func TestBuildLogFilter(t *testing.T) {
	f, flt, err := BuildFilter("json", "warn", "timeout")
	if err != nil {
		t.Fatal(err)
	}
	if f.Name() != "json" || flt.MinLevel != logfmt.LevelWarn || flt.Grep == nil {
		t.Errorf("unexpected: format=%s level=%v grep=%v", f.Name(), flt.MinLevel, flt.Grep)
	}
	if _, _, err := BuildFilter("nope", "", ""); err == nil {
		t.Error("unknown format should error")
	}
	if _, _, err := BuildFilter("", "loud", ""); err == nil {
		t.Error("unknown level should error")
	}
	if _, _, err := BuildFilter("", "", "("); err == nil {
		t.Error("invalid regexp should error")
	}
}

func TestFilterWriter_JSONFilterAndRender(t *testing.T) {
	var out bytes.Buffer
	_, flt, _ := BuildFilter("json", "warn", "")
	w := NewFilterWriter(&out, logfmt.JSON, flt, RenderPlain)

	// Two lines: one INFO (dropped), one ERROR (kept, reformatted to LEVEL msg).
	w.Write([]byte(`{"level":"info","message":"starting"}` + "\n"))
	w.Write([]byte(`{"level":"error","message":"boom"}` + "\n"))
	got := out.String()
	if strings.Contains(got, "starting") {
		t.Errorf("INFO line should be filtered out, got %q", got)
	}
	if strings.TrimSpace(got) != "ERROR  boom" {
		t.Errorf("kept line render = %q, want %q", strings.TrimSpace(got), "ERROR  boom")
	}
}

func TestFilterWriter_PartialLineFlush(t *testing.T) {
	var out bytes.Buffer
	w := NewFilterWriter(&out, logfmt.Raw, logfmt.Filter{}, RenderPlain)
	w.Write([]byte("first\nno-newline-yet"))
	if got := out.String(); strings.TrimSpace(got) != "first" {
		t.Errorf("before flush = %q, want just 'first'", got)
	}
	w.Flush()
	if !strings.Contains(out.String(), "no-newline-yet") {
		t.Errorf("flush should emit the buffered partial line, got %q", out.String())
	}
}

var jsonLines = []string{
	`{"level":"info","msg":"listening"}`,
	`{"level":"warn","msg":"slow query"}`,
	`{"level":"error","msg":"upstream refused"}`,
}

func autoWriter(dst *bytes.Buffer) *FilterWriter {
	w := NewFilterWriter(dst, logfmt.DefaultFormat(), logfmt.Filter{}, RenderPlain)
	w.DetectFormat()
	return w
}

// `--log-format auto` on a JSON stream has to end up parsing JSON — including
// the first lines, which is where the error someone opened the log for usually
// is. So they are held, not printed twice in two shapes.
func TestFilterWriterDetectsAndReleasesTheHeldLines(t *testing.T) {
	var out bytes.Buffer
	w := autoWriter(&out)

	w.Write([]byte(jsonLines[0] + "\n"))
	if out.Len() != 0 {
		t.Fatalf("a line must be held while the format is unknown, got %q", out.String())
	}
	w.Write([]byte(jsonLines[1] + "\n" + jsonLines[2] + "\n"))

	if w.format.Name() != "json" {
		t.Fatalf("format = %q, want json", w.format.Name())
	}
	got := out.String()
	for _, want := range []string{"INFO  listening", "WARN  slow query", "ERROR  upstream refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q is missing %q — a held line was lost", got, want)
		}
	}
}

// A stream the detector cannot explain must not cost the log: at the end of the
// stream everything held comes out, parsed by the default.
func TestFilterWriterReleasesEverythingOnFlush(t *testing.T) {
	var out bytes.Buffer
	w := autoWriter(&out)
	w.Write([]byte("something entirely its own\n"))
	if out.Len() != 0 {
		t.Fatal("still undecided, so nothing should be out yet")
	}
	w.Flush()
	if got := out.String(); !strings.Contains(got, "something entirely its own") {
		t.Errorf("output = %q, want the held line released", got)
	}
	if w.auto {
		t.Error("the stream ended; there is nothing left to detect")
	}
}

// A quiet stream is the dangerous case: two lines a minute would otherwise sit
// in the buffer unseen. The wait bounds it in time as well as in lines.
func TestFilterWriterGivesUpAfterTheWait(t *testing.T) {
	old := autoHoldFor
	autoHoldFor = 20 * time.Millisecond
	defer func() { autoHoldFor = old }()

	var out bytes.Buffer
	w := autoWriter(&out)
	w.Write([]byte("a lonely line\n"))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		done := !w.auto
		w.mu.Unlock()
		if done {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := out.String(); !strings.Contains(got, "a lonely line") {
		t.Errorf("output = %q, want the line released after the wait", got)
	}
}

// Held lines must not pile up without bound on a stream nothing recognises.
func TestFilterWriterStopsHoldingAfterTheCap(t *testing.T) {
	var out bytes.Buffer
	w := autoWriter(&out)
	for i := 0; i < autoHoldLines; i++ {
		w.Write([]byte("10.0.0.5 - - \"GET / HTTP/1.1\" 200 2\n"))
	}
	if w.auto {
		t.Error("the cap must end the holding")
	}
	if n := strings.Count(out.String(), "\n"); n != autoHoldLines {
		t.Errorf("released %d lines, want %d", n, autoHoldLines)
	}
}

// auto is a format name wherever one is accepted, and it must not be mistaken
// for a typo.
func TestBuildLogFilterAcceptsAuto(t *testing.T) {
	f, _, err := BuildFilter("auto", "", "")
	if err != nil {
		t.Fatalf("auto rejected: %v", err)
	}
	if f.Name() != logfmt.DefaultFormat().Name() {
		t.Errorf("format = %q, want the default until the content decides", f.Name())
	}
	if !FormatAuto("  AUTO ") {
		t.Error("the name should be recognised regardless of case and padding")
	}
	if _, _, err := BuildFilter("jsno", "", ""); err == nil {
		t.Error("a typo must still be rejected")
	}
}
