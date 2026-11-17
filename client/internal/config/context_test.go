// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadCfg(t *testing.T, p string) Config {
	t.Helper()
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const twoClusters = `agent_secret: global-made-up
port: 9000
insecure: true
contexts:
  prod:
    agent_secret: prod-made-up
    port: 9555
    addr_mode: ip
    insecure: false
    ca: /made/up/prod-ca.pem
  bare:
    confirm_switch: true
`

func TestForContextPerContextAndGlobalFallback(t *testing.T) {
	c := loadCfg(t, writeCfg(t, twoClusters, 0o600))

	prod := c.ForContext("prod")
	if prod.AgentSecret != "prod-made-up" || prod.Port != 9555 || prod.AddrMode != "ip" || prod.CA != "/made/up/prod-ca.pem" {
		t.Errorf("prod = %+v", prod)
	}
	if prod.Insecure {
		t.Error("an explicit contexts.prod.insecure: false must override the global true")
	}
	if got := prod.Source(KeyAgentSecret); got != "contexts.prod.agent_secret" {
		t.Errorf("prod secret source = %q", got)
	}

	// No block, and a block without credential keys: the global values.
	for _, name := range []string{"other", "bare"} {
		g := c.ForContext(name)
		if g.AgentSecret != "global-made-up" || g.Port != 9000 || !g.Insecure {
			t.Errorf("%s = %+v, want the global values", name, g)
		}
		if got := g.Source(KeyAgentSecret); got != SourceFile {
			t.Errorf("%s secret source = %q", name, got)
		}
	}
}

func TestForContextWithoutGlobalFallback(t *testing.T) {
	c := loadCfg(t, writeCfg(t, "contexts:\n  a:\n    agent_secret: a-made-up\n", 0o600))
	if got := c.ForContext("a").AgentSecret; got != "a-made-up" {
		t.Errorf("a = %q", got)
	}
	if got := c.ForContext("b"); got.AgentSecret != "" || got.SecretMode() {
		t.Errorf("b has no credentials of its own and no global: got secret %q", got.AgentSecret)
	}
	if got := c.ForContext("a").Source(KeyPort); got != SourceDefault {
		t.Errorf("port source = %q, want default", got)
	}
}

func TestForContextPrecedenceEnvOverContextOverGlobal(t *testing.T) {
	p := writeCfg(t, twoClusters, 0o600)
	t.Setenv("SWARMEXEC_AGENT_SECRET", "env-made-up")
	t.Setenv("SWARMEXEC_PORT", "7777")
	prod := loadCfg(t, p).ForContext("prod")
	if prod.AgentSecret != "env-made-up" || prod.Port != 7777 {
		t.Errorf("env must beat the context block: %+v", prod)
	}
	if got := prod.Source(KeyAgentSecret); got != "env SWARMEXEC_AGENT_SECRET" {
		t.Errorf("source = %q", got)
	}
	if prod.AddrMode != "ip" { // no env for it: the context block applies
		t.Errorf("addr_mode = %q", prod.AddrMode)
	}
}

func TestForContextSecretReplacesGlobalSecretFile(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "secret")
	if err := os.WriteFile(sf, []byte("from-global-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := writeCfg(t, "agent_secret_file: "+sf+"\ncontexts:\n  prod:\n    agent_secret: prod-made-up\n", 0o600)
	c := loadCfg(t, p)
	if v, _ := c.ForContext("prod").AgentSecretValue(); v != "prod-made-up" {
		t.Errorf("prod secret = %q: the global agent_secret_file shadowed it", v)
	}
	if v, _ := c.ForContext("elsewhere").AgentSecretValue(); v != "from-global-file" {
		t.Errorf("elsewhere = %q, want the global file's secret", v)
	}
}

func TestExistingGlobalConfigResolvesUnchanged(t *testing.T) {
	p := writeCfg(t, "agent_secret: old-made-up\ninsecure: true\naddr_mode: ip\nport: 9443\nca: /made/up/ca.pem\n", 0o600)
	c := loadCfg(t, p)
	for _, name := range []string{"default", "anything"} {
		got := c.ForContext(name)
		got.src, c.src = nil, nil
		if got.AgentSecret != c.AgentSecret || got.Insecure != c.Insecure || got.AddrMode != c.AddrMode ||
			got.Port != c.Port || got.CA != c.CA {
			t.Errorf("%s: %+v differs from the global %+v", name, got, c)
		}
	}
}

func TestContextBlockValidation(t *testing.T) {
	for _, body := range []string{
		"contexts:\n  a:\n    addr_mode: nonsense\n",
		"contexts:\n  a:\n    port: 70000\n",
	} {
		if _, err := Load(writeCfg(t, body, 0o600)); err == nil {
			t.Errorf("%q loaded without error", body)
		}
	}
}

func TestSetContextKeysLeavesOtherClustersAlone(t *testing.T) {
	p := writeCfg(t, "# keep me\n"+twoClusters+"future_key: 1\n", 0o600)
	if _, err := SetContextKeys(p, "staging", ContextKeys{AgentSecret: "staging-made-up", Insecure: true, AddrMode: "ip", Port: 9443}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	for _, want := range []string{"# keep me", "future_key: 1", "prod-made-up", "global-made-up"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("lost %q:\n%s", want, raw)
		}
	}
	c := loadCfg(t, p)
	if got := c.ForContext("prod").AgentSecret; got != "prod-made-up" {
		t.Errorf("prod = %q", got)
	}
	if got := c.ForContext("staging").AgentSecret; got != "staging-made-up" {
		t.Errorf("staging = %q", got)
	}
	if c.AgentSecret != "global-made-up" {
		t.Errorf("global = %q", c.AgentSecret)
	}
	if !c.Contexts["bare"].ConfirmSwitch {
		t.Error("another context's block changed")
	}
}

func TestSetContextKeysDropsShadowingSecretFile(t *testing.T) {
	p := writeCfg(t, "contexts:\n  a:\n    agent_secret_file: /made/up/file\n", 0o600)
	if _, err := SetContextKeys(p, "a", ContextKeys{AgentSecret: "new-made-up", Insecure: true}); err != nil {
		t.Fatal(err)
	}
	if got := loadCfg(t, p).ForContext("a"); got.AgentSecret != "new-made-up" || got.AgentSecretFile != "" {
		t.Errorf("a = %+v", got)
	}
}

func TestSetContextKeysDoesNotPersistTheEnvironment(t *testing.T) {
	p := writeCfg(t, "agent_secret: global-made-up\n", 0o600)
	t.Setenv("SWARMEXEC_CA", "/made/up/env-ca.pem")
	t.Setenv("SWARMEXEC_AGENT_SECRET_FILE", "/made/up/env-secret")
	t.Setenv("SWARMEXEC_OPERATOR", "env-operator")
	t.Setenv("SWARMEXEC_NO_UPDATE_CHECK", "1")
	if _, err := SetContextKeys(p, "x", ContextKeys{AgentSecret: "x-made-up", Insecure: true}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	for _, bad := range []string{"env-ca.pem", "env-secret", "env-operator", "check_updates"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("persisted %q:\n%s", bad, raw)
		}
	}
}

func TestSetContextKeysAtomicAnd0600OnExistingFile(t *testing.T) {
	p := writeCfg(t, "agent_secret: g\n", 0o644)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SetContextKeys(p, "x", ContextKeys{AgentSecret: "x-made-up", Insecure: true}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v; want 0600 although the file pre-existed as 0644", fi.Mode().Perm(), err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestSaveIsAtomicAnd0600OnExistingFile(t *testing.T) {
	p := writeCfg(t, "agent_secret: g\n", 0o644)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Default().Save(p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestSetContextKeysRefusesBrokenFile(t *testing.T) {
	const broken = "contexts: [unclosed\n"
	p := writeCfg(t, broken, 0o600)
	if _, err := SetContextKeys(p, "x", ContextKeys{AgentSecret: "s", Insecure: true}); err == nil {
		t.Fatal("overwrote a file that does not parse")
	}
	if raw, _ := os.ReadFile(p); string(raw) != broken {
		t.Errorf("broken file was modified: %q", raw)
	}
}

func TestSetContextKeysCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.yaml")
	if _, err := SetContextKeys(p, "x", ContextKeys{AgentSecret: "x-made-up", Insecure: true, Port: 9443}); err != nil {
		t.Fatal(err)
	}
	if got := loadCfg(t, p).ForContext("x"); got.AgentSecret != "x-made-up" || !got.Insecure {
		t.Errorf("x = %+v", got)
	}
	if _, err := SetContextKeys(p, "", ContextKeys{}); err == nil {
		t.Error("empty context name accepted")
	}
}
