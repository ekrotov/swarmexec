// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rivo/tview"

	"swarmexec/client/internal/logfmt"
)

func jsonViewer(lines ...string) *logViewer {
	v := &logViewer{tv: tview.NewTextView().SetDynamicColors(true), format: logfmt.DefaultFormat()}
	v.addLines("", lines, false)
	return v
}

var jsonLines = []string{
	`{"level":"info","msg":"listening"}`,
	`{"level":"warn","msg":"slow query"}`,
	`{"level":"error","msg":"upstream refused"}`,
}

// The point of the key: five formats, and the output knows which one it is
// better than the operator does.
func TestAutoDetectNowPicksTheFormat(t *testing.T) {
	v := jsonViewer(jsonLines...)
	msg := v.autoDetectNow()

	if v.format.Name() != "json" {
		t.Fatalf("format = %q, want json", v.format.Name())
	}
	if v.auto {
		t.Error("a verdict must end the guessing")
	}
	if !strings.Contains(msg, "json") {
		t.Errorf("message = %q, want it to name the format", msg)
	}
	// The verdict is recorded in the buffer, so the line that explains why the
	// view suddenly looks different scrolls with the output it belongs to.
	last := v.rows[len(v.rows)-1]
	if !last.note || !strings.Contains(last.line, "json") {
		t.Errorf("last row = %+v, want a note naming the format", last)
	}
	// And the buffer is re-rendered through it: structured output, not raw JSON.
	if got := v.tv.GetText(true); !strings.Contains(got, "upstream refused") || strings.Contains(got, `"msg"`) {
		t.Errorf("view was not re-rendered through the new format:\n%s", got)
	}
}

// Too little to go on must say so and keep watching, not guess. A wrong format
// is worse than none: --min-level on a stream parsed wrongly filters nothing.
func TestAutoDetectNowStaysArmedWithoutAVerdict(t *testing.T) {
	v := jsonViewer(`10.0.0.5 - - [22/Sep/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 2`)
	msg := v.autoDetectNow()

	if !v.auto {
		t.Error("without a verdict the view must keep watching")
	}
	if v.format.Name() != logfmt.DefaultFormat().Name() {
		t.Errorf("format = %q, want it unchanged", v.format.Name())
	}
	if !strings.Contains(msg, "cannot tell") {
		t.Errorf("message = %q, want it to admit the uncertainty", msg)
	}
}

// The notes are ours — reconnect notices, container events. Feeding them to the
// detector would have it classify swarmexec instead of the container.
func TestAutoDetectIgnoresOurOwnNotes(t *testing.T) {
	v := jsonViewer()
	for i := 0; i < 10; i++ {
		v.addNote("── reconnected to a new container ──")
	}
	v.addLines("", jsonLines, false)

	if got := v.sampleLocked(); len(got) != len(jsonLines) {
		t.Fatalf("sample = %v, want only the container's own lines", got)
	}
	if v.autoDetectNow(); v.format.Name() != "json" {
		t.Errorf("format = %q, want json", v.format.Name())
	}
}

// An explicit choice outranks a pending guess: nothing is more irritating than
// a format that changes back under a cursor that just set it.
func TestCycleFormatDisarmsAutoDetect(t *testing.T) {
	v := jsonViewer(jsonLines...)
	v.armAutoDetect()
	v.cycleFormat()
	if v.auto {
		t.Error("cycling the format must end auto-detection")
	}
}

// While undecided the title has to say so, or "auto" and "classic" look the
// same on screen.
func TestStatusNamesTheUndecidedState(t *testing.T) {
	v := jsonViewer()
	v.armAutoDetect()
	if got := v.status(); !strings.Contains(got, "fmt:auto→classic") {
		t.Errorf("status = %q, want the pending detection named", got)
	}
	v.addLines("", jsonLines, false)
	v.autoDetectNow()
	if got := v.status(); !strings.Contains(got, "fmt:json") || strings.Contains(got, "auto") {
		t.Errorf("status = %q, want the settled format alone", got)
	}
}

// --- the pipe side -------------------------------------------------------

func autoWriter(dst *bytes.Buffer) *filterWriter {
	w := newFilterWriter(dst, logfmt.DefaultFormat(), logfmt.Filter{}, renderPlain)
	w.detectFormat()
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
	f, _, err := buildLogFilter("auto", "", "")
	if err != nil {
		t.Fatalf("auto rejected: %v", err)
	}
	if f.Name() != logfmt.DefaultFormat().Name() {
		t.Errorf("format = %q, want the default until the content decides", f.Name())
	}
	if !logFormatAuto("  AUTO ") {
		t.Error("the name should be recognised regardless of case and padding")
	}
	if _, _, err := buildLogFilter("jsno", "", ""); err == nil {
		t.Error("a typo must still be rejected")
	}
}

// A full sample it cannot explain is an answer too. Without it, a view nobody
// can classify would rescan its whole buffer ten times a second for as long as
// it stays open.
func TestAutoDetectGivesUpOnAFullSample(t *testing.T) {
	v := jsonViewer()
	acc := make([]string, 0, logfmt.DetectSample)
	for i := 0; i < logfmt.DetectSample; i++ {
		acc = append(acc, `10.0.0.5 - - [22/Sep/2026:10:00:00 +0000] "GET / HTTP/1.1" 200 2`)
	}
	v.addLines("", acc, false)

	msg := v.autoDetectNow()
	if v.auto {
		t.Error("a full sample must end the rescanning")
	}
	if v.format.Name() != logfmt.DefaultFormat().Name() {
		t.Errorf("format = %q, want the default kept", v.format.Name())
	}
	if !strings.Contains(msg, "could not tell") {
		t.Errorf("message = %q, want it to say the detector failed", msg)
	}
}
