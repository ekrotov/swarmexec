// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

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
