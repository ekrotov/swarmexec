// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/moby/moby/api/types/swarm"
)

// The definition is the feature: each case is a state a pipeline can meet
// right after a deploy, with the verdict a pipeline must get for it.
func TestJudge(t *testing.T) {
	since := time.Now()
	before, after := since.Add(-time.Hour), since.Add(time.Minute)
	cases := []struct {
		name    string
		s       waitSnapshot
		healthy bool
		want    waitVerdict
		why     string
	}{
		{"all running", waitSnapshot{have: 3, want: 3}, false, waitConverged, "3/3 running"},
		{"still starting", waitSnapshot{have: 1, want: 3}, false, waitPending, "1/3 running"},
		{"stuck with a reason", waitSnapshot{have: 0, want: 2, taskErr: "no suitable node (scheduling constraints not satisfied)"},
			false, waitPending, "no suitable node"},
		{"update in flight", waitSnapshot{have: 3, want: 3, updateState: "updating"}, false, waitPending, "updating"},
		{"rollback in flight", waitSnapshot{have: 3, want: 3, updateState: "rollback_started"}, false, waitPending, "rollback started"},
		// Looks settled — 3/3 — but half the tasks are the old version.
		{"update paused", waitSnapshot{name: "web", have: 3, want: 3, updateState: "paused", updateMessage: "update paused due to failure"},
			false, waitFailed, "runs a mix of old and new tasks"},
		{"rollback paused", waitSnapshot{have: 3, want: 3, updateState: "rollback_paused"}, false, waitFailed, "rollback paused"},
		// Converged — to the PREVIOUS spec. The deploy failed.
		{"rolled back during the wait", waitSnapshot{have: 3, want: 3, updateState: "rollback_completed", updateDone: after},
			false, waitFailed, "rolled back during the wait"},
		// A rollback from last week is history; the service runs its spec.
		{"old rollback", waitSnapshot{have: 3, want: 3, updateState: "rollback_completed", updateDone: before},
			false, waitConverged, "3/3 running"},
		{"completed update", waitSnapshot{have: 2, want: 2, updateState: "completed"}, false, waitConverged, "2/2 running"},
		{"scaled to zero", waitSnapshot{have: 0, want: 0}, false, waitConverged, "scaled to 0"},
		{"job not done", waitSnapshot{isJob: true, have: 1, want: 3}, false, waitPending, "1/3 completed"},
		{"job done", waitSnapshot{isJob: true, have: 3, want: 3}, false, waitConverged, "3/3 completed"},
		{"missing", waitSnapshot{missing: true}, false, waitFailed, "no such service"},
		{"healthy", waitSnapshot{have: 2, want: 2, health: &serviceHealth{Healthy: 2}}, true, waitConverged, "2/2 running"},
		{"unhealthy", waitSnapshot{have: 2, want: 2, health: &serviceHealth{Healthy: 1, Unhealthy: 1}}, true, waitPending, "1 unhealthy"},
		{"health starting", waitSnapshot{have: 2, want: 2, health: &serviceHealth{Starting: 2}}, true, waitPending, "2 starting"},
		// The rule that keeps --healthy honest: no verdict is not a green one.
		{"health unknown", waitSnapshot{have: 2, want: 2, healthUnknown: []string{"node-3"}}, true, waitPending, "health unknown on node-3"},
		{"health not asked", waitSnapshot{have: 2, want: 2, healthUnknown: []string{"node-3"}}, false, waitConverged, "2/2 running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, why := judge(c.s, since, c.healthy)
			if v != c.want || !strings.Contains(why, c.why) {
				t.Errorf("judge = %v %q, want %v containing %q", v, why, c.want, c.why)
			}
		})
	}
}

type fakeTaskLister struct{ tasks []swarm.Task }

func (f fakeTaskLister) ServiceList(context.Context, client.ServiceListOptions) (client.ServiceListResult, error) {
	return client.ServiceListResult{}, nil
}
func (f fakeTaskLister) TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error) {
	return client.TaskListResult{Items: f.tasks}, nil
}

// The reason shown is the newest one, and a running task is never a reason.
func TestLatestTaskError(t *testing.T) {
	now := time.Now()
	task := func(state swarm.TaskState, desired swarm.TaskState, errMsg, msg string, at time.Time) swarm.Task {
		return swarm.Task{DesiredState: desired, Status: swarm.TaskStatus{State: state, Err: errMsg, Message: msg, Timestamp: at}}
	}
	l := fakeTaskLister{tasks: []swarm.Task{
		task(swarm.TaskStateRunning, swarm.TaskStateRunning, "", "started", now),
		task(swarm.TaskStateFailed, swarm.TaskStateShutdown, "task: non-zero exit (1)", "", now.Add(-time.Minute)),
		task(swarm.TaskStatePending, swarm.TaskStateRunning, "", "no suitable node (1 node not available)", now.Add(-time.Second)),
	}}
	if got := latestTaskError(context.Background(), l, "svc"); got != "no suitable node (1 node not available)" {
		t.Errorf("latest = %q", got)
	}
	if got := latestTaskError(context.Background(), fakeTaskLister{tasks: []swarm.Task{
		task(swarm.TaskStateRunning, swarm.TaskStateRunning, "", "started", now),
	}}, "svc"); got != "" {
		t.Errorf("a healthy service has no reason, got %q", got)
	}
}
