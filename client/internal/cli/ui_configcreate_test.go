// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"swarmexec/client/internal/swarmops"
)

// T-14: n on the Configs tab opens the Secrets tab's create dialog for a
// config; Ctrl-S creates it through the manager API and the tab reloads.
func TestConfigsTabCreatesAConfig(t *testing.T) {
	u, ls, _ := editUI(t, editSwarm())
	onLoop(t, u, func() { u.tab("configs").enter() })
	waitForTable(t, u, u.cfgTab().table, "n to create one")

	onLoop(t, u, func() { u.cfgTab().keys(rune_(u.km.CfgNew)) })
	waitFront(t, u, pageCfgForm)
	if text := drawFront(t, u, 120, 40); !strings.Contains(text, "new config") {
		t.Errorf("dialog:\n%s", text)
	}
	for _, ev := range parseKeys(t, "app.conf<Tab>team=web<Tab>ilisten 80<Esc>") {
		key(t, u, ev)
	}
	key(t, u, tcell.NewEventKey(tcell.KeyCtrlS, 0, tcell.ModCtrl))
	waitForTable(t, u, u.cfgTab().table, "app.conf")
	waitFront(t, u, pageMain)

	ls.mu.Lock()
	defer ls.mu.Unlock()
	if n := len(ls.fs.Configs); n != 1 {
		t.Fatalf("%d configs on the manager", n)
	}
	c := ls.fs.Configs[0]
	if c.Spec.Name != "app.conf" || string(c.Spec.Data) != "listen 80" || c.Spec.Labels["team"] != "web" {
		t.Errorf("created %q %q %v", c.Spec.Name, c.Spec.Data, c.Spec.Labels)
	}
}

// The config dialog checks against the config limit (1000 KB), not the
// secret's 500 KB, and its messages name a config.
func TestValidatePayloadPerKind(t *testing.T) {
	between := strings.Repeat("x", swarmops.MaxSecretSize+1)
	if _, _, err := validatePayload(secretKind, "a", "", between); err == nil {
		t.Error("over 500 KB must be refused for a secret")
	}
	if _, _, err := validatePayload(configKind, "a", "", between); err != nil {
		t.Errorf("a config may be over 500 KB: %v", err)
	}
	over := strings.Repeat("x", swarmops.MaxConfigSize+1)
	if _, _, err := validatePayload(configKind, "a", "", over); err == nil || !strings.Contains(err.Error(), "1000 KB") {
		t.Errorf("over 1000 KB must be refused for a config, got %v", err)
	}
	if s := payloadSize(configKind, over); !strings.Contains(s, "> 1000 KB") {
		t.Errorf("size status %q", s)
	}
	bin := writeFile(t, "blob", "a\x00b")
	if _, err := readPayloadFile(configKind, bin, 0); err == nil || !strings.Contains(err.Error(), "docker config create") {
		t.Errorf("binary file: %v", err)
	}
}
