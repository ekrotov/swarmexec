// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

// Tests for the pieces split out in Q-01 round 5 that the end-to-end tests
// reach only on their happy path.

// The version picker's autocomplete: substring matches in order, everything
// for an empty field, never more than versionSuggestionLimit.
func TestVersionSuggestions(t *testing.T) {
	from := []string{"2.1.0", "2.0.1", "1.9.0"}
	if got := strings.Join(versionSuggestions(from, " 2. "), ","); got != "2.1.0,2.0.1" {
		t.Errorf("filtered = %s", got)
	}
	if got := len(versionSuggestions(from, "")); got != 3 {
		t.Errorf("empty field offers %d, want all 3", got)
	}
	many := make([]string, versionSuggestionLimit+10)
	for i := range many {
		many[i] = "1." + strings.Repeat("0", i%3)
	}
	if got := len(versionSuggestions(many, "")); got != versionSuggestionLimit {
		t.Errorf("offered %d, want the limit %d", got, versionSuggestionLimit)
	}
}

// A device is not a file to read a secret from.
func TestReadSecretFileRefusesDevice(t *testing.T) {
	if _, err := readSecretFile("/dev/null", 0); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v, want not a regular file", err)
	}
}

func TestRolloutLine(t *testing.T) {
	task := func(desired, state swarm.TaskState, errMsg, msg string) swarm.Task {
		var tk swarm.Task
		tk.DesiredState, tk.Status.State, tk.Status.Err, tk.Status.Message = desired, state, errMsg, msg
		return tk
	}
	run, shut := swarm.TaskStateRunning, swarm.TaskStateShutdown
	for _, tc := range []struct {
		name     string
		tasks    []swarm.Task
		wantMsg  string
		wantDone bool
	}{
		{"nothing yet", nil, "0/0 running", false},
		{"all running", []swarm.Task{task(run, run, "", ""), task(run, run, "", "")}, "2/2 running", true},
		{"failed task's error", []swarm.Task{task(run, run, "", ""), task(run, swarm.TaskStateRejected, "port in use", "x")},
			"1/2 running — port in use", false},
		{"message when no error", []swarm.Task{task(run, swarm.TaskStateFailed, "", "exit 1")}, "0/1 running — exit 1", false},
		// A failure of a task no longer wanted says nothing once the rest run.
		{"old failure, all running", []swarm.Task{task(run, run, "", ""), task(shut, swarm.TaskStateFailed, "boom", "")},
			"1/1 running", true},
	} {
		msg, done := rolloutLine(tc.tasks)
		if msg != tc.wantMsg || done != tc.wantDone {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, msg, done, tc.wantMsg, tc.wantDone)
		}
	}
}
