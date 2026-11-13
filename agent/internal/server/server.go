// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package server implements the Agent gRPC service defined in CONTRACT.md:
// container discovery, the interactive exec bridge, logs and container events,
// port forwarding, volumes, images, resource stats, prune and version.
package server

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/audit"
	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// Options tunes per-session behavior.
type Options struct {
	// IdleTimeout aborts a session after this much inactivity in both
	// directions. Zero disables the check (interactive shells idle legitimately).
	IdleTimeout time.Duration
	// MaxSessionTime aborts a session after this total duration. Zero disables.
	MaxSessionTime time.Duration
	// SecretAuth selects lenient identity resolution: when no client certificate
	// is required (self-signed + shared-secret mode), the audit identity falls
	// back to a client-supplied operator header or "anonymous" instead of
	// failing. Leave false for CA-verified mTLS, where a cert CN is mandatory.
	SecretAuth bool
	// ForwardImage overrides the image port-forward sidecars run from. Empty
	// means "the agent's own image", discovered by self-inspection, which is
	// right on every normal deployment: it is already present on the node, so a
	// forward never blocks on a registry pull.
	ForwardImage string
	// VolumeSizeInterval sets the volume-size cache's periodic rescan interval.
	// Zero uses the default (volumeSizeInterval).
	VolumeSizeInterval time.Duration
	// StatsInterval sets how often the container-stats sampler takes a reading —
	// also the resolution of every CPU percentage, which is a delta between two
	// of them. Zero uses the default (statsInterval).
	StatsInterval time.Duration
	// MaxStreams caps concurrent Exec + Logs + PortForward streams on this node.
	// Zero uses the default (defaultMaxStreams); negative disables the cap.
	MaxStreams int
	// MaxForwardSidecars caps live port-forward sidecar containers on this node.
	// Zero uses the default (defaultMaxForwardSidecars); negative disables it.
	MaxForwardSidecars int
}

// Server is the Agent gRPC service implementation.
type Server struct {
	pb.UnimplementedAgentServer

	docker     DockerClient
	authz      auth.Authorizer
	audit      *audit.Logger
	log        *slog.Logger
	metrics    Metrics
	opts       Options
	sizeCache  *volumeSizeCache     // serves ListVolumes(with_size) from memory
	statsCache *containerStatsCache // serves Stats from memory (see statscache.go)
	reclaim    *reclaimCache        // serves ReclaimStats from memory (see reclaim.go)
	scheduler  *pruneScheduler      // the node's prune schedule (see schedule.go)

	// pruneMu serializes prunes, manual and scheduled (see runPrune).
	pruneMu sync.Mutex

	// identityFn extracts the authenticated client identity from the RPC
	// context. Overridable in tests; defaults to the mTLS peer CN.
	identityFn func(context.Context) (string, error)

	// draining is set during graceful shutdown; new streams (Exec, Logs,
	// PortForward, WatchContainerEvents) are rejected.
	draining atomic.Bool
	// wg tracks every in-flight stream — exec sessions, port-forwards, log
	// streams and event watches — so shutdown can wait for them to drain and,
	// after a forced stop, for each to write its end-of-stream audit record.
	wg sync.WaitGroup

	// active counts the streams wg tracks, for the shutdown log.
	active atomic.Int64

	// streams caps concurrent streaming RPCs; sidecars caps the containers a
	// port-forward creates. Two limits because they bound different things: a
	// stream costs a goroutine, a sidecar costs a container on the node.
	streams  *limiter
	sidecars *limiter

	// muxes holds one port-forward sidecar per (container, port), shared by
	// every connection of that forward.
	muxes *muxPool

	// The agent's own image, resolved once by self-inspection and reused by
	// every port-forward sidecar. See forwardImage.
	forwardImageOnce sync.Once
	forwardImageVal  string
	forwardImageErr  error
}

// New constructs a Server. metrics may be nil, in which case a no-op sink is used.
func New(docker DockerClient, authz auth.Authorizer, auditLog *audit.Logger, log *slog.Logger, metrics Metrics, opts Options) *Server {
	if metrics == nil {
		metrics = NopMetrics{}
	}
	s := &Server{
		docker:  docker,
		authz:   authz,
		audit:   auditLog,
		log:     log,
		metrics: metrics,
		opts:    opts,
	}
	s.sizeCache = newVolumeSizeCache(docker, log, opts.VolumeSizeInterval)
	s.statsCache = newContainerStatsCache(docker, log, opts.StatsInterval)
	s.reclaim = newReclaimCache(s, log)
	s.scheduler = newPruneScheduler(s, log)
	s.muxes = newMuxPool()
	s.streams = newLimiter(orDefault(opts.MaxStreams, defaultMaxStreams), "streams", "-max-streams")
	s.sidecars = newLimiter(orDefault(opts.MaxForwardSidecars, defaultMaxForwardSidecars),
		"port-forward sidecars", "-max-forward-sidecars")
	s.streams.refused = func() { s.metrics.LimitRefused("streams") }
	s.sidecars.refused = func() { s.metrics.LimitRefused("forward_sidecars") }
	if opts.SecretAuth {
		s.identityFn = identityFromContextLenient
	} else {
		s.identityFn = identityFromPeer
	}
	return s
}

// StartVolumeSizeCache launches the background volume-size cache: an initial
// scan, then reactive (volume create/destroy) and periodic refreshes, until ctx
// is cancelled. Call it once after New.
func (s *Server) StartVolumeSizeCache(ctx context.Context) {
	go s.sizeCache.run(ctx)
}

// StartStatsCache launches the background container-stats sampler. Unlike the
// volume cache it only does work while a client is actually asking for stats
// (see statsIdleAfter), so an agent nobody is watching stays idle. Call it once
// after New.
func (s *Server) StartStatsCache(ctx context.Context) {
	go s.statsCache.run(ctx)
}

// StartReclaimCache launches the background reclaim measurement: once shortly
// after start, then hourly, and after every prune. Call it once after New.
func (s *Server) StartReclaimCache(ctx context.Context) {
	go s.reclaim.run(ctx)
}

// ListContainers lists running containers on the local node, optionally
// filtered by a service-name or container-name substring.
//
// Authorized and audited like every other RPC, which it once was not. It is
// gated by transport auth either way, but skipping the policy hook meant the
// pluggable Authorizer could never restrict discovery — and discovery is what
// an operator needs before they can target anything: this call returns the
// container ids and service names that exec, logs and port-forward take as
// input. It is also the cheapest possible reconnaissance, since it inspects
// nothing.
//
// It does not go through s.authorize: that helper resolves the swarm service by
// inspecting a container, and this RPC has no container to inspect.
// authorizeUnary is the same identity → authorize → audit sequence without
// that step.
func (s *Server) ListContainers(ctx context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	identity, err := s.authorizeUnary(ctx, auth.Request{Action: "container.list"})
	if err != nil {
		return nil, err
	}

	list, err := s.docker.ContainerList(ctx, client.ContainerListOptions{All: false})
	if err != nil {
		s.log.Error("ContainerList failed", "err", err)
		return nil, status.Errorf(codes.Internal, "list containers: %v", err)
	}

	filter := strings.TrimSpace(req.GetServiceFilter())
	resp := &pb.ListResponse{}
	for _, c := range list.Items {
		name := containerName(c.Names)
		service := c.Labels[swarmServiceLabel]
		if filter != "" && !strings.Contains(service, filter) && !strings.Contains(name, filter) {
			continue
		}
		resp.Containers = append(resp.Containers, &pb.ContainerInfo{
			Id:      c.ID,
			Name:    name,
			Service: service,
			Volumes: namedVolumes(c.Mounts),
			Ports:   tcpPorts(c.Ports),
		})
	}
	s.audit.ContainerList(identity, filter, len(resp.Containers))
	return resp, nil
}

// containerName returns the primary container name without its leading slash.
func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

// namedVolumes returns the names of the named volumes a container mounts,
// ignoring bind mounts and anonymous/tmpfs mounts (which have no volume name).
func namedVolumes(mounts []container.MountPoint) []string {
	var out []string
	for _, m := range mounts {
		if m.Type == mount.TypeVolume && m.Name != "" {
			out = append(out, m.Name)
		}
	}
	return out
}

// tcpPorts returns the container-side TCP ports of a container summary,
// deduplicated and ascending — the suggestion list a client offers for a port
// forward.
//
// The summary lists a port once per host binding (IPv4 and IPv6 separately for
// a published port) and also lists ports that are only EXPOSEd, with no binding
// at all; PrivatePort is the container side in both cases, which is the side a
// forward targets. UDP and SCTP are dropped because PortForward is TCP only.
func tcpPorts(ports []container.PortSummary) []uint32 {
	var out []uint32
	for _, p := range ports {
		if p.PrivatePort == 0 || (p.Type != "" && !strings.EqualFold(p.Type, "tcp")) {
			continue
		}
		out = append(out, uint32(p.PrivatePort))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// resolveService inspects the container to recover its swarm service name. A
// failure here is non-fatal: it returns "" so authorization and audit still
// proceed with whatever identity/container information is available.
func (s *Server) resolveService(ctx context.Context, containerID string) string {
	res, err := s.docker.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		s.log.Warn("ContainerInspect failed; service name unresolved", "container_id", containerID, "err", err)
		return ""
	}
	info := res.Container
	if info.Config == nil {
		return ""
	}
	return info.Config.Labels[swarmServiceLabel]
}

// authorize is the per-RPC security gate shared by the streaming handlers
// (Exec/Logs/PortForward): resolve the mTLS identity and the swarm service,
// authorize req — whose action-specific fields (Action, ContainerID, and any of
// Cmd/User/TTY/Port) the caller has already set — audit the decision, and count a
// denial. It fills req.Identity and req.Service. On failure it returns a ready
// gRPC status error (with empty identity/service) so callers just `return err`.
// Keeping the sequence in one place stops the drain/authorize/audit/deny order —
// and the audit of denied requests — from drifting between handlers.
func (s *Server) authorize(ctx context.Context, req auth.Request) (identity, service string, err error) {
	identity, err = s.identity(ctx)
	if err != nil {
		return "", "", err
	}
	service = s.resolveService(ctx, req.ContainerID)
	req.Identity = identity
	req.Service = service
	if reason, ok := s.decide(ctx, req); !ok {
		return "", "", status.Errorf(codes.PermissionDenied, "authorization denied: %s", reason)
	}
	return identity, service, nil
}

// authorizeUnary is the gate of the unary RPCs that act on the node rather than
// on one container (listings, volumes, images, stats, reclaim): resolve the
// identity, authorize req — whose Action and any Volume the caller has set —
// audit the decision and count a denial. It fills req.Identity and returns the
// identity, or a ready gRPC status error.
//
// One helper rather than the same nine lines in every handler: "every RPC is
// authorized and audited" is then a property of the code's shape, not of
// nobody ever forgetting a copy.
func (s *Server) authorizeUnary(ctx context.Context, req auth.Request) (string, error) {
	identity, err := s.identity(ctx)
	if err != nil {
		return "", err
	}
	req.Identity = identity
	if reason, ok := s.decide(ctx, req); !ok {
		return "", status.Errorf(codes.PermissionDenied, "authorization denied: %s", reason)
	}
	return identity, nil
}

// identity resolves the caller's identity, as an Unauthenticated status error
// when there is none.
func (s *Server) identity(ctx context.Context) (string, error) {
	identity, err := s.identityFn(ctx)
	if err != nil {
		return "", status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
	}
	return identity, nil
}

// decide asks the Authorizer about req (Identity already set), audits the
// decision with req's container and service, and counts a denial. It returns
// the Authorizer's reason and whether req is allowed. Every authorization in
// the agent goes through here, including each category of a Prune.
func (s *Server) decide(ctx context.Context, req auth.Request) (reason string, allowed bool) {
	decision := s.authz.Authorize(ctx, req)
	s.audit.AuthDecision(req.Identity, req.ContainerID, req.Service, decision.Allow, decision.Reason)
	if !decision.Allow {
		s.metrics.AuthDenied()
	}
	return decision.Reason, decision.Allow
}

// trackStream counts a stream into the drain (wg and active) for its lifetime;
// call the returned func when it ends. Exec and PortForward do this inline.
//
// Logs and WatchContainerEvents need it most: a follow or an open view never
// ends on its own, so a drain that did not count them reported "drained" at
// once and GracefulStop then waited on them without limit.
func (s *Server) trackStream() (done func()) {
	s.wg.Add(1)
	s.active.Add(1)
	return func() {
		s.active.Add(-1)
		s.wg.Done()
	}
}

// StartDrain marks the server as draining so new streams are refused with
// Unavailable. Existing ones continue until they end or are forcibly stopped.
func (s *Server) StartDrain() {
	s.draining.Store(true)
}

// WaitForSessions blocks until every in-flight stream (exec, port-forward,
// logs, event watch) has ended or ctx is cancelled. It returns true if all
// drained, false on ctx timeout. After a forced stop it is called again, so
// streams whose context was just cancelled get to write their end records.
//
// Forward sidecars are torn down either way: they are containers on the node,
// and a shutdown that left them running would leak one per forward the agent
// ever served. They linger for muxIdleLinger by design, so "no connections" is
// not the same as "nothing to clean up".
func (s *Server) WaitForSessions(ctx context.Context) bool {
	defer s.muxes.closeAll()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// ActiveSessions reports the current number of live streams the drain waits
// for (exec, port-forward, logs, event watches).
func (s *Server) ActiveSessions() int64 { return s.active.Load() }

// identityFromPeer extracts the verified client-certificate Common Name from
// the gRPC peer. It only succeeds when mTLS has produced a verified chain.
func identityFromPeer(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("no peer information in context")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", errors.New("connection is not mutually authenticated TLS")
	}
	chains := tlsInfo.State.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return "", errors.New("no verified client certificate chain")
	}
	cn := chains[0][0].Subject.CommonName
	if cn == "" {
		return "", errors.New("client certificate has empty common name")
	}
	return cn, nil
}

// peerAddr returns the client's network address for audit logging, or "" if
// unavailable.
func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

// compile-time assertion that *Server implements the generated service.
var _ pb.AgentServer = (*Server)(nil)
