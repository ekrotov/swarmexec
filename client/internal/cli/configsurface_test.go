// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// resolveWith builds the real command tree, parses args on `ps` and resolves
// the config the way every command does.
func resolveWith(t *testing.T, g *globalFlags, args ...string) (cmd *cobra.Command, errOut *bytes.Buffer, err error) {
	t.Helper()
	root := newRootCmd(g, Version{})
	cmd, _, ferr := root.Find([]string{"ps"})
	if ferr != nil {
		t.Fatal(ferr)
	}
	errOut = &bytes.Buffer{}
	cmd.SetErr(errOut)
	if perr := cmd.ParseFlags(args); perr != nil {
		t.Fatal(perr)
	}
	return cmd, errOut, nil
}

func clearSwarmexecEnv(t *testing.T) {
	t.Helper()
	isolatedDockerConfig(t)
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	for _, n := range []string{"SWARMEXEC_CA", "SWARMEXEC_CERT", "SWARMEXEC_KEY", "SWARMEXEC_PORT",
		"SWARMEXEC_ADDR_MODE", "SWARMEXEC_SERVER_NAME", "SWARMEXEC_AGENT_SECRET",
		"SWARMEXEC_AGENT_SECRET_FILE", "SWARMEXEC_INSECURE", "SWARMEXEC_OPERATOR", "SWARMEXEC_UI_DIM",
		"SWARMEXEC_KEYS", "SWARMEXEC_CONFIG"} {
		t.Setenv(n, "")
	}
}

// Precedence, end to end: flag > env > contexts.<name>.<key> > global file >
// default, for the secret that is spelled two ways. An agent_secret_file from
// the config used to beat both --agent-secret and SWARMEXEC_AGENT_SECRET.
func TestSecretPrecedenceFlagEnvContextFile(t *testing.T) {
	clearSwarmexecEnv(t)
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	globalFile := write("global", "global-file\n")
	ctxFile := write("ctx", "ctx-file\n")
	cfgPath := write("config.yaml", "insecure: true\nagent_secret_file: "+globalFile+
		"\ncontexts:\n  default:\n    agent_secret_file: "+ctxFile+"\n")
	// a second config where only the global file is set
	globalOnly := write("global.yaml", "insecure: true\nagent_secret_file: "+globalFile+"\n")

	secret := func(cfgPath string, args ...string) (string, string) {
		t.Helper()
		g := &globalFlags{}
		cmd, _, _ := resolveWith(t, g, append([]string{"--config", cfgPath}, args...)...)
		cfg, err := g.resolveConfig(cmd, resolveEndpoint("default"))
		if err != nil {
			t.Fatal(err)
		}
		v, err := cfg.AgentSecretValue()
		if err != nil {
			t.Fatal(err)
		}
		return v, cfg.Source("agent_secret")
	}

	if v, src := secret(globalOnly); v != "global-file" || src != "config file" {
		t.Errorf("global file: %q (%s)", v, src)
	}
	if v, src := secret(cfgPath); v != "ctx-file" || !strings.HasPrefix(src, "contexts.default.") {
		t.Errorf("context over global: %q (%s)", v, src)
	}
	t.Setenv("SWARMEXEC_AGENT_SECRET", "env-secret")
	if v, src := secret(cfgPath); v != "env-secret" || src != "env SWARMEXEC_AGENT_SECRET" {
		t.Errorf("env over a file secret: %q (%s)", v, src)
	}
	if v, src := secret(globalOnly, "--agent-secret", "flag-secret"); v != "flag-secret" || src != "flag --agent-secret" {
		t.Errorf("flag over env and file: %q (%s)", v, src)
	}
	t.Setenv("SWARMEXEC_AGENT_SECRET", "")
	if v, _ := secret(globalOnly, "--agent-secret", "flag-secret"); v != "flag-secret" {
		t.Errorf("flag over an agent_secret_file from the config: %q", v)
	}
	if v, _ := secret(globalOnly, "--agent-secret-file", ctxFile); v != "ctx-file" {
		t.Errorf("--agent-secret-file: %q", v)
	}
}

func TestResolveConfigRefusesATypoedKey(t *testing.T) {
	clearSwarmexecEnv(t)
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("contexts:\n  prod:\n    confirmSwitch: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &globalFlags{}
	cmd, _, _ := resolveWith(t, g, "--config", p)
	_, err := g.resolveConfig(cmd, resolveEndpoint("default"))
	if err == nil || !strings.Contains(err.Error(), "confirmSwitch") || !strings.Contains(err.Error(), "confirm_switch") {
		t.Errorf("err = %v", err)
	}
}

// A foreign key warns once on stderr, however many times the config is
// resolved (the TUI does it on every cluster switch).
func TestResolveConfigWarnsOnceOnAForeignKey(t *testing.T) {
	clearSwarmexecEnv(t)
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("insecure: true\nagent_secret: x\nfuture_feature: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &globalFlags{}
	cmd, errOut, _ := resolveWith(t, g, "--config", p)
	for i := 0; i < 3; i++ {
		if _, err := g.resolveConfig(cmd, resolveEndpoint("default")); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(errOut.String(), `unknown key "future_feature"`); n != 1 {
		t.Errorf("warned %d times:\n%s", n, errOut)
	}
}

func TestInvalidLogLevelIsAUsageError(t *testing.T) {
	for _, bad := range []string{"bogus", "debg", "5"} {
		g := &globalFlags{logLevel: bad}
		err := g.initLogging()
		var ce *cliError
		if err == nil || !errors.As(err, &ce) || ce.code != exitUsage || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	for _, ok := range []string{"debug", "INFO", "warn", "warning", "error", "off"} {
		if _, err := parseLogLevel(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

// keys.yaml sits next to the config file in use, so --config moves it too;
// SWARMEXEC_KEYS still wins.
func TestKeysFileFollowsConfigFlag(t *testing.T) {
	clearSwarmexecEnv(t)
	dir := t.TempDir()
	if got, want := keysPathFor(filepath.Join(dir, "team.yaml")), filepath.Join(dir, "keys.yaml"); got != want {
		t.Errorf("keysPathFor(--config) = %q, want %q", got, want)
	}
	t.Setenv("SWARMEXEC_KEYS", "/elsewhere/k.yaml")
	if got := keysPathFor(filepath.Join(dir, "team.yaml")); got != "/elsewhere/k.yaml" {
		t.Errorf("SWARMEXEC_KEYS = %q", got)
	}
}
