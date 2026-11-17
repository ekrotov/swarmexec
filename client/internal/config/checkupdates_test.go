// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ui.check_updates: on by default, off from the file or from
// SWARMEXEC_NO_UPDATE_CHECK, and a saved config neither writes the default
// nor loses a written false (F17).
func TestCheckUpdatesFileEnvAndSave(t *testing.T) {
	t.Setenv(EnvNoUpdateCheck, "")
	dir := t.TempDir()
	if !Default().UI.UpdateChecks() {
		t.Fatal("default must be on")
	}

	off := filepath.Join(dir, "off.yaml")
	if err := os.WriteFile(off, []byte("ui:\n  check_updates: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(off)
	if err != nil {
		t.Fatal(err)
	}
	if c.UI.UpdateChecks() {
		t.Error("ui.check_updates: false not honoured")
	}
	// Save → Load round trip keeps the false.
	saved := filepath.Join(dir, "saved.yaml")
	if _, err := c.Save(saved); err != nil {
		t.Fatal(err)
	}
	if c2, err := Load(saved); err != nil || c2.UI.UpdateChecks() {
		t.Errorf("round trip lost check_updates: false (err %v)", err)
	}
	// Saving the default does not write the key.
	plain := filepath.Join(dir, "plain.yaml")
	if _, err := Default().Save(plain); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(plain); strings.Contains(string(b), "check_updates") {
		t.Errorf("default config wrote check_updates:\n%s", b)
	}

	on := filepath.Join(dir, "on.yaml")
	if err := os.WriteFile(on, []byte("ui:\n  check_updates: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]bool{"": true, "0": true, "false": true, "off": true, "1": false, "yes": false, "true": false} {
		t.Setenv(EnvNoUpdateCheck, v)
		c, err := Load(on)
		if err != nil || c.UI.UpdateChecks() != want {
			t.Errorf("%s=%q: UpdateChecks = %v, want %v (err %v)", EnvNoUpdateCheck, v, c.UI.UpdateChecks(), want, err)
		}
	}
}
