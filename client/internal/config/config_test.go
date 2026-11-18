// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextUnderlay(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	body := "contexts:\n  prod:\n    underlay: [10.0.1.5/16, \" fd00::/64 \"]\n"
	if err := os.WriteFile(good, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(good)
	if err != nil {
		t.Fatal(err)
	}
	got := c.Underlay("prod")
	if len(got) != 2 || got[0].String() != "10.0.0.0/16" || got[1].String() != "fd00::/64" {
		t.Errorf("Underlay(prod) = %v, want [10.0.0.0/16 fd00::/64]", got)
	}
	if got := c.Underlay("other"); got != nil {
		t.Errorf("Underlay(other) = %v, want nil", got)
	}
	// A hand-built config with a bad entry skips it instead of failing.
	hand := Config{Contexts: map[string]ContextConfig{"x": {Underlay: []string{"nope", "192.168.0.0/24"}}}}
	if got := hand.Underlay("x"); len(got) != 1 || got[0].String() != "192.168.0.0/24" {
		t.Errorf("hand Underlay = %v", got)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("contexts:\n  prod:\n    underlay: [10.0.0.0]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "contexts.prod.underlay") {
		t.Errorf("bad underlay: err = %v, want one naming contexts.prod.underlay", err)
	}
}

func TestSaveOmitsEmptyContexts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if _, err := Default().Save(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "contexts") {
		t.Errorf("saved config mentions contexts without any set:\n%s", b)
	}
}

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

// ui.sync_docker_context and contexts.<name>.confirm_switch: both off unless
// the file says so, per context for the confirm, and a value that is not a
// boolean is a load error naming the file rather than a silent "off".
func TestContextSwitchSettings(t *testing.T) {
	if c := Default(); c.UI.SyncDockerContext || c.ConfirmSwitch("prod") {
		t.Errorf("defaults: sync=%v confirm(prod)=%v, want both off", c.UI.SyncDockerContext, c.ConfirmSwitch("prod"))
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	body := "ui:\n  sync_docker_context: true\ncontexts:\n  prod:\n    confirm_switch: true\n  dev:\n    underlay: [10.0.0.0/16]\n"
	if err := os.WriteFile(good, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(good)
	if err != nil {
		t.Fatal(err)
	}
	if !c.UI.SyncDockerContext {
		t.Error("ui.sync_docker_context: true not loaded")
	}
	if c.UI.Dim != DefaultUIDim {
		t.Errorf("a ui block without dim lost the default: %v", c.UI.Dim)
	}
	if !c.ConfirmSwitch("prod") || c.ConfirmSwitch("dev") || c.ConfirmSwitch("other") {
		t.Errorf("confirm prod/dev/other = %v/%v/%v, want true/false/false", c.ConfirmSwitch("prod"), c.ConfirmSwitch("dev"), c.ConfirmSwitch("other"))
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("contexts:\n  prod:\n    confirm_switch: maybe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("confirm_switch: maybe: err = %v, want a load error naming the file", err)
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

// Every SWARMEXEC_* variable lands on its field; unparsable numbers and
// booleans leave the value from before.
func TestOverlayEnvEveryVariable(t *testing.T) {
	for k, v := range map[string]string{
		"SWARMEXEC_CA":                "/e/ca",
		"SWARMEXEC_CERT":              "/e/cert",
		"SWARMEXEC_KEY":               "/e/key",
		"SWARMEXEC_PORT":              "9001",
		"SWARMEXEC_ADDR_MODE":         "ip",
		"SWARMEXEC_SERVER_NAME":       "agent.example",
		"SWARMEXEC_AGENT_SECRET":      "s3",
		"SWARMEXEC_AGENT_SECRET_FILE": "/e/secret",
		"SWARMEXEC_OPERATOR":          "alice",
		"SWARMEXEC_INSECURE":          " Yes ",
		"SWARMEXEC_UI_DIM":            "0.5",
	} {
		t.Setenv(k, v)
	}
	c := Default()
	if err := overlayEnv(&c); err != nil {
		t.Fatal(err)
	}
	got := []string{c.CA, c.Cert, c.Key, c.AddrMode, c.ServerName, c.AgentSecret, c.AgentSecretFile, c.Operator}
	want := []string{"/e/ca", "/e/cert", "/e/key", "ip", "agent.example", "s3", "/e/secret", "alice"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("strings = %q, want %q", got, want)
	}
	if c.Port != 9001 || !c.Insecure || c.UI.Dim != 0.5 {
		t.Errorf("port/insecure/dim = %d/%v/%v, want 9001/true/0.5", c.Port, c.Insecure, c.UI.Dim)
	}

	t.Setenv("SWARMEXEC_INSECURE", "off")
	if err := overlayEnv(&c); err != nil || c.Insecure {
		t.Errorf("off: err %v insecure %v", err, c.Insecure)
	}
}
