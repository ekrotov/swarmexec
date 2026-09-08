package cli

import (
	"strings"
	"testing"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/secscan"
)

func TestSecurityBadge(t *testing.T) {
	if got := securityBadge(nil); got != "" {
		t.Errorf("no risks: badge = %q, want empty", got)
	}
	risks := []secscan.Finding{{Rule: "root-user", Severity: secscan.SevHigh}}
	if got := securityBadge(risks); !strings.Contains(got, "🛡") {
		t.Errorf("with risks: badge = %q, want a shield", got)
	}
}

// A service with findings gets the shield in its row; the columns before it are
// unchanged, and a clean service gets no shield.
func TestServiceRowSecurityBadge(t *testing.T) {
	cols := svcColumns{name: 3, mode: 10, repl: 3, image: 5}
	base := resolve.Service{Name: "web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx"}

	clean := serviceRow(base, cols, "")
	if strings.Contains(clean, "🛡") {
		t.Errorf("clean service unexpectedly shows a shield: %q", clean)
	}

	risky := base
	risky.Risks = []secscan.Finding{{Rule: "root-user", Title: "runs as root", Severity: secscan.SevHigh}}
	row := serviceRow(risky, cols, "")
	if !strings.Contains(row, "🛡") {
		t.Errorf("risky service missing shield: %q", row)
	}
	if got := strings.SplitN(row, "🛡", 2)[0]; !strings.HasPrefix(got, clean) {
		t.Errorf("shield altered the leading columns:\n clean: %q\n risky: %q", clean, row)
	}
}
