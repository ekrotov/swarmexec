// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

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

	"swarmexec/client/internal/clientlog"
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
		// Dial now, fail now, and with the real reason: an unreachable agent is
		// reported at connect time, not on the first RPC. NewClient has no
		// equivalent, so these stay until that change is made deliberately.
		//lint:ignore SA1019 see above
		grpc.WithBlock(),
		//lint:ignore SA1019 see above
		grpc.WithReturnConnectionError(),
		//lint:ignore SA1019 see above
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
		opts = append(opts, grpc.WithPerRPCCredentials(bearer{
			secret:   secret,
			operator: cfg.Operator,
			legacy:   cfg.LegacySecret,
		}))
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	start := time.Now()
	//lint:ignore SA1019 WithBlock needs DialContext; moving to NewClient changes when dial errors surface
	conn, err := grpc.DialContext(ctx, addr, opts...)
	clientlog.Timed("dial.agent", start, err, "addr", addr)
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

	// Server verification is skipped ONLY on the explicit --insecure opt-in.
	//
	// It used to also trigger on an empty CA, which made "I did not configure a
	// CA" indistinguishable from "I accept an unverified server". Validate
	// rejects that combination today, so the two agreed — but any caller that
	// built a Config without going through Validate got MITM exposure with
	// nothing in the configuration saying so. A dangerous mode should have
	// exactly one way to reach it, and it should be spelled out.
	if cfg.Insecure {
		tlsCfg.InsecureSkipVerify = true
		return tlsCfg, nil
	}
	if cfg.CA == "" {
		return nil, fmt.Errorf("no CA configured and --insecure not set: " +
			"set ca in the config to verify the agent, or opt in explicitly")
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

// bearer authenticates each RPC with the shared secret and carries the operator
// identity for audit. It requires transport security (TLS), which the agent
// always uses.
//
// It sends a proof BOUND to the connection, not the secret. RequireTransportSecurity
// returning true is not the protection it looks like: InsecureSkipVerify still
// counts as "transport security", so the old behaviour handed the raw secret to
// whatever server answered — and in the documented self-signed mode the client
// verifies nothing. See authmeta.Bind.
//
// legacy sends the raw secret as well, for agents predating the bound form. It
// is opt-in and off by default: a downgrade an attacker can trigger is not a
// compatibility feature.
type bearer struct {
	secret   string
	operator string
	legacy   bool
}

func (b bearer) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	m := map[string]string{}
	if b.operator != "" {
		m[authmeta.OperatorKey] = b.operator
	}

	cert, err := peerCert(ctx)
	if err != nil {
		// No certificate means no binding is possible. Refuse rather than fall
		// back to the raw secret: silently sending the credential in the one
		// situation we cannot reason about is how this was broken before.
		if !b.legacy {
			return nil, err
		}
		m[authmeta.SecretKey] = b.secret
		return m, nil
	}

	m[authmeta.BindingKey] = authmeta.Bind(b.secret, cert)
	if b.legacy {
		m[authmeta.SecretKey] = b.secret
	}
	return m, nil
}

// peerCert returns the DER of the server certificate this RPC travels to. gRPC
// exposes it through the per-RPC RequestInfo, which is the supported way to
// bind a credential to the channel carrying it.
func peerCert(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("no call context; cannot bind credentials to the connection")
	}
	ri, ok := credentials.RequestInfoFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("no TLS information for this call; cannot authenticate the agent securely")
	}
	tlsInfo, ok := ri.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, fmt.Errorf("connection is not TLS; refusing to send credentials")
	}
	if len(tlsInfo.State.PeerCertificates) == 0 {
		return nil, fmt.Errorf("agent presented no certificate; refusing to send credentials")
	}
	return tlsInfo.State.PeerCertificates[0].Raw, nil
}

func (b bearer) RequireTransportSecurity() bool { return true }
