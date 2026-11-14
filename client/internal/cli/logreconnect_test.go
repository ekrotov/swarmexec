// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"swarmexec/client/internal/logfmt"
)

// A note row is shown verbatim (dimmed) regardless of the format, so a reconnect
// notice never gets swallowed by a structured parser or a filter.
func TestRenderLogRow_NoteAlwaysShown(t *testing.T) {
	got := renderLogRow(logfmt.JSON, logRow{note: true, line: "── reconnected ──"}, logfmt.Entry{})
	if !strings.Contains(got, "reconnected") {
		t.Errorf("note row not rendered verbatim: %q", got)
	}
}
