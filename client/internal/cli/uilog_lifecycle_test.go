// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/rivo/tview"

	"swarmexec/client/internal/logfmt"
)

// A stream failure used to be printed straight into the TextView, outside the
// row buffer, so the next format, level or filter key (which re-renders from
// the buffer) erased the one line that said why the view had stopped.
func TestLogViewerErrorSurvivesRebuild(t *testing.T) {
	v := &logViewer{tv: tview.NewTextView().SetDynamicColors(true)}
	v.format, v.filter = logfmt.Formats()[0], logfmt.Filter{}
	v.addError("error: connection reset")
	v.rebuild()
	if got := v.tv.GetText(true); !strings.Contains(got, "connection reset") {
		t.Errorf("the stream error did not survive a rebuild: %q", got)
	}
}

// A container that never prints a newline must not grow the ingest buffer
// without bound.
func TestLogIngestBoundsAPartialLine(t *testing.T) {
	v := &logViewer{tv: tview.NewTextView().SetDynamicColors(true)}
	v.format, v.filter = logfmt.Formats()[0], logfmt.Filter{}
	w := &logIngest{v: v}
	chunk := []byte(strings.Repeat("x", 16<<10))
	for i := 0; i < 20; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
		w.mu.Lock()
		n := len(w.buf)
		w.mu.Unlock()
		if n > maxPartialLine {
			t.Fatalf("partial-line buffer reached %d bytes, cap %d", n, maxPartialLine)
		}
	}
	v.mu.Lock()
	rows := len(v.rows)
	v.mu.Unlock()
	if rows == 0 {
		t.Error("the overlong text was dropped instead of shown")
	}
}
