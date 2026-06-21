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
