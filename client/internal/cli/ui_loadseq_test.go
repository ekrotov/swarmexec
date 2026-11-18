// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/rivo/tview"
)

func TestLoadSeqDropsOlderAnswersOnly(t *testing.T) {
	var s loadSeq
	a, b, c := s.start(), s.start(), s.start()
	if !s.fresh(b) {
		t.Fatal("the first answer to arrive must apply, even if it is not the newest ticket")
	}
	if s.fresh(a) {
		t.Error("an answer older than one already applied was applied")
	}
	if s.latest(c) || !s.latest(b) {
		t.Error("latest should name the newest applied ticket")
	}
	if !s.fresh(c) {
		t.Error("the newest answer was refused")
	}
}

// Two loads in flight, the newer one answering first: the older, slower answer
// must not paint over it.
func TestListTabLoadIgnoresAnOlderSlowAnswer(t *testing.T) {
	u := runningUI(t)
	l := newListTab[string](u, []string{"NAME"}, func(s string) string { return s })
	l.row = func(r int, it string) { l.table.SetCell(r, 0, tview.NewTableCell(it)) }
	l.empty = func() string { return "none" }
	slow, fast := make(chan struct{}), make(chan struct{})
	fetch := func(gate chan struct{}, rows ...string) func(context.Context) ([]string, error) {
		return func(context.Context) ([]string, error) { <-gate; return rows, nil }
	}
	onLoop(t, u, func() {
		l.load("test.old", fetch(slow, "old"))
		l.load("test.new", fetch(fast, "new"))
	})
	close(fast)
	waitRows(t, u, l, []string{"new"})
	close(slow)
	time.Sleep(200 * time.Millisecond) // let the stale answer reach the loop, if it is going to
	waitRows(t, u, l, []string{"new"})
}

func waitRows(t *testing.T, u *ui, l *listTab[string], want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got []string
		onLoop(t, u, func() { got = slices.Clone(l.st().rows) })
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rows = %q, want %q", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
