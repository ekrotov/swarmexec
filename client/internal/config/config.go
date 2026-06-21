// Package config resolves swarmexec client configuration from (in increasing
// precedence) built-in defaults, an optional YAML config file, environment
// variables, and finally command-line flags. The cli package binds flags and
// applies them last; this package handles defaults + file + env.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// DefaultPort is the agent's gRPC/TLS port (CONTRACT.md §2).
const DefaultPort = 9443

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
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Port:     DefaultPort,
		AddrMode: AddrModeHostname,
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
	return cfg, nil
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
	// mTLS is mandatory: all three TLS paths must be present and readable.
	for _, f := range []struct {
		name, path string
	}{{"--ca", c.CA}, {"--cert", c.Cert}, {"--key", c.Key}} {
		if f.path == "" {
			return fmt.Errorf("missing TLS material: %s is required (mTLS is mandatory)", f.name)
		}
		if _, err := os.Stat(f.path); err != nil {
			return fmt.Errorf("TLS material for %s not readable: %w", f.name, err)
		}
	}
	return nil
}
