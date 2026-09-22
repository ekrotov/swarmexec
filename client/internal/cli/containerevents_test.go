// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	pb "swarmexec/internal/pb"
)

// The events worth a line are the ones the container did on its own. Docker
// also emits exec_create/exec_start/attach/resize for what THIS TOOL does —
// showing those would have the view narrate its own operator.
func TestDescribeContainerEventIgnoresOurOwnFootprint(t *testing.T) {
	for _, action := range []string{"exec_create", "exec_start", "exec_detach", "attach", "resize", "top", "commit"} {
		if got := describeContainerEvent(&pb.ContainerEvent{Action: action}); got != "" {
			t.Errorf("action %q should be silent, got %q", action, got)
		}
	}
}

// The three an operator is actually waiting for, and what each must convey.
func TestDescribeContainerEventNamesWhatMatters(t *testing.T) {
	cases := []struct {
		ev   *pb.ContainerEvent
		want []string // substrings the line must carry
	}{
		// "Running but failing every probe" is invisible from the manager and
		// absent from the logs — the case this feature exists for.
		{&pb.ContainerEvent{Action: "health_status: unhealthy", Health: "unhealthy"}, []string{"FAILING", "red"}},
		{&pb.ContainerEvent{Action: "health_status: healthy", Health: "healthy"}, []string{"passing", "green"}},
		// An OOM kill usually presents as a bare exit 137 and a silent log,
		// which reads like the program crashing rather than the kernel taking
		// it away. So it is named.
		{&pb.ContainerEvent{Action: "oom"}, []string{"out of memory", "red"}},
		{&pb.ContainerEvent{Action: "die", ExitCode: 137}, []string{"137", "red"}},
		{&pb.ContainerEvent{Action: "die"}, []string{"code 0"}},
		{&pb.ContainerEvent{Action: "start"}, []string{"started"}},
		{&pb.ContainerEvent{Action: "restart"}, []string{"restarted"}},
	}
	for _, c := range cases {
		got := describeContainerEvent(c.ev)
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%q -> %q, missing %q", c.ev.GetAction(), got, want)
			}
		}
	}
}

// An unknown health verdict must still be shown. Docker may add one, and
// silently dropping a health transition is the failure mode this whole feature
// is meant to remove.
func TestDescribeContainerEventShowsUnknownHealthVerdicts(t *testing.T) {
	got := describeContainerEvent(&pb.ContainerEvent{Action: "health_status: degraded", Health: "degraded"})
	if !strings.Contains(got, "degraded") {
		t.Errorf("an unfamiliar verdict must still be reported, got %q", got)
	}
}
