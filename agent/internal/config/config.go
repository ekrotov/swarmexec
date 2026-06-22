// Package config parses agent configuration from flags with environment
// variable fallbacks (REQUIREMENTS §7).
package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the agent's gRPC/TLS port (CONTRACT.md §2).
const DefaultPort = 9443

// Config holds all runtime configuration for the agent.
type Config struct {
	Port       int    // gRPC listen port (default 9443); used to derive ListenAddr
	ListenAddr string // gRPC listen address, e.g. ":9443" (overrides Port when set)

	CACert     string // path to CA cert used to verify client certs (optional in self-signed mode)
	ServerCert string // path to server certificate (unused in self-signed mode)
	ServerKey  string // path to server private key (unused in self-signed mode)

	// SelfSigned makes the agent generate its own server certificate at startup
	// (SANs taken from the Docker node info), so no server cert/key need to be
	// provisioned. Clients then skip server-cert verification and authenticate
	// with the shared AgentSecret instead (Portainer-style).
	SelfSigned bool
	// CertSANs are extra comma-separated SANs to add to the self-signed cert,
	// e.g. "DNS:swarmexec-agent,IP:10.0.0.5". The node hostname/IP and loopback
	// are always included.
	CertSANs string

	// AgentSecret is the shared secret clients must present (gRPC metadata). When
	// set, every RPC is authenticated against it. Empty disables the check.
	AgentSecret string
	// AgentSecretFile, if set, is read to obtain AgentSecret (e.g. a Docker
	// secret at /run/secrets/swarmexec_agent_secret).
	AgentSecretFile string

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

// envInt parses an integer env var, falling back to def on unset/invalid.
func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// envBool parses a boolean env var (1/true/yes/on), falling back to def.
func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
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
	fs.IntVar(&c.Port, "port", envInt("SWARMEXEC_PORT", DefaultPort), "gRPC listen port (env SWARMEXEC_PORT)")
	fs.StringVar(&c.ListenAddr, "listen", env("SWARMEXEC_LISTEN", ""), "gRPC listen address; overrides -port when set, e.g. \":9443\" (env SWARMEXEC_LISTEN)")
	fs.StringVar(&c.CACert, "ca-cert", env("SWARMEXEC_CA_CERT", ""), "path to CA certificate for verifying client certs (env SWARMEXEC_CA_CERT)")
	fs.StringVar(&c.ServerCert, "server-cert", env("SWARMEXEC_SERVER_CERT", ""), "path to server certificate (env SWARMEXEC_SERVER_CERT)")
	fs.StringVar(&c.ServerKey, "server-key", env("SWARMEXEC_SERVER_KEY", ""), "path to server private key (env SWARMEXEC_SERVER_KEY)")
	fs.BoolVar(&c.SelfSigned, "self-signed", envBool("SWARMEXEC_SELF_SIGNED", false), "generate a self-signed server cert at startup (no server cert/key needed) (env SWARMEXEC_SELF_SIGNED)")
	fs.StringVar(&c.CertSANs, "cert-sans", env("SWARMEXEC_CERT_SANS", ""), "extra SANs for the self-signed cert, e.g. \"DNS:swarmexec-agent,IP:10.0.0.5\" (env SWARMEXEC_CERT_SANS)")
	fs.StringVar(&c.AgentSecret, "agent-secret", env("SWARMEXEC_AGENT_SECRET", ""), "shared secret clients must present; empty disables (env SWARMEXEC_AGENT_SECRET)")
	fs.StringVar(&c.AgentSecretFile, "agent-secret-file", env("SWARMEXEC_AGENT_SECRET_FILE", ""), "file to read the shared secret from, e.g. a Docker secret (env SWARMEXEC_AGENT_SECRET_FILE)")
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
	// -listen (or SWARMEXEC_LISTEN) takes precedence; otherwise derive the
	// address from -port so the port is a first-class, simple knob.
	if c.ListenAddr == "" {
		if c.Port < 1 || c.Port > 65535 {
			return nil, fmt.Errorf("invalid -port %d (must be 1-65535)", c.Port)
		}
		c.ListenAddr = fmt.Sprintf(":%d", c.Port)
	}
	return c, nil
}

// Validate checks that required fields are present and consistent. It is not
// called when only --version was requested.
func (c *Config) Validate() error {
	if c.SelfSigned {
		// The server cert/key are generated at startup; ca-cert is optional
		// (when set, client certificates are still verified for identity).
		// Without a CA, the shared secret is the only authentication, so warn
		// callers by requiring it.
		secret, err := c.AgentSecretValue()
		if err != nil {
			return err
		}
		if c.CACert == "" && secret == "" {
			return fmt.Errorf("self-signed mode needs either -ca-cert (verify client certs) or -agent-secret/-agent-secret-file (shared-secret auth); otherwise anyone reachable could exec")
		}
		return nil
	}

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
		return fmt.Errorf("missing required TLS configuration: %v (or use -self-signed)", missing)
	}
	return nil
}

// AgentSecretValue resolves the shared secret: the contents of AgentSecretFile
// if set (trimmed), otherwise AgentSecret. Returns "" when no secret is configured.
func (c *Config) AgentSecretValue() (string, error) {
	if c.AgentSecretFile != "" {
		b, err := os.ReadFile(c.AgentSecretFile)
		if err != nil {
			return "", fmt.Errorf("read agent secret file %q: %w", c.AgentSecretFile, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return c.AgentSecret, nil
}
