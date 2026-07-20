// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package server implements the Agent gRPC service: container discovery and the
// bidirectional interactive exec bridge defined in CONTRACT.md.
package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
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
}

// Server is the Agent gRPC service implementation.
type Server struct {
	pb.UnimplementedAgentServer

	docker  DockerClient
	authz   auth.Authorizer
	audit   *audit.Logger
	log     *slog.Logger
	metrics Metrics
	opts    Options

	// identityFn extracts the authenticated client identity from the RPC
	// context. Overridable in tests; defaults to the mTLS peer CN.
	identityFn func(context.Context) (string, error)

	// draining is set during graceful shutdown; new Exec sessions are rejected.
	draining atomic.Bool
	// wg tracks in-flight Exec sessions so shutdown can wait for them to drain.
	wg sync.WaitGroup

	active atomic.Int64

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
	if opts.SecretAuth {
		s.identityFn = identityFromContextLenient
	} else {
		s.identityFn = identityFromPeer
	}
	return s
}

// ListContainers lists running containers on the local node, optionally
// filtered by a service-name or container-name substring.
func (s *Server) ListContainers(ctx context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	containers, err := s.docker.ContainerList(ctx, container.ListOptions{All: false})
	if err != nil {
		s.log.Error("ContainerList failed", "err", err)
		return nil, status.Errorf(codes.Internal, "list containers: %v", err)
	}

	filter := strings.TrimSpace(req.GetServiceFilter())
	resp := &pb.ListResponse{}
	for _, c := range containers {
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
		})
	}
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
func namedVolumes(mounts []types.MountPoint) []string {
	var out []string
	for _, m := range mounts {
		if m.Type == mount.TypeVolume && m.Name != "" {
			out = append(out, m.Name)
		}
	}
	return out
}

// resolveService inspects the container to recover its swarm service name. A
// failure here is non-fatal: it returns "" so authorization and audit still
// proceed with whatever identity/container information is available.
func (s *Server) resolveService(ctx context.Context, containerID string) string {
	info, err := s.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		s.log.Warn("ContainerInspect failed; service name unresolved", "container_id", containerID, "err", err)
		return ""
	}
	if info.Config == nil {
		return ""
	}
	return info.Config.Labels[swarmServiceLabel]
}

// StartDrain marks the server as draining so new Exec sessions are refused with
// Unavailable. Existing sessions continue until they end or are forcibly stopped.
func (s *Server) StartDrain() {
	s.draining.Store(true)
}

// WaitForSessions blocks until all in-flight Exec sessions have ended or ctx is
// cancelled. It returns true if all sessions drained, false on ctx timeout.
func (s *Server) WaitForSessions(ctx context.Context) bool {
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

// ActiveSessions reports the current number of live exec sessions.
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
