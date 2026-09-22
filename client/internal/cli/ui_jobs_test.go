// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"swarmexec/client/internal/resolve"
)

// job builds a replicated job that has completed `completed` of `total` tasks.
func job(name string, completed, total, running int) resolve.Service {
	return resolve.Service{
		Name: name, Mode: resolve.ModeReplicatedJob,
		Running: running, Desired: 1, // what swarm reports: MaxConcurrent
		Completed: completed, JobTotal: total,
	}
}

// The bug this fixes: a job that ran to completion has nothing running, and the
// row said 0/1 in red — the display of a service that is down, for one that did
// exactly what it was asked to do.
func TestProgressCountShowsJobCompletions(t *testing.T) {
	if got := progressCount(job("backup", 1, 1, 0)); got != "1/1" {
		t.Errorf("finished job reads %q, want 1/1", got)
	}
	if got := progressCount(job("backup", 2, 5, 1)); got != "2/5" {
		t.Errorf("running job reads %q, want 2/5", got)
	}
	svc := resolve.Service{Name: "web", Mode: resolve.ModeReplicated, Running: 1, Desired: 3}
	if got := progressCount(svc); got != "1/3" {
		t.Errorf("service reads %q, want 1/3", got)
	}
}

func TestProgressColor(t *testing.T) {
	cases := []struct {
		name string
		svc  resolve.Service
		want tcell.Color
	}{
		{"finished job", job("backup", 1, 1, 0), tcell.ColorAqua},
		{"job in flight", job("backup", 2, 5, 1), tcell.ColorOrange},
		{"job started, none done", job("backup", 0, 5, 2), tcell.ColorOrange},
		{"job stuck at the gate", job("backup", 0, 5, 0), tcell.ColorRed},
		{"job asking for nothing", job("backup", 0, 0, 0), tcell.ColorGray},
		{"service down", resolve.Service{Mode: resolve.ModeReplicated, Running: 0, Desired: 3}, tcell.ColorRed},
		{"service healthy", resolve.Service{Mode: resolve.ModeReplicated, Running: 3, Desired: 3}, tcell.ColorAqua},
	}
	for _, c := range cases {
		if got := progressColor(c.svc); got != c.want {
			t.Errorf("%s: progressColor = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestServiceRowRendersAJobsCompletions(t *testing.T) {
	cols := svcColumns{name: 6, mode: 14, repl: 3}
	got := serviceRow(job("backup", 3, 3, 0), cols, "", "")
	if !strings.Contains(got, "3/3") {
		t.Errorf("serviceRow(job) = %q, want the completion count 3/3", got)
	}
	if strings.Contains(got, "0/1") {
		t.Errorf("serviceRow(job) = %q, still shows the live task count", got)
	}
}

// A stack with a finished job in it must not read as degraded forever. The
// stack row is what an operator scans first, and a nightly backup would have
// dragged it down every day between runs.
func TestGroupByStackCountsAJobsCompletions(t *testing.T) {
	svcs := []resolve.Service{
		svcIn("shop", "shop_web", 2, 2),
		func() resolve.Service { j := job("shop_backup", 1, 1, 0); j.Stack = "shop"; return j }(),
	}
	rows := groupByStack(svcs)
	if len(rows) != 1 {
		t.Fatalf("want one stack, got %+v", rows)
	}
	if rows[0].Running != 3 || rows[0].Desired != 3 {
		t.Errorf("stack tasks = %d/%d, want 3/3", rows[0].Running, rows[0].Desired)
	}
	if got := serviceColor(rows[0].Running, rows[0].Desired); got != tcell.ColorAqua {
		t.Errorf("stack colour = %v, want aqua", got)
	}
}
