// Package config parses agent configuration from flags with environment
// variable fallbacks (REQUIREMENTS §7).
package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// Config holds all runtime configuration for the agent.
type Config struct {
	ListenAddr string // gRPC listen address, e.g. ":9443"

	CACert     string // path to CA cert used to verify client certs
	ServerCert string // path to server certificate
	ServerKey  string // path to server private key

	DockerHost string // docker daemon endpoint

	DrainTimeout   time.Duration // graceful-shutdown drain window
	IdleTimeout    time.Duration // per-session idle timeout (0 = disabled)
	MaxSessionTime time.Duration // per-session max duration (0 = disabled)

	LogLevel  string // debug|info|warn|error
	LogFormat string // json|text
	AuditDest string // audit log destination: "stdout", "stderr", or a file path

	MetricsAddr string // optional Prometheus listen address, e.g. ":9100" (empty = disabled)

	ShowVersion bool
}

// env returns the environment variable value or the provided default.
func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envDuration parses a duration env var, falling back to def on unset/invalid.
func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Parse builds a Config from the given args (excluding the program name).
// Flags take precedence over environment variables, which take precedence over
// built-in defaults. Output and errors are written to out for testability.
func Parse(args []string, out io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("swarmexec-agent", flag.ContinueOnError)
	fs.SetOutput(out)

	c := &Config{}
	fs.StringVar(&c.ListenAddr, "listen", env("SWARMEXEC_LISTEN", ":9443"), "gRPC listen address (env SWARMEXEC_LISTEN)")
	fs.StringVar(&c.CACert, "ca-cert", env("SWARMEXEC_CA_CERT", ""), "path to CA certificate for verifying client certs (env SWARMEXEC_CA_CERT)")
	fs.StringVar(&c.ServerCert, "server-cert", env("SWARMEXEC_SERVER_CERT", ""), "path to server certificate (env SWARMEXEC_SERVER_CERT)")
	fs.StringVar(&c.ServerKey, "server-key", env("SWARMEXEC_SERVER_KEY", ""), "path to server private key (env SWARMEXEC_SERVER_KEY)")
	fs.StringVar(&c.DockerHost, "docker-host", env("SWARMEXEC_DOCKER_HOST", "unix:///var/run/docker.sock"), "docker daemon endpoint (env SWARMEXEC_DOCKER_HOST)")
	fs.DurationVar(&c.DrainTimeout, "drain-timeout", envDuration("SWARMEXEC_DRAIN_TIMEOUT", 5*time.Second), "graceful shutdown drain window (env SWARMEXEC_DRAIN_TIMEOUT)")
	fs.DurationVar(&c.IdleTimeout, "idle-timeout", envDuration("SWARMEXEC_IDLE_TIMEOUT", 0), "per-session idle timeout, 0=disabled (env SWARMEXEC_IDLE_TIMEOUT)")
	fs.DurationVar(&c.MaxSessionTime, "max-session", envDuration("SWARMEXEC_MAX_SESSION", 0), "per-session max duration, 0=disabled (env SWARMEXEC_MAX_SESSION)")
	fs.StringVar(&c.LogLevel, "log-level", env("SWARMEXEC_LOG_LEVEL", "info"), "log level: debug|info|warn|error (env SWARMEXEC_LOG_LEVEL)")
	fs.StringVar(&c.LogFormat, "log-format", env("SWARMEXEC_LOG_FORMAT", "json"), "log format: json|text (env SWARMEXEC_LOG_FORMAT)")
	fs.StringVar(&c.AuditDest, "audit-dest", env("SWARMEXEC_AUDIT_DEST", "stdout"), "audit log destination: stdout|stderr|<file path> (env SWARMEXEC_AUDIT_DEST)")
	fs.StringVar(&c.MetricsAddr, "metrics-addr", env("SWARMEXEC_METRICS_ADDR", ""), "Prometheus metrics listen address, empty=disabled (env SWARMEXEC_METRICS_ADDR)")
	fs.BoolVar(&c.ShowVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks that required fields are present and consistent. It is not
// called when only --version was requested.
func (c *Config) Validate() error {
	var missing []string
	if c.CACert == "" {
		missing = append(missing, "ca-cert")
	}
	if c.ServerCert == "" {
		missing = append(missing, "server-cert")
	}
	if c.ServerKey == "" {
		missing = append(missing, "server-key")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required TLS configuration: %v", missing)
	}
	return nil
}
