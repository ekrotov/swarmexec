// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/swarmops"
	"swarmexec/internal/pb"
)

// The per-node prune dialog driven through its table (review Q-01 pinned it
// before showPruneDialog became a type): the rows and the summary under
// them, tick / all / age, and the prune itself against a fake agent.

// pruneAgent answers ReclaimStats with stats (Unimplemented when nil, like an
// agent older than the RPC) and records each Prune it is asked for.
type pruneAgent struct {
	pb.UnimplementedAgentServer
	stats *pb.ReclaimStatsResponse

	mu  sync.Mutex
	got []*pb.PruneRequest
}

func (a *pruneAgent) ReclaimStats(context.Context, *pb.ReclaimStatsRequest) (*pb.ReclaimStatsResponse, error) {
	if a.stats == nil {
		return nil, status.Error(codes.Unimplemented, "unknown method ReclaimStats")
	}
	return a.stats, nil
}

func (a *pruneAgent) Prune(_ context.Context, req *pb.PruneRequest) (*pb.PruneResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, req)
	out := &pb.PruneResponse{}
	for _, c := range req.GetCategories() {
		out.Results = append(out.Results, &pb.PruneCategoryResult{Category: c, Count: 1, FreedBytes: 1 << 20})
	}
	return out, nil
}

func (a *pruneAgent) requests() []*pb.PruneRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.got)
}

// pruneDialog opens the dialog for node-a (whose agent is a) through
// openNodePrune, as the Nodes tab does, and returns its table and summary.
func pruneDialog(t *testing.T, a *pruneAgent) (*ui, *tview.Table, *tview.TextView, *int) {
	t.Helper()
	u, _, _ := editUIWith(t, editSwarm(), map[string]pb.AgentServer{"node-a": a})
	afters := new(int)
	onLoop(t, u, func() {
		u.openNodePrune(swarmops.NodeInfo{Hostname: "node-a"}, u.ctree, func() { *afters++ })
	})
	waitFront(t, u, pageNodePrune)
	var table *tview.Table
	var summary *tview.TextView
	onLoop(t, u, func() {
		_, p := u.pages.GetFrontPage()
		table = findTable(p)
		if flex, ok := p.(*centeredBox).inner.(*tview.Flex); ok {
			summary, _ = flex.GetItem(1).(*tview.TextView)
		}
	})
	if table == nil || summary == nil {
		t.Fatal("the dialog has no table or no summary")
	}
	return u, table, summary, afters
}

// pruneRows is the dialog's rows as "box label" lines.
func pruneRows(t *testing.T, u *ui, table *tview.Table) []string {
	t.Helper()
	var rows []string
	onLoop(t, u, func() {
		for r := 0; r < table.GetRowCount(); r++ {
			rows = append(rows, table.GetCell(r, 0).Text+" "+table.GetCell(r, 1).Text+" | "+table.GetCell(r, 2).Text)
		}
	})
	return rows
}

func selectRow(t *testing.T, u *ui, table *tview.Table, row int) {
	t.Helper()
	onLoop(t, u, func() { table.Select(row, 0) })
}

func TestPruneDialogShowsRowsAndSummary(t *testing.T) {
	u, table, summary, _ := pruneDialog(t, &pruneAgent{stats: statsFixture()})
	rows := pruneRows(t, u, table)
	want := []string{
		"[x[] stopped containers (> 24h) |",
		"[ []  every unused image |",
		"[x[] untagged images |",
		"[x[] build cache |",
		"[x[] unused local networks |",
		"[ [] VOLUMES — data, never in all |",
	}
	if len(rows) != len(want) {
		t.Fatalf("rows:\n%s", strings.Join(rows, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(strings.ReplaceAll(rows[i], "  ", " "), strings.ReplaceAll(w, "  ", " ")) {
			t.Errorf("row %d = %q, want prefix %q", i, rows[i], w)
		}
	}
	onLoop(t, u, func() {
		if c, _, _ := table.GetCell(5, 1).Style.Decompose(); c != tcell.ColorRed {
			t.Errorf("the volumes row is %v, want red", c)
		}
	})
	if s := textOf(t, u, summary); !hasAll(s, "expected: frees about", "measured") || strings.Contains(s, "VOLUMES ticked") {
		t.Errorf("summary:\n%s", s)
	}
}

func TestPruneDialogKeys(t *testing.T) {
	u, table, summary, _ := pruneDialog(t, &pruneAgent{stats: statsFixture()})

	// Space ticks the row under the cursor: images, then volumes.
	selectRow(t, u, table, 1)
	keyRune(t, u, ' ')
	selectRow(t, u, table, 5)
	keyRune(t, u, ' ')
	rows := pruneRows(t, u, table)
	if !strings.HasPrefix(rows[1], "[x[]") || !strings.HasPrefix(rows[5], "[x[]") {
		t.Fatalf("space did not tick:\n%s", strings.Join(rows, "\n"))
	}
	if s := textOf(t, u, summary); !hasAll(s, "every unused image also removes", "VOLUMES ticked") {
		t.Errorf("summary with images and volumes:\n%s", s)
	}

	// a: with everything on it clears all but volumes; again, it ticks all
	// but volumes (and untagged images, which every unused image includes).
	keyRune(t, u, 'a')
	rows = pruneRows(t, u, table)
	for i, row := range rows {
		if on := strings.HasPrefix(row, "[x[]"); on != (i == 5) {
			t.Errorf("after a on all: %q", row)
		}
	}
	keyRune(t, u, 'a')
	rows = pruneRows(t, u, table)
	for i, row := range rows {
		if on := strings.HasPrefix(row, "[x[]"); on != (i != 2) {
			t.Errorf("after a again: %q", row)
		}
	}

	// t: the age prompt. A bad answer is refused, a good one relabels.
	keyRune(t, u, 't')
	waitFront(t, u, pageNodePrunePrompt)
	answer(t, u, "soon")
	waitFront(t, u, pageInfo)
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageNodePrune)
	keyRune(t, u, 't')
	waitFront(t, u, pageNodePrunePrompt)
	answer(t, u, "2h")
	waitFront(t, u, pageNodePrune)
	if rows := pruneRows(t, u, table); !strings.Contains(rows[0], "(> 2h)") {
		t.Errorf("after t 2h: %q", rows[0])
	}
	if s := textOf(t, u, summary); !strings.Contains(s, "measured for stopped > 24h") {
		t.Errorf("summary does not say the figure is for 24h:\n%s", s)
	}
	// Esc on the age prompt keeps the age.
	keyRune(t, u, 't')
	waitFront(t, u, pageNodePrunePrompt)
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageNodePrune)

	// An unbound key is passed on; Esc closes.
	keyRune(t, u, 'Z')
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageMain)
}

func TestPruneDialogPrunesAfterTheConfirm(t *testing.T) {
	a := &pruneAgent{stats: statsFixture()}
	u, _, _, afters := pruneDialog(t, a)
	keyRune(t, u, 'w')
	waitFront(t, u, pageConfirm)
	if text := drawFront(t, u, 240, 60); !hasAll(text, `Prune on "node-a"`, "build cache") {
		t.Errorf("confirm:\n%s", text)
	}
	// Cancel goes back to the dialog.
	keyCode(t, u, tcell.KeyEscape)
	waitFront(t, u, pageNodePrune)

	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageConfirm)
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageInfo)
	if text := drawFront(t, u, 240, 60); !hasAll(text, "stopped containers: 1 removed", "build cache: 1 removed") {
		t.Errorf("result:\n%s", text)
	}
	reqs := a.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d prune requests", len(reqs))
	}
	if got, want := reqs[0].GetCategories(), defaultPruneChoice().categories(); !slices.Equal(got, want) {
		t.Errorf("pruned %v, want %v", got, want)
	}
	if n := afterCount(t, u, afters); n != 1 {
		t.Errorf("after ran %d times", n)
	}
	if pageOpen(t, u, pageNodePrune) {
		t.Error("the dialog stayed open under the result")
	}
}

func TestPruneDialogNothingTickedAndNotMeasured(t *testing.T) {
	u, table, summary, _ := pruneDialog(t, &pruneAgent{stats: &pb.ReclaimStatsResponse{}})
	if s := textOf(t, u, summary); !strings.Contains(s, "not measured yet") {
		t.Errorf("summary without a measurement:\n%s", s)
	}
	for _, row := range []int{0, 2, 3, 4} {
		selectRow(t, u, table, row)
		keyRune(t, u, ' ')
	}
	keyCode(t, u, tcell.KeyEnter)
	waitFront(t, u, pageInfo)
	if text := drawFront(t, u, 240, 60); !strings.Contains(text, "Nothing ticked.") {
		t.Errorf("notice:\n%s", text)
	}
}

// An agent that predates ReclaimStats gets the old image menu — here the
// notice that no image figures are known for the node.
func TestNodePruneOnAnOldAgent(t *testing.T) {
	u, _, ring := editUIWith(t, editSwarm(), map[string]pb.AgentServer{"node-a": &pruneAgent{}})
	onLoop(t, u, func() { u.openNodePrune(swarmops.NodeInfo{Hostname: "node-a"}, u.ctree, nil) })
	waitForLog(t, ring, "ui notice", "No image information")

	// A node with no agent at all: the error is the notice.
	onLoop(t, u, func() { u.openNodePrune(swarmops.NodeInfo{Hostname: "node-b"}, u.ctree, nil) })
	waitForLog(t, ring, "ui notice", "prune: ")
}
