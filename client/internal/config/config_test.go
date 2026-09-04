// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	c := Default()
	if c.Port != DefaultPort {
		t.Errorf("port = %d, want %d", c.Port, DefaultPort)
	}
	if c.AddrMode != AddrModeHostname {
		t.Errorf("addr-mode = %q, want %q", c.AddrMode, AddrModeHostname)
	}
	if c.UI.Dim != DefaultUIDim {
		t.Errorf("ui.dim = %v, want %v", c.UI.Dim, DefaultUIDim)
	}
}

func TestUIDimFileEnvAndDefault(t *testing.T) {
	dir := t.TempDir()

	// A config file with no ui block keeps the default.
	noUI := filepath.Join(dir, "no-ui.yaml")
	if err := os.WriteFile(noUI, []byte("port: 7000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(noUI); err != nil {
		t.Fatal(err)
	} else if c.UI.Dim != DefaultUIDim {
		t.Errorf("absent ui.dim = %v, want default %v", c.UI.Dim, DefaultUIDim)
	}

	// The file sets it; the env overrides it.
	withUI := filepath.Join(dir, "ui.yaml")
	if err := os.WriteFile(withUI, []byte("ui:\n  dim: 0.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(withUI); err != nil {
		t.Fatal(err)
	} else if c.UI.Dim != 0.3 {
		t.Errorf("file ui.dim = %v, want 0.3", c.UI.Dim)
	}
	t.Setenv("SWARMEXEC_UI_DIM", "0")
	if c, err := Load(withUI); err != nil {
		t.Fatal(err)
	} else if c.UI.Dim != 0 {
		t.Errorf("env ui.dim = %v, want 0 (env over file)", c.UI.Dim)
	}
}

func TestLoadFileThenEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("ca: /file/ca.pem\nport: 7000\naddr_mode: ip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SWARMEXEC_PORT", "8001") // env overrides file

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.CA != "/file/ca.pem" {
		t.Errorf("ca = %q, want /file/ca.pem", c.CA)
	}
	if c.Port != 8001 {
		t.Errorf("port = %d, want 8001 (env over file)", c.Port)
	}
	if c.AddrMode != "ip" {
		t.Errorf("addr-mode = %q, want ip", c.AddrMode)
	}
}

func TestValidateRequiresTLS(t *testing.T) {
	c := Default()
	if err := c.Validate(); err == nil {
		t.Fatal("want error when TLS material is missing")
	}

	dir := t.TempDir()
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c.CA, c.Cert, c.Key = write("ca"), write("cert"), write("key")
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error with valid TLS material: %v", err)
	}
}

func TestValidateBadAddrMode(t *testing.T) {
	c := Default()
	c.AddrMode = "bogus"
	dir := t.TempDir()
	for _, f := range []*string{&c.CA, &c.Cert, &c.Key} {
		p := filepath.Join(dir, "f")
		_ = os.WriteFile(p, []byte("x"), 0o600)
		*f = p
	}
	if err := c.Validate(); err == nil {
		t.Fatal("want error for invalid addr-mode")
	}
}
