package cli

import (
	"fmt"
	"strings"
	"testing"
)

// The confirm must show what the rollback DOES, which is the reverse of the
// diff it is built from: the diff reads previous → current, so a "+" line (the
// update added it) becomes a removal, and a "-" line comes back.
func TestRollbackConfirmTextReversesTheDiff(t *testing.T) {
	diff := []string{
		"+ image: app:2.0", // the update introduced 2.0 → rollback removes it
		"- image: app:1.9", // the update dropped 1.9   → rollback restores it
		"  replicas: 3",    // unchanged context line
	}
	got := rollbackConfirmText("web", diff)

	if !strings.Contains(got, `Roll "web" back`) {
		t.Errorf("missing the service name:\n%s", got)
	}
	for _, want := range []string{
		"[red]- image: app:2.0",   // added by the update → shown as removed
		"[green]+ image: app:1.9", // removed by the update → shown as restored
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q — the preview would show the opposite of what happens:\n%s", want, got)
		}
	}
	// The direction must not survive unreversed.
	if strings.Contains(got, "+ image: app:2.0") && !strings.Contains(got, "[red]- image: app:2.0") {
		t.Errorf("the added line was not reversed:\n%s", got)
	}
	if !strings.Contains(got, "replicas: 3") {
		t.Errorf("context line dropped:\n%s", got)
	}
}

// A spec change with no field-level diff must say so rather than showing an
// empty "This will undo:" block.
func TestRollbackConfirmTextNoFieldDiff(t *testing.T) {
	got := rollbackConfirmText("web", nil)
	if !strings.Contains(got, "No field-level differences") {
		t.Errorf("empty diff should be spelled out:\n%s", got)
	}
	if strings.Contains(got, "This will undo") {
		t.Errorf("empty diff should not open an undo list:\n%s", got)
	}
}

// A long diff is capped so the dialog cannot grow past the screen, and the
// remainder is accounted for rather than silently dropped.
func TestRollbackConfirmTextCapsLongDiff(t *testing.T) {
	var diff []string
	for i := 0; i < rollbackDiffPreviewLines+7; i++ {
		diff = append(diff, fmt.Sprintf("+ env: KEY_%d=v", i))
	}
	got := rollbackConfirmText("web", diff)

	if n := strings.Count(got, "env: KEY_"); n != rollbackDiffPreviewLines {
		t.Errorf("showed %d diff lines, want the cap of %d", n, rollbackDiffPreviewLines)
	}
	if !strings.Contains(got, "and 7 more") {
		t.Errorf("the elided remainder is not reported:\n%s", got)
	}
}

// Spec values are operator-controlled (env keys, labels, image refs) and are
// rendered into a dynamic-colour dialog, so they must be escaped.
func TestRollbackConfirmTextEscapesMarkup(t *testing.T) {
	got := rollbackConfirmText("web", []string{"+ env: [red]OOPS=1"})
	if strings.Contains(got, "[red]OOPS") {
		t.Errorf("an injected colour tag survived unescaped:\n%s", got)
	}
}
