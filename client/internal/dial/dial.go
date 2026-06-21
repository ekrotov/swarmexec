// Package dial builds an mTLS gRPC connection to a node's agent. mTLS is
// mandatory and the agent's server identity is always verified — there is no
// insecure-skip path (REQUIREMENTS §7, CONTRACT.md §2).
package dial

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strconv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"swarmexec/client/internal/config"
)

// Dial establishes an mTLS gRPC connection to host:port, blocking until the
// connection (including the TLS handshake) is ready or ctx expires. Blocking
// here means a missing/untrusted cert or unreachable node fails fast with a
// clear error instead of hanging on the first RPC (REQUIREMENTS §8, §11). ctx
// should carry the connect timeout, not the session lifetime.
func Dial(ctx context.Context, host string, port int, cfg config.Config) (*grpc.ClientConn, error) {
	tlsCfg, err := loadTLS(cfg)
	if err != nil {
		return nil, err
	}
	sni := cfg.ServerName
	if sni == "" {
		sni = host
	}
	tlsCfg.ServerName = sni

	creds := credentials.NewTLS(tlsCfg)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := grpc.DialContext(ctx, addr, //nolint:staticcheck // WithBlock needs DialContext
		grpc.WithTransportCredentials(creds),
		grpc.WithBlock(),
		grpc.WithReturnConnectionError(),
		grpc.FailOnNonTempDialError(true),
	)
	if err != nil {
		return nil, fmt.Errorf("cannot reach agent on %s (mTLS/connection error: %w)", addr, err)
	}
	return conn, nil
}

func loadTLS(cfg config.Config) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.Cert, cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key (--cert %s, --key %s): %w", cfg.Cert, cfg.Key, err)
	}
	caPEM, err := os.ReadFile(cfg.CA)
	if err != nil {
		return nil, fmt.Errorf("read CA (--ca %s): %w", cfg.CA, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("no valid certificates found in CA file %s", cfg.CA)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
