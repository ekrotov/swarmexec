// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

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
	// Session timeouts default ON. They used to default to 0/disabled, which
	// meant a forgotten exec session held a docker attach on a production node
	// for as long as the agent ran.
	if c.IdleTimeout != DefaultIdleTimeout || c.MaxSessionTime != DefaultMaxSessionTime {
		t.Errorf("timeouts should default on: idle=%v max=%v", c.IdleTimeout, c.MaxSessionTime)
	}
	// 0 means "built-in default" for the caps — an agent started without an
	// opinion must come out capped, not uncapped.
	if c.MaxStreams != 0 || c.MaxForwardSidecars != 0 {
		t.Errorf("caps should default to 0 (= built-in): streams=%d sidecars=%d", c.MaxStreams, c.MaxForwardSidecars)
	}
}

// Turning the checks off has to stay possible: an operator running long
// unattended sessions must be able to say so.
func TestParse_TimeoutsCanBeDisabled(t *testing.T) {
	c, err := Parse([]string{"-idle-timeout", "0", "-max-session", "0"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleTimeout != 0 || c.MaxSessionTime != 0 {
		t.Errorf("explicit 0 must disable: idle=%v max=%v", c.IdleTimeout, c.MaxSessionTime)
	}
}

func TestParse_LimitFlags(t *testing.T) {
	c, err := Parse([]string{"-max-streams", "8", "-max-forward-sidecars", "-1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxStreams != 8 {
		t.Errorf("max-streams: %d", c.MaxStreams)
	}
	// Negative is the documented "no limit" spelling.
	if c.MaxForwardSidecars != -1 {
		t.Errorf("max-forward-sidecars: %d", c.MaxForwardSidecars)
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
