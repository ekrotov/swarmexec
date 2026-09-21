package cli

import (
	"errors"
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

	out := securityOverlayText(securityOverlayState{scanned: 2, flagged: []resolve.Service{attacker, victim}, loaded: true})

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

// The empty and all-clear states must not claim more than they know. A green
// "no risks" for data that was never read is the one output a security view
// must never produce: it is indistinguishable from a real all-clear.
func TestSecurityOverlayTextStatesItsGaps(t *testing.T) {
	// Never fetched.
	out := securityOverlayText(securityOverlayState{})
	if !strings.Contains(out, "nothing has been scanned") {
		t.Errorf("no data must not read as an all-clear: %q", out)
	}
	if strings.Contains(out, "No security risks identified") {
		t.Errorf("unscanned state must not contain an all-clear: %q", out)
	}

	// Never fetched, and we know why — name it.
	out = securityOverlayText(securityOverlayState{staleErr: errors.New("manager unreachable")})
	if !strings.Contains(out, "manager unreachable") {
		t.Errorf("the reason should be shown: %q", out)
	}

	// Fetched, genuinely empty cluster: "no services" is not "no risks".
	out = securityOverlayText(securityOverlayState{loaded: true})
	if strings.Contains(out, "No security risks identified") {
		t.Errorf("an empty cluster must not read as an all-clear: %q", out)
	}
	if !strings.Contains(out, "no services") {
		t.Errorf("want the empty-cluster wording: %q", out)
	}

	// Fetched and clean: an all-clear, naming what was checked.
	out = securityOverlayText(securityOverlayState{scanned: 7, loaded: true})
	if !strings.Contains(out, "No security risks identified") || !strings.Contains(out, "Checked:") {
		t.Errorf("an all-clear must name what was checked: %q", out)
	}
}

// A refresh that failed after an earlier success leaves the previous answer on
// screen. Presenting it as current is the subtler half of the same mistake.
func TestSecurityOverlayTextMarksStaleData(t *testing.T) {
	st := securityOverlayState{scanned: 3, loaded: true, staleErr: errors.New("manager unreachable")}

	out := securityOverlayText(st)
	if !strings.Contains(out, "last refresh failed") {
		t.Errorf("a stale all-clear must be marked: %q", out)
	}
	if !strings.Contains(out, "manager unreachable") {
		t.Errorf("the reason should be shown: %q", out)
	}

	// Also when there ARE findings — the list is just as stale as the all-clear.
	svc := swarm.Service{}
	svc.Spec.Name = "web"
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{User: "root"}
	st.flagged = []resolve.Service{{Name: "web", Risks: secscan.Scan(svc)}}
	if out := securityOverlayText(st); !strings.Contains(out, "last refresh failed") {
		t.Errorf("stale findings must be marked too: %q", out)
	}
}
