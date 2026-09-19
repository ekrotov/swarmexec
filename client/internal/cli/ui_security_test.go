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

// …and the same, through the function that actually builds the overlay. The
// test above only proves what tview.Escape does; it would keep passing if the
// renderer stopped calling it. This one fails in that case.
//
// The attack it pins down: a service deployed with an env key or User carrying
// a colour tag recolours the rest of the overlay, hiding its own finding and
// every finding of every service rendered after it — suppressing the output of
// the tool whose whole job is to surface risk.
func TestSecurityOverlayTextNeutralisesInjectedTags(t *testing.T) {
	inject := func(name, user string, env []string) resolve.Service {
		svc := swarm.Service{}
		svc.Spec.Name = name
		svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{User: user, Env: env}
		return resolve.Service{Name: name, Risks: secscan.Scan(svc)}
	}

	attacker := inject("aaa-evil", `root:0[black:black]`, []string{`[:white]DB_PASSWORD=hunter2`})
	victim := inject("zzz-victim", "root", nil)
	if !secscan.Actionable(attacker.Risks) || !secscan.Actionable(victim.Risks) {
		t.Fatal("test premise broken: both services must be flagged")
	}

	out := securityOverlayText(2, []resolve.Service{attacker, victim})

	// Every tag the attacker supplied must arrive inert. tview's escape form is
	// "[" + text + "[]", so the opening bracket is no longer a tag start.
	for _, injected := range []string{"[:white]", "[black:black]"} {
		if strings.Contains(out, injected) {
			t.Errorf("overlay carries an active injected tag %q:\n%s", injected, out)
		}
	}
	// The victim's finding must still be rendered — the point is that it stays
	// visible, not merely that the attacker's string changed.
	if !strings.Contains(out, "zzz-victim") {
		t.Errorf("the later service disappeared from the overlay:\n%s", out)
	}
	// A region tag injected through the service NAME would misdirect
	// Highlight/ScrollToHighlight; ids are index-based and names are escaped.
	if strings.Count(out, `["s`) != 2 {
		t.Errorf("want exactly 2 region tags, got %d:\n%s", strings.Count(out, `["s`), out)
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("overlay leaks a secret value:\n%s", out)
	}
}

// The empty and all-clear states must not claim more than they know.
func TestSecurityOverlayTextStatesItsGaps(t *testing.T) {
	if out := securityOverlayText(0, nil); !strings.Contains(out, "nothing has been scanned") {
		t.Errorf("no data must not read as an all-clear: %q", out)
	}
	out := securityOverlayText(7, nil)
	if !strings.Contains(out, "No security risks identified") || !strings.Contains(out, "Checked:") {
		t.Errorf("an all-clear must name what was checked: %q", out)
	}
}
