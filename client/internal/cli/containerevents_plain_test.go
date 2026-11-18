// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"swarmexec/client/internal/logfmt"
	pb "swarmexec/internal/pb"
)

// Notes are rendered escaped, so a colour tag in an event line shows up
// literally: "[red]● healthcheck FAILING[-]" (KOR-07). The lines carry words,
// not tags.
func TestDescribeContainerEventHasNoColourTags(t *testing.T) {
	evs := []*pb.ContainerEvent{
		{Action: "health_status: unhealthy", Health: "unhealthy"},
		{Action: "health_status: healthy", Health: "healthy"},
		{Action: "health_status: starting", Health: "starting"},
		{Action: "health_status: degraded", Health: "degraded"},
		{Action: "oom"}, {Action: "die", ExitCode: 1}, {Action: "die"},
		{Action: "kill"}, {Action: "start"}, {Action: "restart"}, {Action: "stop"},
		{Action: "pause"}, {Action: "unpause"},
	}
	for _, ev := range evs {
		line := describeContainerEvent(ev)
		if line == "" || strings.ContainsAny(line, "[]") {
			t.Errorf("%q -> %q: want a plain, non-empty line", ev.GetAction(), line)
		}
		// The note row wraps the line in exactly one gray tag pair; anything
		// more is the line's own tags, shown to the operator as text.
		got := renderLogRow(logfmt.JSON, logRow{note: true, line: line}, logfmt.Entry{})
		if strings.Count(got, "[") != 2 {
			t.Errorf("%q renders with stray tags: %q", ev.GetAction(), got)
		}
	}
}
