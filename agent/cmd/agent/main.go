// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Command agent is the swarmexec Swarm agent: a long-running mTLS gRPC server
// that proxies interactive exec sessions into containers on its own node.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"swarmexec/agent/internal/audit"
	"swarmexec/agent/internal/auth"
	"swarmexec/agent/internal/config"
	"swarmexec/agent/internal/metrics"
	"swarmexec/agent/internal/server"
	"swarmexec/agent/internal/tlsconf"
	"swarmexec/agent/internal/version"
	"swarmexec/internal/pb"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Sidecar mode short-circuits everything below: a forward sidecar has no
	// docker socket, no secrets and no listener — it only pipes one connection.
	if addr, ok := forwardTarget(args); ok {
		if err := runForwardSidecar(addr); err != nil {
			// The control line on stderr already reported the reason to the
			// agent; a second "fatal:" line would only pollute its log.
			os.Exit(1)
		}
		return nil
	}

	cfg, err := config.Parse(args, os.Stderr)
	if err != nil {
		return err
	}
	if cfg.ShowVersion {
		fmt.Printf("swarmexec-agent %s (protocol %s)\n", version.Version, version.Protocol)
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := newLogger(cfg)
	log.Info("starting swarmexec agent",
		"version", version.Version,
		"protocol", version.Protocol,
		"listen", cfg.ListenAddr,
		"docker_host", cfg.DockerHost,
	)

	auditWriter, closeAudit, err := openAuditWriter(cfg.AuditDest)
	if err != nil {
		return err
	}
	defer closeAudit()
	auditLog := audit.New(slog.New(slog.NewJSONHandler(auditWriter, &slog.HandlerOptions{Level: slog.LevelInfo})))

	dockerCli, err := client.NewClientWithOpts(
		client.WithHost(cfg.DockerHost),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	defer dockerCli.Close()

	// Resolve the optional shared secret (file or value).
	secret, err := cfg.AgentSecretValue()
	if err != nil {
		return err
	}

	// Build the TLS config: either a self-signed cert generated here (SANs from
	// the Docker node info) or a provisioned server cert + client CA.
	var tlsCfg *tls.Config
	if cfg.SelfSigned {
		ictx, icancel := context.WithTimeout(context.Background(), 5*time.Second)
		sans := gatherSANs(ictx, dockerCli, cfg, log)
		icancel()
		var sanDesc []string
		tlsCfg, sanDesc, err = tlsconf.SelfSignedServerConfig(sans, cfg.CACert, time.Now())
		if err != nil {
			return err
		}
		log.Info("using self-signed server certificate",
			"sans", sanDesc,
			"verify_client_certs", cfg.CACert != "",
			"shared_secret", secret != "",
		)
	} else {
		tlsCfg, err = tlsconf.ServerConfig(cfg.CACert, cfg.ServerCert, cfg.ServerKey)
		if err != nil {
			return err
		}
	}

	// Optional Prometheus metrics endpoint.
	var sink server.Metrics = server.NopMetrics{}
	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		prom := metrics.New()
		sink = prom
		mux := http.NewServeMux()
		mux.Handle("/metrics", prom.Handler())
		metricsSrv = &http.Server{Addr: cfg.MetricsAddr, Handler: mux}
		go func() {
			log.Info("metrics endpoint listening", "addr", cfg.MetricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Error("metrics server failed", "err", err)
			}
		}()
	}

	// Lenient audit identity only when no client certificate is required
	// (self-signed without a client CA); otherwise the cert CN is mandatory.
	secretAuthIdentity := cfg.SelfSigned && cfg.CACert == ""

	srv := server.New(dockerCli, auth.AllowAll{}, auditLog, log, sink, server.Options{
		IdleTimeout:    cfg.IdleTimeout,
		MaxSessionTime: cfg.MaxSessionTime,
		SecretAuth:     secretAuthIdentity,
		ForwardImage:   cfg.ForwardImage,
	})

	// Background volume-size cache: scans at startup, then refreshes on volume
	// create/destroy events and periodically, so ListVolumes(with_size) answers
	// from memory instead of running a slow du-style scan per request.
	cacheCtx, cacheCancel := context.WithCancel(context.Background())
	defer cacheCancel()
	srv.StartVolumeSizeCache(cacheCtx)

	serverOpts := []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		// Permit the client's keepalive pings (every ~15s, even without an active
		// stream); the default policy would GOAWAY them as "too many pings". This
		// keeps a long, silent RPC (e.g. the volume disk-usage scan) alive over an
		// ssh tunnel. Also ping idle clients ourselves so dead links are noticed.
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	}
	if secret != "" {
		serverOpts = append(serverOpts,
			grpc.ChainUnaryInterceptor(server.SecretUnaryInterceptor(secret)),
			grpc.ChainStreamInterceptor(server.SecretStreamInterceptor(secret)),
		)
		log.Info("shared-secret authentication enabled")
	}
	grpcSrv := grpc.NewServer(serverOpts...)
	pb.RegisterAgentServer(grpcSrv, srv)

	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("gRPC server listening", "addr", cfg.ListenAddr)
		serveErr <- grpcSrv.Serve(lis)
	}()

	// Graceful shutdown on SIGTERM/SIGINT (REQUIREMENTS §4 graceful shutdown).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serveErr:
		return err
	case sig := <-sigCh:
		log.Info("shutdown signal received; draining", "signal", sig.String(), "drain_timeout", cfg.DrainTimeout)
		srv.StartDrain()

		drainCtx, cancel := context.WithTimeout(context.Background(), cfg.DrainTimeout)
		defer cancel()
		if srv.WaitForSessions(drainCtx) {
			log.Info("all sessions drained; stopping gracefully")
			grpcSrv.GracefulStop()
		} else {
			log.Warn("drain window elapsed; forcing stop", "active_sessions", srv.ActiveSessions())
			grpcSrv.Stop()
		}

		if metricsSrv != nil {
			shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
			defer c()
			_ = metricsSrv.Shutdown(shutCtx)
		}
		log.Info("agent stopped")
		return nil
	}
}

// newLogger builds the operational slog.Logger per config.
func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.ToLower(cfg.LogFormat) == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(h)
}

// gatherSANs builds the SAN list for the self-signed server cert: a stable
// "swarmexec-agent" name (so clients can pin it via --server-name), loopback,
// this node's hostname and advertised Swarm address (from the Docker API), and
// any operator-supplied extras.
func gatherSANs(ctx context.Context, cli *client.Client, cfg *config.Config, log *slog.Logger) []string {
	sans := []string{"DNS:swarmexec-agent", "DNS:localhost", "IP:127.0.0.1", "IP:::1"}
	if info, err := cli.Info(ctx); err != nil {
		log.Warn("could not read Docker info for cert SANs; using static SANs only", "err", err)
	} else {
		if info.Name != "" {
			sans = append(sans, "DNS:"+info.Name)
		}
		if info.Swarm.NodeAddr != "" {
			sans = append(sans, "IP:"+info.Swarm.NodeAddr)
		}
	}
	if cfg.CertSANs != "" {
		sans = append(sans, cfg.CertSANs)
	}
	return sans
}

// openAuditWriter resolves the audit destination to a writer and a closer.
func openAuditWriter(dest string) (io.Writer, func(), error) {
	switch dest {
	case "", "stdout":
		return os.Stdout, func() {}, nil
	case "stderr":
		return os.Stderr, func() {}, nil
	default:
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open audit log %q: %w", dest, err)
		}
		return f, func() { _ = f.Close() }, nil
	}
}
