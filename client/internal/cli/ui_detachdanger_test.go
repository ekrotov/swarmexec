// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// d is detach in the secret detail and the network members overlay — a
// rolling update of the service it detaches from — so it is drawn red like
// every other destructive key, in the overlay's footer and in the tab's ?
// help. attach stays yellow.
func TestDetachIsADangerKey(t *testing.T) {
	const red, yellow = "[red]d[white] detach", "[yellow]a[white] attach"
	u, _, _ := editUI(t, editSwarm())

	openSecretDetail(t, u, apiKeySecret)
	if f := footerText(u); !strings.Contains(f, red) || !strings.Contains(f, yellow) {
		t.Errorf("secret detail footer %q: want d red, a yellow", f)
	}
	sendSpecial(t, u, tcell.KeyEscape)
	waitPageGone(t, u, pageSecDetail)

	openMembers(t, u, "web")
	if f := footerText(u); !strings.Contains(f, red) || !strings.Contains(f, yellow) {
		t.Errorf("network members footer %q: want d red, a yellow", f)
	}

	for name, sec := range map[string]func(*helpWriter){
		"secrets":  u.secTab().helpSection,
		"networks": u.netTab().helpSection,
	} {
		var h helpWriter
		sec(&h)
		if !strings.Contains(h.b.String(), "[red]  d ") {
			t.Errorf("%s help does not draw detach red:\n%s", name, h.b.String())
		}
	}
}
