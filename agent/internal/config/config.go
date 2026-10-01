// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

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

	"swarmexec/internal/deploy"
)

// DefaultPort is the agent's gRPC/TLS port (CONTRACT.md §2).
// DefaultPort is the agent's listen port. It lives in internal/deploy because
// the client dials it and `swarmexec init` publishes it; three copies of 9443
// is exactly the kind of drift that package exists to prevent.
const DefaultPort = deploy.DefaultPort

// Session lifetime defaults. Chosen to end ABANDONED sessions without
// interrupting working ones: half an hour of complete silence in both
// directions is not someone thinking, and a shell open for twelve hours is a
// shell someone forgot. Both are configurable, and 0 still means "no check".
const (
	DefaultIdleTimeout    = 30 * time.Minute
	DefaultMaxSessionTime = 12 * time.Hour
)

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

	// AllowLegacySecret accepts the RAW shared secret from clients that predate
	// connection-bound authentication. On by default so an agent upgrade does
	// not strand older clients; turn it off once they are rolled forward.
	AllowLegacySecret bool

	DockerHost string // docker daemon endpoint

	// ForwardImage overrides the image port-forward sidecars run from; empty
	// means the agent's own image, found by self-inspection.
	ForwardImage string

	DrainTimeout   time.Duration // graceful-shutdown drain window
	IdleTimeout    time.Duration // per-session idle timeout (0 = disabled)
	MaxSessionTime time.Duration // per-session max duration (0 = disabled)

	// MaxStreams caps concurrent Exec/Logs/PortForward streams on this node and
	// MaxForwardSidecars caps live port-forward sidecar containers. 0 = the
	// built-in default, negative = no limit.
	MaxStreams         int
	MaxForwardSidecars int

	LogLevel  string // debug|info|warn|error
	LogFormat string // json|text
	AuditDest string // audit log destination: "stdout", "stderr", or a file path

	MetricsAddr string // optional Prometheus listen address, e.g. ":9100" (empty = disabled)
	PolicyFile  string // optional rule file; empty = allow every authenticated request

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
	// The flags named from deploy.Flag* are the ones `swarmexec init` writes:
	// they are a contract with the client, not just this binary's interface.
	// The literal ones below are the agent's own.
	fs.IntVar(&c.Port, deploy.FlagPort, envInt("SWARMEXEC_PORT", DefaultPort), "gRPC listen port (env SWARMEXEC_PORT)")
	fs.StringVar(&c.ListenAddr, "listen", env("SWARMEXEC_LISTEN", ""), "gRPC listen address; overrides -port when set, e.g. \":9443\" (env SWARMEXEC_LISTEN)")
	fs.StringVar(&c.CACert, "ca-cert", env("SWARMEXEC_CA_CERT", ""), "path to CA certificate for verifying client certs (env SWARMEXEC_CA_CERT)")
	fs.StringVar(&c.ServerCert, "server-cert", env("SWARMEXEC_SERVER_CERT", ""), "path to server certificate (env SWARMEXEC_SERVER_CERT)")
	fs.StringVar(&c.ServerKey, "server-key", env("SWARMEXEC_SERVER_KEY", ""), "path to server private key (env SWARMEXEC_SERVER_KEY)")
	fs.BoolVar(&c.SelfSigned, deploy.FlagSelfSigned, envBool("SWARMEXEC_SELF_SIGNED", false), "generate a self-signed server cert at startup (no server cert/key needed) (env SWARMEXEC_SELF_SIGNED)")
	fs.StringVar(&c.CertSANs, "cert-sans", env("SWARMEXEC_CERT_SANS", ""), "extra SANs for the self-signed cert, e.g. \"DNS:swarmexec-agent,IP:10.0.0.5\" (env SWARMEXEC_CERT_SANS)")
	fs.StringVar(&c.AgentSecret, "agent-secret", env("SWARMEXEC_AGENT_SECRET", ""), "shared secret clients must present; empty disables (env SWARMEXEC_AGENT_SECRET)")
	fs.StringVar(&c.AgentSecretFile, deploy.FlagAgentSecretFile, env("SWARMEXEC_AGENT_SECRET_FILE", ""), "file to read the shared secret from, e.g. a Docker secret (env SWARMEXEC_AGENT_SECRET_FILE)")
	fs.BoolVar(&c.AllowLegacySecret, deploy.FlagAllowLegacySecret, envBool("SWARMEXEC_ALLOW_LEGACY_SECRET", true), "accept the raw shared secret from clients predating connection-bound auth; disable once clients are upgraded (env SWARMEXEC_ALLOW_LEGACY_SECRET)")
	fs.StringVar(&c.DockerHost, deploy.FlagDockerHost, env("SWARMEXEC_DOCKER_HOST", deploy.SocketURL), "docker daemon endpoint (env SWARMEXEC_DOCKER_HOST)")
	fs.DurationVar(&c.DrainTimeout, deploy.FlagDrainTimeout, envDuration("SWARMEXEC_DRAIN_TIMEOUT", 5*time.Second), "graceful shutdown drain window (env SWARMEXEC_DRAIN_TIMEOUT)")
	// Non-zero by default. These used to be 0/disabled, which meant an exec
	// session that nobody ever closed lived as long as the agent — a forgotten
	// shell on a production node, holding a docker attach, indefinitely. The
	// values are deliberately generous: they end abandoned sessions, they do not
	// ration working ones. Set them to 0 to turn the checks off again.
	fs.DurationVar(&c.IdleTimeout, "idle-timeout", envDuration("SWARMEXEC_IDLE_TIMEOUT", DefaultIdleTimeout), "per-session idle timeout, 0=disabled (env SWARMEXEC_IDLE_TIMEOUT)")
	fs.DurationVar(&c.MaxSessionTime, "max-session", envDuration("SWARMEXEC_MAX_SESSION", DefaultMaxSessionTime), "per-session max duration, 0=disabled (env SWARMEXEC_MAX_SESSION)")
	fs.IntVar(&c.MaxStreams, "max-streams", envInt("SWARMEXEC_MAX_STREAMS", 0), "max concurrent exec/logs/port-forward streams, 0=built-in default, negative=unlimited (env SWARMEXEC_MAX_STREAMS)")
	fs.IntVar(&c.MaxForwardSidecars, "max-forward-sidecars", envInt("SWARMEXEC_MAX_FORWARD_SIDECARS", 0), "max live port-forward sidecar containers, 0=built-in default, negative=unlimited (env SWARMEXEC_MAX_FORWARD_SIDECARS)")
	fs.StringVar(&c.ForwardImage, "forward-image", env("SWARMEXEC_FORWARD_IMAGE", ""), "image for port-forward sidecars; empty = the agent's own image (env SWARMEXEC_FORWARD_IMAGE)")
	fs.StringVar(&c.LogLevel, "log-level", env("SWARMEXEC_LOG_LEVEL", "info"), "log level: debug|info|warn|error (env SWARMEXEC_LOG_LEVEL)")
	fs.StringVar(&c.LogFormat, deploy.FlagLogFormat, env("SWARMEXEC_LOG_FORMAT", "json"), "log format: json|text (env SWARMEXEC_LOG_FORMAT)")
	fs.StringVar(&c.AuditDest, deploy.FlagAuditDest, env("SWARMEXEC_AUDIT_DEST", "stdout"), "audit log destination: stdout|stderr|<file path> (env SWARMEXEC_AUDIT_DEST)")
	fs.StringVar(&c.PolicyFile, deploy.FlagPolicyFile, env("SWARMEXEC_POLICY_FILE", ""), "authorization rule file (YAML); empty = allow every authenticated request (env SWARMEXEC_POLICY_FILE)")
	fs.StringVar(&c.MetricsAddr, deploy.FlagMetricsAddr, env("SWARMEXEC_METRICS_ADDR", ""), "Prometheus metrics listen address, empty=disabled (env SWARMEXEC_METRICS_ADDR)")
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
