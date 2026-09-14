// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"testing"

	"github.com/rivo/tview"

	"swarmexec/client/internal/logfmt"
)

// rebuild runs on the MAIN event loop — every key that changes format, level or
// grep re-renders the whole ring there, so its cost is keyboard latency. It was
// ~230ms for a full buffer, which is what made those keys feel like the program
// had hung. These two pin the two cases apart.
func benchRows() []logRow {
	rows := make([]logRow, 0, logBufferCap)
	for i := 0; i < logBufferCap; i++ {
		rows = append(rows, logRow{
			line: fmt.Sprintf(`10.0.0.24 - - [14/Sep/2026:19:43:20 +0000] "GET /ocs/v2.php/apps/dashboard/api/v2/widget-items?w=%d HTTP/1.1" 200 1784 "-" "Mozilla/5.0"`, i),
		})
	}
	return rows
}

// Warm: a level or grep change keeps the format, so nothing needs reparsing.
// This is the common case, and the one that was worst to sit through.
func BenchmarkLogRebuildWarm(b *testing.B) {
	v := &logViewer{tv: tview.NewTextView().SetDynamicColors(true), rows: benchRows()}
	v.format, v.filter = logfmt.Formats()[0], logfmt.Filter{}
	v.rebuild()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v.rebuild()
	}
}

// Cold: a format change invalidates every row's cached parse and rendering.
func BenchmarkLogRebuildCold(b *testing.B) {
	v := &logViewer{tv: tview.NewTextView().SetDynamicColors(true), rows: benchRows()}
	v.filter = logfmt.Filter{}
	fs := logfmt.Formats()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v.format = fs[i%len(fs)]
		v.rebuild()
	}
}

// A row is parsed once per format, not once per pass — and certainly not twice
// per pass, which is what renderLogRow re-parsing made it.
func TestLogRowCachesPerFormat(t *testing.T) {
	r := logRow{line: `{"level":"warn","msg":"hello"}`}
	f := logfmt.JSON

	e1, s1 := r.resolve(f)
	if r.cachedFor != f.Name() {
		t.Fatalf("cachedFor = %q, want %q", r.cachedFor, f.Name())
	}
	// Corrupt the cache: a second resolve for the SAME format must return it
	// untouched, proving nothing was recomputed.
	r.rendered = "sentinel"
	_, s2 := r.resolve(f)
	if s2 != "sentinel" {
		t.Errorf("resolve recomputed for an unchanged format: %q", s2)
	}
	_ = e1
	_ = s1

	// A different format invalidates it.
	other := logfmt.Formats()[0]
	if other.Name() == f.Name() {
		other = logfmt.Formats()[1]
	}
	if _, s3 := r.resolve(other); s3 == "sentinel" {
		t.Error("a format change must invalidate the cached rendering")
	}
}

// addLines runs on stream goroutines and must never reach the application:
// going through QueueUpdateDraw once per network chunk is what starved the
// event loop. A nil app is the assertion — the old code would panic here.
func TestAddLinesNeverTouchesTheApp(t *testing.T) {
	v := &logViewer{app: nil, tv: tview.NewTextView(), format: logfmt.Formats()[0]}
	for i := 0; i < 200; i++ {
		v.addLines("", []string{fmt.Sprintf("line %d", i)}, false)
	}
	v.addNote("── reconnected ──")

	if v.pending.Len() == 0 {
		t.Fatal("output should be waiting for the flusher")
	}
	if got := v.tv.GetText(false); got != "" {
		t.Errorf("nothing may reach the widget before a flush, got %q", got)
	}
}
