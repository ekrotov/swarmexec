// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ui.check_registry: on by default (today's behaviour), off from the file or
// from SWARMEXEC_NO_REGISTRY_CHECK, and a saved config neither writes the
// default nor loses a written false (PRF-03).
func TestCheckRegistryFileEnvAndSave(t *testing.T) {
	t.Setenv(EnvNoRegistryCheck, "")
	dir := t.TempDir()
	if !Default().UI.RegistryChecks() {
		t.Fatal("default must be on")
	}

	off := filepath.Join(dir, "off.yaml")
	if err := os.WriteFile(off, []byte("ui:\n  check_registry: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(off)
	if err != nil || c.UI.RegistryChecks() {
		t.Fatalf("ui.check_registry: false not honoured (err %v)", err)
	}
	if !c.UI.UpdateChecks() {
		t.Error("check_registry must not touch the release check")
	}
	saved := filepath.Join(dir, "saved.yaml")
	if _, err := c.Save(saved); err != nil {
		t.Fatal(err)
	}
	if c2, err := Load(saved); err != nil || c2.UI.RegistryChecks() {
		t.Errorf("round trip lost check_registry: false (err %v)", err)
	}
	plain := filepath.Join(dir, "plain.yaml")
	if _, err := Default().Save(plain); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(plain); strings.Contains(string(b), "check_registry") {
		t.Errorf("default config wrote check_registry:\n%s", b)
	}

	on := filepath.Join(dir, "on.yaml")
	if err := os.WriteFile(on, []byte("ui:\n  check_registry: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]bool{"": true, "0": true, "off": true, "1": false, "yes": false} {
		t.Setenv(EnvNoRegistryCheck, v)
		c, err := Load(on)
		if err != nil || c.UI.RegistryChecks() != want {
			t.Errorf("%s=%q: RegistryChecks = %v, want %v (err %v)", EnvNoRegistryCheck, v, c.UI.RegistryChecks(), want, err)
		}
	}
}
