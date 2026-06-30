// Package dial builds the gRPC connection to a node's agent. The default is
// mutual TLS with the agent's server identity verified against a CA. When a
// shared secret is configured (self-signed agent), the client may skip server
// verification (--insecure) and authenticate with the secret instead, sent as
// per-RPC metadata (REQUIREMENTS §7, CONTRACT.md §2).
package dial

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"swarmexec/client/internal/config"
	"swarmexec/internal/authmeta"
)

// Dial establishes a gRPC connection to host:port, blocking until the
// connection (including the TLS handshake) is ready or ctx expires. Blocking
// here means a missing/untrusted cert or unreachable node fails fast with a
// clear error instead of hanging on the first RPC (REQUIREMENTS §8, §11). ctx
// should carry the connect timeout, not the session lifetime.
func Dial(ctx context.Context, host string, port int, cfg config.Config) (*grpc.ClientConn, error) {
	tlsCfg, err := loadTLS(cfg)
	if err != nil {
		return nil, err
	}
	if !tlsCfg.InsecureSkipVerify {
		sni := cfg.ServerName
		if sni == "" {
			sni = host
		}
		tlsCfg.ServerName = sni
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithBlock(),
		grpc.WithReturnConnectionError(),
		grpc.FailOnNonTempDialError(true),
		// Keep the connection warm with periodic pings. A long, silent RPC (e.g.
		// the disk-usage scan behind volume sizes) sends no application bytes, so
		// over an ssh -W tunnel the idle TCP link can be dropped by the bastion/NAT
		// mid-call. Pings keep traffic flowing so the call survives. The agent's
		// keepalive enforcement policy permits this interval.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                15 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	// When the Docker context is an ssh:// endpoint the swarm nodes are usually
	// only reachable through the bastion, not directly. ProxyDialer tunnels the
	// TCP connection over that same SSH host so agent RPCs work like the Docker
	// API does. gRPC still uses addr for SNI/authority (set above).
	if cfg.ProxyDialer != nil {
		opts = append(opts, grpc.WithContextDialer(cfg.ProxyDialer))
	}

	// Shared-secret auth: attach the secret (and operator identity) to every RPC.
	secret, err := cfg.AgentSecretValue()
	if err != nil {
		return nil, err
	}
	if secret != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearer{secret: secret, operator: cfg.Operator}))
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := grpc.DialContext(ctx, addr, opts...) //nolint:staticcheck // WithBlock needs DialContext
	if err != nil {
		return nil, fmt.Errorf("cannot reach agent on %s (mTLS/connection error: %w)", addr, err)
	}
	return conn, nil
}

func loadTLS(cfg config.Config) (*tls.Config, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	// Client certificate is optional in shared-secret mode.
	if cfg.Cert != "" && cfg.Key != "" {
		cert, err := tls.LoadX509KeyPair(cfg.Cert, cfg.Key)
		if err != nil {
			return nil, fmt.Errorf("load client cert/key (cert %s, key %s): %w", cfg.Cert, cfg.Key, err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	// Server verification: skip when --insecure or when no CA is provided
	// (self-signed agent). Otherwise verify against the CA.
	if cfg.Insecure || cfg.CA == "" {
		tlsCfg.InsecureSkipVerify = true
		return tlsCfg, nil
	}
	caPEM, err := os.ReadFile(cfg.CA)
	if err != nil {
		return nil, fmt.Errorf("read CA (ca %s): %w", cfg.CA, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certificates found in CA file %s", cfg.CA)
	}
	tlsCfg.RootCAs = pool
	return tlsCfg, nil
}

// bearer attaches the shared secret and operator identity as per-RPC metadata.
// It requires transport security (TLS), which the agent always uses.
type bearer struct {
	secret   string
	operator string
}

func (b bearer) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	m := map[string]string{authmeta.SecretKey: b.secret}
	if b.operator != "" {
		m[authmeta.OperatorKey] = b.operator
	}
	return m, nil
}

func (b bearer) RequireTransportSecurity() bool { return true }
