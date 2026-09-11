package cli

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/swarm"
	"github.com/rivo/tview"

	"swarmexec/client/internal/resolve"
	"swarmexec/client/internal/secscan"
)

func TestSecurityBadge(t *testing.T) {
	// A clean service still occupies the slot, so the name column lines up.
	blank := strings.Repeat(" ", securityBadgeWidth)
	if got := securityBadge(nil); got != blank {
		t.Errorf("no risks: badge = %q, want %q", got, blank)
	}
	// A low-only finding (an unset User — the Swarm default) must NOT badge, or
	// nearly every row gets a shield and the signal is worthless.
	low := []secscan.Finding{{Rule: "root-user", Severity: secscan.SevLow}}
	if got := securityBadge(low); got != blank {
		t.Errorf("low-only risks: badge = %q, want %q", got, blank)
	}
	risks := []secscan.Finding{{Rule: "root-user", Severity: secscan.SevHigh}}
	got := securityBadge(risks)
	if !strings.Contains(got, "🛡") {
		t.Errorf("with risks: badge = %q, want a shield", got)
	}
	// Both variants must occupy the same rendered width, or flagged rows shift
	// the whole table sideways.
	if w, bw := tview.TaggedStringWidth(got), tview.TaggedStringWidth(blank); w != bw {
		t.Errorf("badge width %d != blank width %d — the name column would not line up", w, bw)
	}
}

// A service with findings gets the shield in its row; the columns before it are
// unchanged, and a clean service gets no shield.
func TestServiceRowSecurityBadge(t *testing.T) {
	cols := svcColumns{name: 3, mode: 10, repl: 3, image: 5}
	base := resolve.Service{Name: "web", Mode: "replicated", Running: 1, Desired: 1, Image: "nginx"}

	clean := serviceRow(base, cols, "", "")
	if strings.Contains(clean, "🛡") {
		t.Errorf("clean service unexpectedly shows a shield: %q", clean)
	}

	risky := base
	risky.Risks = []secscan.Finding{{Rule: "root-user", Title: "runs as root", Severity: secscan.SevHigh}}
	row := serviceRow(risky, cols, "", "")
	if !strings.Contains(row, "🛡") {
		t.Errorf("risky service missing shield: %q", row)
	}
	// The shield leads the row, and everything after it must be identical to the
	// clean row — same columns, same offsets.
	if strings.TrimPrefix(row, "🛡 ") != strings.TrimPrefix(clean, strings.Repeat(" ", securityBadgeWidth)) {
		t.Errorf("shield changed the row body:\n clean: %q\n risky: %q", clean, row)
	}
	if w, cw := tview.TaggedStringWidth(row), tview.TaggedStringWidth(clean); w != cw {
		t.Errorf("row widths differ (%d vs %d) — columns would not line up", w, cw)
	}
}

// Docker does not restrict env-var key or User characters, so a finding's text
// can contain tview colour tags. The overlay renders with dynamic colours and
// regions, so every spec-derived string must be escaped or an injected tag
// swallows/recolours the rest of the overlay (and can break the region tags).
func TestSecurityOverlayEscapesSpecMarkup(t *testing.T) {
	svc := swarm.Service{}
	svc.Spec.Name = "web"
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{
		User: "1000",
		Env:  []string{`[:white]DB_PASSWORD=hunter2`},
	}
	fs := secscan.Scan(svc)
	if len(fs) == 0 {
		t.Fatal("expected a secret-in-env finding for the injected key")
	}
	raw := fs[0].Detail
	if !strings.Contains(raw, "[:white]") {
		t.Fatalf("test premise broken: finding detail should carry the raw key: %q", raw)
	}
	// The value must never be in the finding, escaped or not.
	if strings.Contains(raw, "hunter2") {
		t.Errorf("finding leaks the secret value: %q", raw)
	}
	// Escaped, the tag is inert: tview renders it literally instead of parsing it.
	esc := tview.Escape(raw)
	if strings.Contains(esc, "[:white]") {
		t.Errorf("escaping left an active colour tag: %q", esc)
	}
	if tview.TaggedStringWidth(esc) <= tview.TaggedStringWidth(raw) {
		t.Errorf("escaped text should render wider (tag shown literally): esc=%d raw=%d",
			tview.TaggedStringWidth(esc), tview.TaggedStringWidth(raw))
	}
}
