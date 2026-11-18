// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// clearEnv keeps the developer's SWARMEXEC_* variables out of a Load test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, n := range []string{"SWARMEXEC_CA", "SWARMEXEC_CERT", "SWARMEXEC_KEY", "SWARMEXEC_PORT",
		"SWARMEXEC_ADDR_MODE", "SWARMEXEC_SERVER_NAME", "SWARMEXEC_AGENT_SECRET",
		"SWARMEXEC_AGENT_SECRET_FILE", "SWARMEXEC_INSECURE", "SWARMEXEC_OPERATOR", "SWARMEXEC_UI_DIM"} {
		t.Setenv(n, "")
	}
}

// The typo CLI-06 found: contexts.prod.confirmSwitch loaded without a word and
// the production prompt stayed off.
func TestLoadRefusesATypoedKey(t *testing.T) {
	clearEnv(t)
	for _, c := range []struct{ body, key, hint string }{
		{"contexts:\n  prod:\n    confirmSwitch: true\n", "contexts.prod.confirmSwitch", "contexts.prod.confirm_switch"},
		{"agent_secrt: x\n", "agent_secrt", "agent_secret"},
		{"ui:\n  sync_docker_contex: true\n", "ui.sync_docker_contex", "ui.sync_docker_context"},
		{"addr-mode: ip\n", "addr-mode", "addr_mode"},
	} {
		_, err := Load(writeConfig(t, c.body))
		if err == nil || !strings.Contains(err.Error(), c.key) || !strings.Contains(err.Error(), "did you mean "+c.hint) {
			t.Errorf("%q: err = %v, want it to name %s and suggest %s", c.body, err, c.key, c.hint)
		}
	}
}

// A key that is nothing like a setting is most likely a newer version's: it is
// a warning, not a refusal, so an older client keeps working on a shared file.
func TestLoadWarnsOnAForeignKey(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeConfig(t, "port: 9001\nfuture_feature: on\nui:\n  theme: dark\n"))
	if err != nil {
		t.Fatalf("a far-away unknown key must not refuse the file: %v", err)
	}
	if cfg.Port != 9001 {
		t.Errorf("port = %d, the known keys must still apply", cfg.Port)
	}
	got := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(got, `"future_feature"`) || !strings.Contains(got, `"ui.theme"`) {
		t.Errorf("warnings = %q", cfg.Warnings)
	}
}

func TestLoadAcceptsEveryKnownKey(t *testing.T) {
	clearEnv(t)
	body := "ca: a\ncert: b\nkey: c\nport: 1\naddr_mode: ip\nserver_name: s\nagent_secret: x\nagent_secret_file: f\n" +
		"insecure: true\nlegacy_secret: true\noperator: o\nlogs:\n  format: json\n  min_level: warn\n" +
		"ui:\n  dim: 0.3\n  sync_docker_context: true\n  check_updates: false\n" +
		"contexts:\n  any-name:\n    underlay: [10.0.0.0/16]\n    confirm_switch: true\n    port: 2\n    insecure: false\n"
	cfg, err := Load(writeConfig(t, body))
	if err != nil || len(cfg.Warnings) != 0 {
		t.Errorf("err %v warnings %v", err, cfg.Warnings)
	}
}

// Invalid environment values are reported, not defaulted.
func TestLoadReportsInvalidEnvValues(t *testing.T) {
	for _, c := range []struct{ name, val string }{
		{"SWARMEXEC_PORT", "80a0"},
		{"SWARMEXEC_PORT", "70000"},
		{"SWARMEXEC_INSECURE", "maybe"},
		{"SWARMEXEC_UI_DIM", "dim"},
	} {
		clearEnv(t)
		t.Setenv(c.name, c.val)
		_, err := Load(filepath.Join(t.TempDir(), "none.yaml"))
		if err == nil || !strings.Contains(err.Error(), c.name+"="+`"`+c.val+`"`) {
			t.Errorf("%s=%s: err = %v", c.name, c.val, err)
		}
	}
}

// The documented order is flag, env, context, global file, default. An
// agent_secret_file in the file used to beat an exported SWARMEXEC_AGENT_SECRET
// because AgentSecretValue prefers the file.
func TestEnvSecretBeatsFileSecretFile(t *testing.T) {
	clearEnv(t)
	secretFile := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(secretFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "agent_secret_file: "+secretFile+"\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := cfg.AgentSecretValue(); v != "from-file" {
		t.Fatalf("file alone: %q", v)
	}
	t.Setenv("SWARMEXEC_AGENT_SECRET", "from-env")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := cfg.AgentSecretValue(); v != "from-env" {
		t.Errorf("env over the file's agent_secret_file: got %q, want from-env", v)
	}
	// The context key still sits under the env.
	path = writeConfig(t, "agent_secret_file: "+secretFile+"\ncontexts:\n  c:\n    agent_secret_file: "+secretFile+"\n")
	cfg, _ = Load(path)
	if v, _ := cfg.ForContext("c").AgentSecretValue(); v != "from-env" {
		t.Errorf("env over a context secret file: got %q", v)
	}
}
