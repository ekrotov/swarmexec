package cli

import (
	"strings"
	"testing"
)

func TestInfoText(t *testing.T) {
	out := infoText(Version{Binary: "v1.2.3", Proto: "swarmexec/v1"})
	for _, want := range []string{"v1.2.3", "swarmexec/v1", contactEmail, "license"} {
		if !strings.Contains(out, want) {
			t.Errorf("info output missing %q:\n%s", want, out)
		}
	}
}
