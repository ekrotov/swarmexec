package config

import (
	"io"
	"testing"
	"time"
)

func TestParse_Defaults(t *testing.T) {
	c, err := Parse(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":9443" {
		t.Errorf("default listen: %q", c.ListenAddr)
	}
	if c.DockerHost != "unix:///var/run/docker.sock" {
		t.Errorf("default docker host: %q", c.DockerHost)
	}
	if c.DrainTimeout != 5*time.Second {
		t.Errorf("default drain: %v", c.DrainTimeout)
	}
	if c.IdleTimeout != 0 || c.MaxSessionTime != 0 {
		t.Errorf("timeouts should default disabled: idle=%v max=%v", c.IdleTimeout, c.MaxSessionTime)
	}
}

func TestParse_FlagsOverride(t *testing.T) {
	c, err := Parse([]string{"-listen", ":7000", "-idle-timeout", "30s"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":7000" {
		t.Errorf("listen override: %q", c.ListenAddr)
	}
	if c.IdleTimeout != 30*time.Second {
		t.Errorf("idle override: %v", c.IdleTimeout)
	}
}

func TestParse_PortDerivesListen(t *testing.T) {
	c, err := Parse([]string{"-port", "8443"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 8443 || c.ListenAddr != ":8443" {
		t.Errorf("port=%d listen=%q, want 8443/:8443", c.Port, c.ListenAddr)
	}
}

func TestParse_ListenOverridesPort(t *testing.T) {
	c, err := Parse([]string{"-port", "8443", "-listen", ":7000"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":7000" {
		t.Errorf("-listen must win over -port, got %q", c.ListenAddr)
	}
}

func TestParse_InvalidPort(t *testing.T) {
	if _, err := Parse([]string{"-port", "70000"}, io.Discard); err == nil {
		t.Fatal("expected error for out-of-range port")
	}
}

func TestValidate_RequiresTLS(t *testing.T) {
	c := &Config{}
	if err := c.Validate(); err == nil {
		t.Fatal("expected validation error for missing TLS config")
	}
	c = &Config{CACert: "ca", ServerCert: "cert", ServerKey: "key"}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}
