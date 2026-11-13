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

	"github.com/moby/moby/client"
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
	// docker socket, no secrets and no listener — it only moves bytes.
	//
	// Mux mode first: it is what the agent asks for now, and a binary that
	// understands both must not mistake one for the other.
	if addr, ok := forwardMuxTarget(args); ok {
		if err := runForwardMuxSidecar(addr); err != nil {
			os.Exit(1)
		}
		return nil
	}
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
	// Identity is only verifiable when a client certificate is required and
	// checked against a CA. In self-signed shared-secret mode the operator name
	// is whatever the client put in a header, so say so on every record.
	identityVerified := !(cfg.SelfSigned && cfg.CACert == "")
	auditLog := audit.New(slog.New(slog.NewJSONHandler(auditWriter, &slog.HandlerOptions{Level: slog.LevelInfo})), identityVerified)

	// API-version negotiation is the default in moby/moby/client (the old
	// WithAPIVersionNegotiation option is a deprecated no-op).
	dockerCli, err := client.New(
		client.WithHost(cfg.DockerHost),
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
		prom := metrics.New(version.Version, version.Protocol)
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

	// Authorization: allow every authenticated request unless a rule file is
	// given. A file that does not parse stops the agent here — an agent that
	// started with "no policy" because its policy had a typo would be the
	// worst outcome of all.
	var authz auth.Authorizer = auth.AllowAll{}
	if cfg.PolicyFile != "" {
		p, err := auth.LoadPolicy(cfg.PolicyFile, identityVerified)
		if err != nil {
			return err
		}
		authz = p
		log.Info("authorization policy loaded", "file", cfg.PolicyFile)
	}

	srv := server.New(dockerCli, authz, auditLog, log, sink, server.Options{
		IdleTimeout:        cfg.IdleTimeout,
		MaxSessionTime:     cfg.MaxSessionTime,
		SecretAuth:         secretAuthIdentity,
		ForwardImage:       cfg.ForwardImage,
		MaxStreams:         cfg.MaxStreams,
		MaxForwardSidecars: cfg.MaxForwardSidecars,
	})

	// Background volume-size cache: scans at startup, then refreshes on volume
	// create/destroy events and periodically, so ListVolumes(with_size) answers
	// from memory instead of running a slow du-style scan per request.
	cacheCtx, cacheCancel := context.WithCancel(context.Background())
	defer cacheCancel()
	srv.StartVolumeSizeCache(cacheCtx)
	// Background container-stats sampler for the Stats RPC. Unlike the volume
	// cache it idles unless a client is actually asking, so an agent nobody is
	// watching costs nothing.
	srv.StartStatsCache(cacheCtx)
	// What a prune would free, measured hourly, and the prune schedule read
	// from this agent's own container labels (swarmexec.prune.*). Invalid
	// labels disable the schedule; they never stop the agent.
	srv.StartReclaimCache(cacheCtx)
	srv.StartPruneScheduler(cacheCtx)

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
		// First in the chain, so a panic anywhere below — authentication
		// included — costs one RPC, not every session on the node.
		grpc.ChainUnaryInterceptor(server.RecoveryUnaryInterceptor(log)),
		grpc.ChainStreamInterceptor(server.RecoveryStreamInterceptor(log)),
	}
	if secret != "" {
		// The certificate a client's proof is computed over. Taken from the TLS
		// config actually in use, so it is right for both the self-signed and
		// the provisioned-cert paths.
		var certDER []byte
		if len(tlsCfg.Certificates) > 0 && len(tlsCfg.Certificates[0].Certificate) > 0 {
			certDER = tlsCfg.Certificates[0].Certificate[0]
		}
		secretAuth := &server.SecretAuth{
			Secret:      secret,
			CertDER:     certDER,
			AllowLegacy: cfg.AllowLegacySecret,
			Log:         log,
		}
		serverOpts = append(serverOpts,
			grpc.ChainUnaryInterceptor(secretAuth.UnaryInterceptor()),
			grpc.ChainStreamInterceptor(secretAuth.StreamInterceptor()),
		)
		log.Info("shared-secret authentication enabled",
			"connection_bound", len(certDER) > 0, "legacy_raw_secret_accepted", cfg.AllowLegacySecret)
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
			if !stopGracefully(grpcSrv, gracefulStopTimeout) {
				log.Warn("graceful stop timed out; forced stop", "timeout", gracefulStopTimeout)
			}
		} else {
			log.Warn("drain window elapsed; forcing stop", "active_sessions", srv.ActiveSessions())
			grpcSrv.Stop()
			// Stop cancelled every stream but does not wait for its handler.
			// Give them a moment to return, so each writes its end-of-stream
			// audit record before the audit writer closes.
			exitCtx, c := context.WithTimeout(context.Background(), handlerExitTimeout)
			if !srv.WaitForSessions(exitCtx) {
				log.Warn("streams still running after forced stop", "active_sessions", srv.ActiveSessions())
			}
			c()
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

// Shutdown bounds after the drain window. Swarm sends SIGKILL after the
// service's stop_grace_period (10s by default; the shipped specs set none), so the
// drain window (5s) plus either bound has to finish well inside it.
const (
	// gracefulStopTimeout bounds GracefulStop once the streams have drained.
	// It still waits for in-flight unary RPCs, and a client that never hangs
	// up must not turn the graceful path into a SIGKILL.
	gracefulStopTimeout = 2 * time.Second
	// handlerExitTimeout bounds the wait, after a forced Stop, for cancelled
	// streams to return and write their end-of-stream audit records.
	handlerExitTimeout = 2 * time.Second
)

// grpcStopper is the part of *grpc.Server that shutdown uses.
type grpcStopper interface {
	GracefulStop()
	Stop()
}

// stopGracefully runs GracefulStop for at most timeout, then falls back to
// Stop. It reports whether the graceful stop finished in time.
func stopGracefully(s grpcStopper, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.GracefulStop()
		close(done)
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		// Stop closes every connection and cancels every RPC, but does not
		// wait for handlers; the goroutine above may linger on one that
		// ignores its context, which is harmless in a process about to exit.
		s.Stop()
		return false
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
	if res, err := cli.Info(ctx, client.InfoOptions{}); err != nil {
		log.Warn("could not read Docker info for cert SANs; using static SANs only", "err", err)
	} else {
		info := res.Info
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
