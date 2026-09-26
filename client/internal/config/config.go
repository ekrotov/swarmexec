// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package config resolves swarmexec client configuration from (in increasing
// precedence) built-in defaults, an optional YAML config file, environment
// variables, and finally command-line flags. The cli package binds flags and
// applies them last; this package handles defaults + file + env.
package config

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"swarmexec/internal/deploy"
)

// DefaultPort is the agent's gRPC/TLS port (CONTRACT.md §2). Defined in
// internal/deploy because the same number has to be the one `init` publishes
// and the one the agent listens on.
const DefaultPort = deploy.DefaultPort

// Address selection modes for dialing the node that hosts the target task.
const (
	// AddrModeHostname dials the node's hostname (default).
	AddrModeHostname = "hostname"
	// AddrModeIP dials the node's advertised IP address.
	AddrModeIP = "ip"
)

// Config is the fully resolved client configuration.
type Config struct {
	// TLS material (mTLS is mandatory — REQUIREMENTS §7).
	CA   string `yaml:"ca"`
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`

	// Port is the agent port to dial.
	Port int `yaml:"port"`

	// AddrMode selects how the node dial address is derived (hostname|ip).
	AddrMode string `yaml:"addr_mode"`

	// ServerName overrides the TLS SNI / certificate name used to verify the
	// agent. Empty means use the dial host. Useful when dialing by IP but the
	// agent certificate carries a hostname.
	ServerName string `yaml:"server_name"`

	// AgentSecret is the shared secret presented to a self-signed agent
	// (Portainer-style auth). AgentSecretFile, if set, is read for the value.
	AgentSecret     string `yaml:"agent_secret"`
	AgentSecretFile string `yaml:"agent_secret_file"`

	// Insecure skips verification of the agent's server certificate. Required
	// when connecting to a self-signed agent (no CA to verify against); trust
	// then rests on the shared secret and the network.
	//
	// It protects the secret even so: the client sends a proof bound to the
	// connection rather than the secret itself, so an unverified server cannot
	// capture a credential that works elsewhere (authmeta.Bind). It does NOT
	// authenticate the agent — on a network you do not trust, configure a CA.
	Insecure bool `yaml:"insecure"`

	// LegacySecret additionally sends the RAW shared secret, for agents that
	// predate connection-bound authentication. Off by default, because a
	// downgrade an attacker can trigger is not a compatibility feature: with it
	// on, an unverified server receives the credential itself. Turn it on only
	// while agents are being rolled forward, on a network you trust.
	LegacySecret bool `yaml:"legacy_secret"`

	// Operator is the identity reported for audit when no client certificate is
	// used. Defaults to the local OS username.
	Operator string `yaml:"operator"`

	// Logs holds default log-format/filter settings for `logs` and the TUI log
	// view (flags override these).
	Logs LogsConfig `yaml:"logs"`

	// UI holds appearance settings for the `ui` TUI.
	UI UIConfig `yaml:"ui"`

	// ProxyDialer, when set, establishes the TCP connection to the agent
	// instead of dialing the node directly. It is wired at runtime (never from
	// YAML) when the Docker context is an ssh:// endpoint, so agent traffic
	// tunnels over the same SSH connection as the Docker API — otherwise the
	// nodes (only reachable through the bastion) would be unreachable. nil means
	// dial the node directly.
	ProxyDialer func(ctx context.Context, addr string) (net.Conn, error) `yaml:"-"`
}

// LogsConfig holds default log parsing/filtering for the log views.
type LogsConfig struct {
	// Format parses lines: auto | classic | json | logfmt | gelf | raw. Empty =
	// classic; auto detects the format from the lines themselves.
	Format string `yaml:"format"`
	// MinLevel hides lines below this level: trace..fatal. Empty = no filter.
	MinLevel string `yaml:"min_level"`
}

// DefaultUIDim is how far the backdrop behind an open overlay is faded toward
// the theme background (0 = no dimming, 1 = flat background).
const DefaultUIDim = 0.6

// UIConfig holds appearance settings for the interactive TUI.
type UIConfig struct {
	// Dim is the backdrop fade behind an open overlay, in [0,1]. 0 disables it;
	// higher values make windows behind the focused dialog recede further.
	// Out-of-range values are clamped when the theme is applied.
	Dim float64 `yaml:"dim"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Port:     DefaultPort,
		AddrMode: AddrModeHostname,
		UI:       UIConfig{Dim: DefaultUIDim},
	}
}

// DefaultFilePath returns the default config file location, honoring
// SWARMEXEC_CONFIG and XDG_CONFIG_HOME.
func DefaultFilePath() string {
	if p := os.Getenv("SWARMEXEC_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "swarmexec", "config.yaml")
}

// Load builds a Config from defaults, then overlays the YAML file at path (if it
// exists), then environment variables. A missing file is not an error; a present
// but malformed file is. Pass an empty path to use DefaultFilePath.
func Load(path string) (Config, error) {
	cfg := Default()

	if path == "" {
		path = DefaultFilePath()
	}
	if path != "" {
		if err := overlayFile(&cfg, path); err != nil {
			return cfg, err
		}
	}
	overlayEnv(&cfg)
	// Default the audited operator identity to the local OS username.
	if cfg.Operator == "" {
		if u, err := user.Current(); err == nil {
			cfg.Operator = u.Username
		}
	}
	return cfg, nil
}

// Save writes the config as YAML to path (DefaultFilePath if empty), creating
// the directory. The file is 0600 because it may hold the agent secret.
func (c Config) Save(path string) (string, error) {
	if path == "" {
		path = DefaultFilePath()
	}
	if path == "" {
		return "", fmt.Errorf("cannot determine config file path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func overlayFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read config file %s: %w", path, err)
	}
	// Decode into the existing struct so unspecified keys keep their defaults.
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	return nil
}

func overlayEnv(cfg *Config) {
	if v := os.Getenv("SWARMEXEC_CA"); v != "" {
		cfg.CA = v
	}
	if v := os.Getenv("SWARMEXEC_CERT"); v != "" {
		cfg.Cert = v
	}
	if v := os.Getenv("SWARMEXEC_KEY"); v != "" {
		cfg.Key = v
	}
	if v := os.Getenv("SWARMEXEC_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Port = n
		}
	}
	if v := os.Getenv("SWARMEXEC_ADDR_MODE"); v != "" {
		cfg.AddrMode = v
	}
	if v := os.Getenv("SWARMEXEC_SERVER_NAME"); v != "" {
		cfg.ServerName = v
	}
	if v := os.Getenv("SWARMEXEC_AGENT_SECRET"); v != "" {
		cfg.AgentSecret = v
	}
	if v := os.Getenv("SWARMEXEC_AGENT_SECRET_FILE"); v != "" {
		cfg.AgentSecretFile = v
	}
	if v := os.Getenv("SWARMEXEC_OPERATOR"); v != "" {
		cfg.Operator = v
	}
	if v := os.Getenv("SWARMEXEC_INSECURE"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			cfg.Insecure = true
		case "0", "false", "no", "off":
			cfg.Insecure = false
		}
	}
	if v := os.Getenv("SWARMEXEC_UI_DIM"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			cfg.UI.Dim = f
		}
	}
}

// AgentSecretValue resolves the shared secret: AgentSecretFile contents
// (trimmed) if set, otherwise AgentSecret. Returns "" when none is configured.
func (c Config) AgentSecretValue() (string, error) {
	if c.AgentSecretFile != "" {
		b, err := os.ReadFile(c.AgentSecretFile)
		if err != nil {
			return "", fmt.Errorf("read agent secret file %s: %w", c.AgentSecretFile, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return c.AgentSecret, nil
}

// SecretMode reports whether shared-secret authentication is configured.
func (c Config) SecretMode() bool {
	return c.AgentSecret != "" || c.AgentSecretFile != ""
}

// Validate checks that the configuration is usable for dialing an agent.
func (c Config) Validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d", c.Port)
	}
	switch c.AddrMode {
	case AddrModeHostname, AddrModeIP:
	default:
		return fmt.Errorf("invalid addr-mode %q (want %q or %q)", c.AddrMode, AddrModeHostname, AddrModeIP)
	}

	// readable verifies a configured path exists.
	readable := func(name, path string) error {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("TLS material for %s not readable: %w", name, err)
		}
		return nil
	}

	if c.SecretMode() {
		// Shared-secret auth (self-signed agent): client cert is optional, and a
		// CA is optional (without one, the server cert can't be verified, so
		// --insecure is required). Any provided paths must still be readable.
		if c.CA == "" && !c.Insecure {
			return fmt.Errorf("shared-secret mode needs either ca (to verify the agent) or insecure=true (skip verification)")
		}
		if c.CA != "" {
			if err := readable("ca", c.CA); err != nil {
				return err
			}
		}
		if (c.Cert == "") != (c.Key == "") {
			return fmt.Errorf("cert and key must be set together (or both empty)")
		}
		if c.Cert != "" {
			if err := readable("cert", c.Cert); err != nil {
				return err
			}
			if err := readable("key", c.Key); err != nil {
				return err
			}
		}
		return nil
	}

	// Default: mTLS is mandatory — all three TLS paths present and readable.
	for _, f := range []struct {
		name, path string
	}{{"ca", c.CA}, {"cert", c.Cert}, {"key", c.Key}} {
		if f.path == "" {
			return fmt.Errorf("missing TLS material: %s is required (mTLS is mandatory; or set agent_secret for a self-signed agent)", f.name)
		}
		if err := readable(f.name, f.path); err != nil {
			return err
		}
	}
	return nil
}
