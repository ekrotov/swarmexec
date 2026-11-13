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
	// The container health probe (agent/Dockerfile HEALTHCHECK): a short-lived
	// client of the agent next to it, not a second agent.
	if rest, ok := healthcheckMode(args); ok {
		os.Exit(runHealthcheck(rest))
	}
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

	// API-version negotiation is the default in moby/moby/client (the old
	// WithAPIVersionNegotiation option is a deprecated no-op).
	dockerCli, err := client.New(
		client.WithHost(cfg.DockerHost),
	)
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	defer dockerCli.Close()

	a, err := buildServer(cfg, deps{log: log, audit: auditWriter, docker: dockerCli})
	if err != nil {
		return err
	}

	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.ListenAddr, err)
	}

	// Graceful shutdown on SIGTERM/SIGINT (REQUIREMENTS §4 graceful shutdown).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	return a.serve(lis, sigCh)
}

// deps is what run takes from the outside world before it builds the agent: a
// test hands buildServer a fake Docker client and a buffer for the audit log
// instead of a socket and stdout.
type deps struct {
	log    *slog.Logger
	audit  io.Writer
	docker server.DockerClient
}

// agent is a built agent that is not serving yet: everything run assembles
// from its configuration, split from serve so the two can be tested apart —
// buildServer for "this configuration produces this wiring", serve and
// shutdown for the order the agent stops in.
type agent struct {
	log  *slog.Logger
	srv  drainer
	grpc grpcServer

	// metrics is the /metrics endpoint, nil when disabled.
	metrics *http.Server
	// background starts the caches and the prune scheduler; they run until
	// the context serve gives them ends.
	background func(context.Context)

	drainTimeout    time.Duration
	gracefulTimeout time.Duration // see gracefulStopTimeout
	handlerTimeout  time.Duration // see handlerExitTimeout
}

// drainer is the part of *server.Server that shutdown uses.
type drainer interface {
	StartDrain()
	WaitForSessions(ctx context.Context) bool
	ActiveSessions() int64
}

// grpcServer is the part of *grpc.Server that serve and shutdown use.
type grpcServer interface {
	grpcStopper
	Serve(net.Listener) error
}

// buildServer assembles the agent from its configuration: the audit log, the
// TLS config (self-signed with SANs from the Docker node info, or provisioned),
// the authorizer, the server with its options, the shared-secret check and the
// interceptor chain. It starts nothing — no listener, no goroutine; serve does.
func buildServer(cfg *config.Config, d deps) (*agent, error) {
	log := d.log
	// Identity is only verifiable when a client certificate is required and
	// checked against a CA. In self-signed shared-secret mode the operator name
	// is whatever the client put in a header, so say so on every record.
	identityVerified := !(cfg.SelfSigned && cfg.CACert == "")
	auditLog := audit.New(slog.New(slog.NewJSONHandler(d.audit, &slog.HandlerOptions{Level: slog.LevelInfo})), identityVerified)

	// Resolve the optional shared secret (file or value).
	secret, err := cfg.AgentSecretValue()
	if err != nil {
		return nil, err
	}

	// Build the TLS config: either a self-signed cert generated here (SANs from
	// the Docker node info) or a provisioned server cert + client CA.
	var tlsCfg *tls.Config
	if cfg.SelfSigned {
		ictx, icancel := context.WithTimeout(context.Background(), 5*time.Second)
		sans := gatherSANs(ictx, d.docker, cfg, log)
		icancel()
		var sanDesc []string
		tlsCfg, sanDesc, err = tlsconf.SelfSignedServerConfig(sans, cfg.CACert, time.Now())
		if err != nil {
			return nil, err
		}
		log.Info("using self-signed server certificate",
			"sans", sanDesc,
			"verify_client_certs", cfg.CACert != "",
			"shared_secret", secret != "",
		)
	} else {
		tlsCfg, err = tlsconf.ServerConfig(cfg.CACert, cfg.ServerCert, cfg.ServerKey)
		if err != nil {
			return nil, err
		}
	}

	// Optional Prometheus metrics endpoint; serve starts it.
	var sink server.Metrics = server.NopMetrics{}
	var metricsSrv *http.Server
	if cfg.MetricsAddr != "" {
		prom := metrics.New(version.Version, version.Protocol)
		sink = prom
		mux := http.NewServeMux()
		mux.Handle("/metrics", prom.Handler())
		metricsSrv = newMetricsServer(cfg.MetricsAddr, mux)
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
			return nil, err
		}
		authz = p
		log.Info("authorization policy loaded", "file", cfg.PolicyFile)
	}

	srv := server.New(d.docker, authz, auditLog, log, sink, server.Options{
		IdleTimeout:        cfg.IdleTimeout,
		MaxSessionTime:     cfg.MaxSessionTime,
		SecretAuth:         secretAuthIdentity,
		ForwardImage:       cfg.ForwardImage,
		MaxStreams:         cfg.MaxStreams,
		MaxForwardSidecars: cfg.MaxForwardSidecars,
	})

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
	var secretAuth *server.SecretAuth
	if secret != "" {
		// The certificate a client's proof is computed over. Taken from the TLS
		// config actually in use, so it is right for both the self-signed and
		// the provisioned-cert paths.
		var certDER []byte
		if len(tlsCfg.Certificates) > 0 && len(tlsCfg.Certificates[0].Certificate) > 0 {
			certDER = tlsCfg.Certificates[0].Certificate[0]
		}
		secretAuth = &server.SecretAuth{
			Secret:      secret,
			CertDER:     certDER,
			AllowLegacy: cfg.AllowLegacySecret,
			Log:         log,
		}
		log.Info("shared-secret authentication enabled",
			"connection_bound", len(certDER) > 0, "legacy_raw_secret_accepted", cfg.AllowLegacySecret)
	}
	serverOpts = append(serverOpts, interceptors(log, secretAuth, srv)...)
	grpcSrv := grpc.NewServer(serverOpts...)
	pb.RegisterAgentServer(grpcSrv, srv)

	return &agent{
		log:     log,
		srv:     srv,
		grpc:    grpcSrv,
		metrics: metricsSrv,
		background: func(ctx context.Context) {
			// Background volume-size cache: scans at startup, then refreshes
			// on volume create/destroy events and periodically, so
			// ListVolumes(with_size) answers from memory instead of running a
			// slow du-style scan per request.
			srv.StartVolumeSizeCache(ctx)
			// Background container-stats sampler for the Stats RPC. Unlike the
			// volume cache it idles unless a client is actually asking, so an
			// agent nobody is watching costs nothing.
			srv.StartStatsCache(ctx)
			// What a prune would free, measured hourly, and the prune schedule
			// read from this agent's own container labels (swarmexec.prune.*).
			// Invalid labels disable the schedule; they never stop the agent.
			srv.StartReclaimCache(ctx)
			srv.StartPruneScheduler(ctx)
		},
		drainTimeout:    cfg.DrainTimeout,
		gracefulTimeout: gracefulStopTimeout,
		handlerTimeout:  handlerExitTimeout,
	}, nil
}

// serve starts the background work and the metrics endpoint, serves gRPC on
// lis, and on the first value from stop shuts down (see shutdown). It returns
// the serve error if serving fails first, nil after a shutdown.
func (a *agent) serve(lis net.Listener, stop <-chan os.Signal) error {
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	a.background(bgCtx)

	if a.metrics != nil {
		go func() {
			a.log.Info("metrics endpoint listening", "addr", a.metrics.Addr)
			if err := a.metrics.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				a.log.Error("metrics server failed", "err", err)
			}
		}()
	}

	serveErr := make(chan error, 1)
	go func() {
		a.log.Info("gRPC server listening", "addr", lis.Addr().String())
		serveErr <- a.grpc.Serve(lis)
	}()

	select {
	case err := <-serveErr:
		return err
	case sig := <-stop:
		a.shutdown(sig)
		return nil
	}
}

// shutdown is the stop sequence, and its order is the point (R-01):
//
//  1. StartDrain — new streams are refused with Unavailable from here on;
//  2. wait up to drainTimeout for the running streams to end;
//  3. drained: GracefulStop, bounded by gracefulTimeout and falling back to
//     Stop, because it also waits for unary RPCs and a client that never
//     hangs up must not turn the graceful path into a SIGKILL;
//     not drained: Stop at once, then wait up to handlerTimeout for the
//     cancelled handlers to return and write their end-of-stream audit
//     records before the audit writer closes;
//  4. the metrics endpoint last, so the drain stays observable to the end.
//
// Swarm sends SIGKILL after the service's stop_grace_period (10s by default),
// so the drain window plus either bound has to finish well inside it.
func (a *agent) shutdown(sig os.Signal) {
	a.log.Info("shutdown signal received; draining", "signal", sig.String(), "drain_timeout", a.drainTimeout)
	a.srv.StartDrain()

	drainCtx, cancel := context.WithTimeout(context.Background(), a.drainTimeout)
	defer cancel()
	if a.srv.WaitForSessions(drainCtx) {
		a.log.Info("all sessions drained; stopping gracefully")
		if !stopGracefully(a.grpc, a.gracefulTimeout) {
			a.log.Warn("graceful stop timed out; forced stop", "timeout", a.gracefulTimeout)
		}
	} else {
		a.log.Warn("drain window elapsed; forcing stop", "active_sessions", a.srv.ActiveSessions())
		a.grpc.Stop()
		exitCtx, c := context.WithTimeout(context.Background(), a.handlerTimeout)
		if !a.srv.WaitForSessions(exitCtx) {
			a.log.Warn("streams still running after forced stop", "active_sessions", a.srv.ActiveSessions())
		}
		c()
	}

	if a.metrics != nil {
		shutCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = a.metrics.Shutdown(shutCtx)
	}
	a.log.Info("agent stopped")
}

// interceptors builds the server's interceptor chains. Order, outermost first
// — each ChainXInterceptor option appends to its chain, so the order of the
// options is the order of execution:
//
//  1. recovery — first, so a panic anywhere below, authentication included,
//     costs one RPC, not every session on the node;
//  2. the shared-secret check, when a secret is configured (secretAuth != nil)
//     — before the stream gate, so a caller without the secret can neither
//     take a stream slot nor hold up a drain, and is told Unauthenticated even
//     while the agent drains or sits at its cap, as it always was;
//  3. the stream gate (streams only) — drain check, stream slot, drain
//     accounting and per-stream metrics, before the handler's first Recv.
//
// Authorization is not an interceptor: it needs the container's service, which
// only the handler resolves.
func interceptors(log *slog.Logger, secretAuth *server.SecretAuth, srv *server.Server) []grpc.ServerOption {
	unary := []grpc.UnaryServerInterceptor{server.RecoveryUnaryInterceptor(log)}
	stream := []grpc.StreamServerInterceptor{server.RecoveryStreamInterceptor(log)}
	if secretAuth != nil {
		unary = append(unary, secretAuth.UnaryInterceptor())
		stream = append(stream, secretAuth.StreamInterceptor())
	}
	stream = append(stream, srv.StreamInterceptor())
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
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

// newMetricsServer builds the /metrics HTTP server. Without timeouts a client
// that opens a connection and never finishes its headers holds it forever; a
// scrape is one small GET, so tight bounds cost a real Prometheus nothing.
// IdleTimeout stays above common scrape intervals so keep-alive still works.
func newMetricsServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
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

// infoSource is what gatherSANs needs of the Docker client.
type infoSource interface {
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)
}

// gatherSANs builds the SAN list for the self-signed server cert: a stable
// "swarmexec-agent" name (so clients can pin it via --server-name), loopback,
// this node's hostname and advertised Swarm address (from the Docker API), and
// any operator-supplied extras.
func gatherSANs(ctx context.Context, cli infoSource, cfg *config.Config, log *slog.Logger) []string {
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
