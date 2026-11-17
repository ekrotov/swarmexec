// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `config show` reports the context-switch settings: ui.sync_docker_context
// always, contexts.<name>.confirm_switch for the resolved context when set.
// A temp config and DOCKER_CONFIG, never the operator's.
func TestConfigShowContextSwitchSettings(t *testing.T) {
	isolatedDockerConfig(t)
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1") // "default" — never dialled by show
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "insecure: true\nagent_secret: s3cret\nui:\n  sync_docker_context: true\ncontexts:\n  default:\n    confirm_switch: true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	g := &globalFlags{configPath: path}
	out, _, err := runCmd(t, newConfigCmd(g), "show")
	if err != nil {
		t.Fatal(err)
	}
	if !hasAll(out, "confirm switch:", "the TUI asks before switching to default", "sync docker context:  true") {
		t.Errorf("config show:\n%s", out)
	}
	if strings.Contains(out, "s3cret") {
		t.Error("config show printed the secret")
	}

	out, _, err = runCmd(t, newConfigCmd(g), "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v configView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if !v.ConfirmSwitch || !v.SyncDockerContext {
		t.Errorf("json confirm=%v sync=%v, want both true", v.ConfirmSwitch, v.SyncDockerContext)
	}

	// Neither set: no confirm line, sync false.
	if err := os.WriteFile(path, []byte("insecure: true\nagent_secret: s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = runCmd(t, newConfigCmd(g), "show")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "confirm switch") || !strings.Contains(out, "sync docker context:  false") {
		t.Errorf("config show without the keys:\n%s", out)
	}
}

// `config show` names ui.check_updates: on by default, off through the
// environment variable (F17).
func TestConfigShowCheckUpdates(t *testing.T) {
	isolatedDockerConfig(t)
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	t.Setenv("SWARMEXEC_NO_UPDATE_CHECK", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("insecure: true\nagent_secret: s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &globalFlags{configPath: path}
	line := func() string {
		out, _, err := runCmd(t, newConfigCmd(g), "show")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "check updates:") {
				return strings.Join(strings.Fields(l), " ")
			}
		}
		t.Fatalf("no check updates line:\n%s", out)
		return ""
	}
	if got := line(); got != "check updates: true" {
		t.Errorf("default: %q", got)
	}
	t.Setenv("SWARMEXEC_NO_UPDATE_CHECK", "1")
	if got := line(); got != "check updates: false" {
		t.Errorf("with SWARMEXEC_NO_UPDATE_CHECK=1: %q", got)
	}
}

// `config show` reports the values in effect for the resolved context and
// where each came from: the context block, the global key, or the environment.
func TestConfigShowPerContextCredentialsAndSources(t *testing.T) {
	isolatedDockerConfig(t)
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1") // context "default"
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "insecure: true\nagent_secret: global-made-up\nport: 9000\ncontexts:\n  default:\n    agent_secret: ctx-made-up\n    port: 9555\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &globalFlags{configPath: path}
	out, _, err := runCmd(t, newConfigCmd(g), "show", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var v configView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if v.AgentPort != 9555 || v.Sources["port"] != "contexts.default.port" ||
		v.Sources["agent_secret"] != "contexts.default.agent_secret" || v.Sources["insecure"] != "config file" {
		t.Errorf("view = %+v", v)
	}
	if strings.Contains(out, "made-up") {
		t.Error("config show printed a secret")
	}

	t.Setenv("SWARMEXEC_PORT", "7777")
	out, _, err = runCmd(t, newConfigCmd(g), "show")
	if err != nil {
		t.Fatal(err)
	}
	if !hasAll(out, "7777  (env SWARMEXEC_PORT)", "set (hidden)  (contexts.default.agent_secret)") {
		t.Errorf("config show:\n%s", out)
	}
}
