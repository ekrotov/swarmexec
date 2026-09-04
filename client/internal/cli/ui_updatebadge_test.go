package cli

import (
	"strings"
	"testing"

	"swarmexec/client/internal/resolve"
)

func TestUpdateBadge(t *testing.T) {
	cases := []struct {
		state string
		want  string // substring expected, or "" for no badge
	}{
		{"updating", "⟳ updating"},
		{"paused", "⏸ update paused"},
		{"rollback_started", "↺ rolling back"},
		{"rollback_paused", "⏸ rollback paused"},
		{"completed", ""},
		{"rollback_completed", ""},
		{"", ""},
		{"something_new", ""}, // unknown states stay silent rather than mislabel
	}
	for _, c := range cases {
		got := updateBadge(c.state)
		if c.want == "" {
			if got != "" {
				t.Errorf("updateBadge(%q) = %q, want empty", c.state, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("updateBadge(%q) = %q, want to contain %q", c.state, got, c.want)
		}
	}
}

// A mid-update service row carries the badge; a settled one does not, and the
// badge never disturbs the leading columns.
func TestServiceRowUpdateBadge(t *testing.T) {
	cols := svcColumns{name: 3, mode: 10, repl: 3, image: 5}
	base := resolve.Service{Name: "web", Mode: "replicated", Running: 2, Desired: 3, Image: "nginx"}

	settled := serviceRow(base, cols, "")
	if strings.Contains(settled, "updating") {
		t.Errorf("settled row unexpectedly shows a badge: %q", settled)
	}

	updating := base
	updating.UpdateState = "updating"
	row := serviceRow(updating, cols, "")
	if !strings.Contains(row, "⟳ updating") {
		t.Errorf("updating row missing badge: %q", row)
	}
	// The columns before the badge must be identical to the settled row.
	if got := strings.SplitN(row, "⟳", 2)[0]; !strings.HasPrefix(got, settled) {
		t.Errorf("badge altered the leading columns:\n settled: %q\n updating: %q", settled, row)
	}
}
