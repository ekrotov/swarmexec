// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

const (
	// forwardLabel marks sidecar containers so they are obvious in `docker ps`.
	//
	// This comment used to also claim that a restarted agent sweeps the ones its
	// predecessor leaked. No such sweep exists anywhere in the code — it was a
	// description of an intention, and it read as a guarantee. Sidecars are
	// removed on teardown and again on graceful shutdown; a killed agent can
	// still leak one per forward, and the label is how an operator finds those.
	forwardLabel = "swarmexec.role"
	forwardValue = "port-forward"

	// forwardStartTimeout bounds sidecar creation and the target dial. Sidecar
	// start measured ~300ms on a warm node (DESIGN-port-forward.md §3).
	forwardStartTimeout = 20 * time.Second

	// forwardRemoveTimeout bounds the best-effort sidecar cleanup, which runs on
	// its own context because the stream context is usually already cancelled.
	forwardRemoveTimeout = 10 * time.Second
)

// PortForward bridges one client TCP connection to a port inside a container on
// this node. One gRPC stream carries exactly one connection (CONTRACT.md §3).
func (s *Server) PortForward(stream pb.Agent_PortForwardServer) error {
	if s.draining.Load() {
		return status.Error(codes.Unavailable, "agent is shutting down; not accepting new forwards")
	}

	// Stream slot before the first Recv, which blocks on the client. The sidecar
	// slot is taken later, in openSidecarChannel: only the sidecar path creates a
	// container, and a forward that reaches the port directly must not consume
	// from the container budget.
	release, err := s.streams.acquire()
	if err != nil {
		s.log.Warn("port-forward refused: stream limit reached", "in_use", s.streams.inUse())
		return err
	}
	defer release()

	// (1) The first message MUST be StartForward.
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "stream closed before StartForward")
		}
		return err
	}
	start, ok := first.Payload.(*pb.ForwardClientMessage_Start)
	if !ok || start.Start == nil {
		return status.Error(codes.InvalidArgument, "first message must be StartForward")
	}
	sf := start.Start
	if sf.GetContainerId() == "" {
		return status.Error(codes.InvalidArgument, "StartForward requires container_id")
	}
	if p := sf.GetPort(); p == 0 || p > 65535 {
		return status.Errorf(codes.InvalidArgument, "port %d out of range 1-65535", p)
	}

	// (2) Identity, service and authorization — the shared per-RPC gate.
	// Forwarding is its own action: it moves an internal port onto the operator's
	// machine, so a policy may deny it while allowing exec.
	identity, service, err := s.authorize(stream.Context(), auth.Request{
		Action:      "portforward",
		ContainerID: sf.GetContainerId(),
		Port:        sf.GetPort(),
	})
	if err != nil {
		return err
	}

	return s.runForward(stream, sf, identity, service)
}

// runForward establishes the byte channel to the target port and bridges it to
// the gRPC stream until either side closes.
func (s *Server) runForward(stream pb.Agent_PortForwardServer, sf *pb.StartForward, identity, service string) error {
	s.wg.Add(1)
	defer s.wg.Done()
	s.active.Add(1)
	defer s.active.Add(-1)
	begin := time.Now()

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	// (5) Open the channel to the target port. The sidecar path is the general
	// one; see DESIGN-port-forward.md §2 for why a direct dial is not enough.
	ch, sidecar, err := s.openForwardChannel(ctx, sf.GetContainerId(), sf.GetPort())
	if err != nil {
		_ = stream.Send(forwardErrorMsg(err.Error()))
		s.audit.ForwardStart(identity, sf.GetContainerId(), service, sf.GetPort(), sidecar, peerAddr(stream.Context()))
		s.audit.ForwardEnd(identity, sf.GetContainerId(), sf.GetPort(), time.Since(begin), 0, 0)
		s.log.Error("port-forward setup failed",
			"container_id", sf.GetContainerId(), "port", sf.GetPort(), "err", err)
		return forwardStatus(err)
	}
	defer ch.Close()

	s.audit.ForwardStart(identity, sf.GetContainerId(), service, sf.GetPort(), sidecar, peerAddr(stream.Context()))

	// (6) The target accepted; tell the client before any data so it can
	// distinguish "connected" from "connected but silent".
	if err := stream.Send(&pb.ForwardServerMessage{
		Payload: &pb.ForwardServerMessage_Ready{Ready: &pb.ForwardReady{}},
	}); err != nil {
		return err
	}

	var bytesIn, bytesOut atomic.Int64

	// Closing the channel unblocks the reader when the client disconnects.
	go func() {
		<-ctx.Done()
		ch.Close()
	}()

	// Client -> target.
	go func() {
		for {
			msg, rerr := stream.Recv()
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					// Client half-closed: EOF to the target, and keep reading
					// back. This goroutine must NOT cancel here, which is what
					// it used to do — a request/response exchange ends exactly
					// this way (send, shutdown(WR), read the answer), and
					// cancelling tore down the channel while the answer was
					// still in it. The read direction now ends where it should:
					// when the target closes, or when the client disconnects
					// for real and the stream context is cancelled.
					ch.CloseWrite()
					return
				}
				// Anything else means the client is gone, so nothing is coming
				// back either.
				cancel()
				return
			}
			d, ok := msg.Payload.(*pb.ForwardClientMessage_Data)
			if !ok {
				// A second StartForward mid-stream is a protocol violation.
				s.log.Warn("ignoring unexpected message mid-forward",
					"container_id", sf.GetContainerId())
				continue
			}
			// Counted before the write: "in" means bytes received from the
			// operator, which is true regardless of whether the target still
			// accepts them. Counting after would also let the teardown path
			// read the counter before this goroutine got to update it.
			bytesIn.Add(int64(len(d.Data)))
			if _, werr := ch.Write(d.Data); werr != nil {
				cancel()
				return
			}
		}
	}()

	// Target -> client. Blocks until the target closes or the channel is torn down.
	copyErr := ch.CopyTo(&forwardWriter{stream: stream, count: &bytesOut})
	cancel()

	dur := time.Since(begin)
	bIn, bOut := bytesIn.Load(), bytesOut.Load()

	clientGone := stream.Context().Err() != nil
	if !clientGone && copyErr != nil && !errors.Is(copyErr, io.EOF) && !errors.Is(copyErr, net.ErrClosed) {
		_ = stream.Send(forwardErrorMsg("forward stream error: " + copyErr.Error()))
	}

	s.audit.ForwardEnd(identity, sf.GetContainerId(), sf.GetPort(), dur, bIn, bOut)
	s.metrics.BytesTransferred(bIn, bOut)

	if clientGone {
		return status.FromContextError(stream.Context().Err()).Err()
	}
	return nil
}

// forwardChannel is a bidirectional byte channel to a port inside a container.
// Both the sidecar and (future) direct-dial paths satisfy it.
type forwardChannel interface {
	io.Writer
	// CopyTo pumps target output into w until the target closes.
	CopyTo(w io.Writer) error
	// CloseWrite signals EOF to the target without closing the read direction.
	CloseWrite()
	// Close tears the channel down; safe to call more than once.
	Close()
}

// forwardStatus keeps a gRPC code the setup already chose, and supplies
// Internal only for a plain error. Without it the sidecar cap would reach the
// client as Internal — "the agent is broken" — instead of ResourceExhausted,
// which says the node is busy and the call is worth retrying.
func forwardStatus(err error) error {
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}

// openForwardChannel reaches the target port. It reports whether a sidecar was
// used so the audit record can distinguish the two paths.
func (s *Server) openForwardChannel(ctx context.Context, containerID string, port uint32) (forwardChannel, bool, error) {
	// One sidecar per (container, port), shared by every connection of that
	// forward — so the ~300 ms container start is paid once per forward instead
	// of once per TCP connection. See forwardmux.go.
	c, err := s.muxes.openConn(ctx, s, containerID, port)
	if err != nil {
		return nil, true, err
	}
	return c, true, nil
}

// sidecarChannel bridges to a container joined to the target's network
// namespace. Its stdout carries payload bytes; its stderr is an out-of-band
// control channel (see agent -forward-to). The attach stream is NOT a TTY, so
// docker multiplexes the two with stdcopy framing and binary payloads survive
// intact — a TTY would apply line-discipline translation and corrupt them.
// The demux runs exactly once for the channel's lifetime: stdout is piped to
// CopyTo, stderr is parsed for the readiness line and then logged. Demuxing
// twice would consume the payload the second reader is waiting for.
type sidecarChannel struct {
	docker DockerClient
	id     string
	attach types.HijackedResponse
	log    *slog.Logger

	// stdoutR carries payload from the demux goroutine to CopyTo.
	stdoutR *io.PipeReader
	// ready delivers the sidecar's first control line exactly once.
	ready chan error

	// release returns this sidecar's slot in the node's container budget. It is
	// called from Close, so the slot is held for exactly as long as the
	// container can exist — not merely while the stream is being set up.
	release func()

	closeOnce sync.Once
}

func (c *sidecarChannel) Write(p []byte) (int, error) { return c.attach.Conn.Write(p) }

func (c *sidecarChannel) CloseWrite() {
	if c.attach.Conn == nil {
		return
	}
	if err := c.attach.CloseWrite(); err != nil {
		c.log.Debug("sidecar close-write failed", "sidecar", c.id, "err", err)
	}
}

// Read consumes the sidecar's stdout, already separated from its stderr by the
// stdcopy demux. In mux mode those bytes are FRAMES, not payload — the reader
// is internal/forwardmux, not a copier.
func (c *sidecarChannel) Read(p []byte) (int, error) { return c.stdoutR.Read(p) }

func (c *sidecarChannel) CopyTo(w io.Writer) error {
	_, err := io.Copy(w, c.stdoutR)
	return err
}

// startDemux splits the attach stream once: stdout is payload, stderr is the
// control channel (one readiness line, then diagnostics for the agent log).
func (c *sidecarChannel) startDemux() {
	pr, pw := io.Pipe()
	c.stdoutR = pr
	c.ready = make(chan error, 1)

	ctrl := &sidecarControl{log: c.log, id: c.id, ready: c.ready}
	go func() {
		_, err := stdcopy.StdCopy(pw, ctrl, c.attach.Reader)
		if err == nil {
			err = io.EOF
		}
		_ = pw.CloseWithError(err)
		// The sidecar died without ever reporting readiness.
		ctrl.fail(errors.New("forward sidecar exited before reporting readiness"))
	}()
}

func (c *sidecarChannel) Close() {
	c.closeOnce.Do(func() {
		// Close may run before attach succeeded, e.g. when start failed.
		if c.attach.Conn != nil {
			c.attach.Close()
		}
		// The stream context is typically already cancelled by now, so removal
		// gets its own. AutoRemove usually wins the race; this is the backstop.
		rctx, cancel := context.WithTimeout(context.Background(), forwardRemoveTimeout)
		defer cancel()
		if err := c.docker.ContainerRemove(rctx, c.id, container.RemoveOptions{Force: true}); err != nil {
			if !isNotFoundErr(err) {
				c.log.Warn("sidecar removal failed; it may linger", "sidecar", c.id, "err", err)
			}
		}
		// Last: the slot is only free once the container is actually gone.
		if c.release != nil {
			c.release()
		}
	})
}

// openSidecarChannel creates a container in the target's network namespace,
// attaches to its stdio, and waits for it to report a successful dial.
func (s *Server) openSidecarChannel(ctx context.Context, containerID string, port uint32) (*sidecarChannel, error) {
	// A container budget of its own, taken before anything is created. This is
	// the expensive resource on the node: today one TCP connection through a
	// forward is one container, so "open many connections" is literally "start
	// many containers", and the node runs out of PIDs and memory long before it
	// runs out of gRPC streams.
	releaseSlot, err := s.sidecars.acquire()
	if err != nil {
		s.log.Warn("port-forward refused: sidecar limit reached",
			"in_use", s.sidecars.inUse(), "container_id", containerID, "port", port)
		return nil, err
	}

	image, err := s.forwardImage(ctx)
	if err != nil {
		releaseSlot()
		return nil, err
	}

	sctx, cancel := context.WithTimeout(ctx, forwardStartTimeout)
	defer cancel()

	cfg := &container.Config{
		Image: image,
		// Mux mode: this one container serves every connection of the forward.
		// Its stdout therefore carries FRAMES, not payload (internal/forwardmux)
		// — the single most important thing to remember on this path.
		Cmd:          []string{"-forward-mux-to", fmt.Sprintf("127.0.0.1:%d", port)},
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		OpenStdin:    true,
		StdinOnce:    true,
		Tty:          false, // never: a pty would mangle binary payloads
		Labels: map[string]string{
			forwardLabel: forwardValue,
			// Recorded for operators reading `docker ps` on the node.
			"swarmexec.target": containerID,
			"swarmexec.port":   fmt.Sprintf("%d", port),
		},
	}
	hostCfg := &container.HostConfig{
		// The whole point: share the target's netns so 127.0.0.1 is the target.
		NetworkMode:    container.NetworkMode("container:" + containerID),
		AutoRemove:     true,
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
	}

	created, err := s.docker.ContainerCreate(sctx, cfg, hostCfg, nil, nil, "")
	if err != nil {
		releaseSlot()
		return nil, fmt.Errorf("create forward sidecar: %w", err)
	}

	// From here the slot is owned by the channel: every failure path below goes
	// through ch.Close, which releases it. Releasing here as well would return
	// the slot twice — hence the idempotent release from limiter.acquire.
	ch := &sidecarChannel{docker: s.docker, id: created.ID, log: s.log, release: releaseSlot}

	// Attach before start so no output is missed.
	attach, err := s.docker.ContainerAttach(sctx, created.ID, container.AttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		ch.Close()
		return nil, fmt.Errorf("attach forward sidecar: %w", err)
	}
	ch.attach = attach
	ch.startDemux()

	if err := s.docker.ContainerStart(sctx, created.ID, container.StartOptions{}); err != nil {
		ch.Close()
		return nil, fmt.Errorf("start forward sidecar: %w", err)
	}

	// The sidecar dials the target and reports the outcome on stderr. Waiting
	// for it is what makes ForwardReady meaningful.
	if err := ch.awaitReady(sctx, port); err != nil {
		ch.Close()
		return nil, err
	}
	return ch, nil
}

// awaitReady blocks until the sidecar reports the outcome of its dial.
func (c *sidecarChannel) awaitReady(ctx context.Context, port uint32) error {
	select {
	case err := <-c.ready:
		if err != nil {
			return fmt.Errorf("port %d in the target container: %w", port, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for forward sidecar to reach port %d", port)
	}
}

// sidecarControl parses the sidecar's stderr. The protocol is one line —
// "ok" or "error: <reason>" — after which everything is a diagnostic.
type sidecarControl struct {
	log   *slog.Logger
	id    string
	ready chan error

	once sync.Once
	buf  []byte
}

func (w *sidecarControl) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line == "" {
			continue
		}
		var reported bool
		w.once.Do(func() {
			reported = true
			switch {
			case line == "ok", line == "mux ok":
				w.ready <- nil
			case strings.HasPrefix(line, "error: "):
				w.ready <- errors.New(strings.TrimPrefix(line, "error: "))
			default:
				w.ready <- fmt.Errorf("sidecar sent unexpected readiness line %q", line)
			}
		})
		if !reported {
			w.log.Debug("forward sidecar", "sidecar", w.id, "msg", line)
		}
	}
	return len(p), nil
}

// fail reports a readiness failure if nothing was reported yet; a no-op once
// the sidecar has already spoken.
func (w *sidecarControl) fail(err error) {
	w.once.Do(func() { w.ready <- err })
}

// forwardImage returns the image to run sidecars from: the agent's own, so it
// is guaranteed present on the node and a forward never waits on a pull.
func (s *Server) forwardImage(ctx context.Context) (string, error) {
	if s.opts.ForwardImage != "" {
		return s.opts.ForwardImage, nil
	}
	s.forwardImageOnce.Do(func() {
		host, err := os.Hostname()
		if err != nil {
			s.forwardImageErr = fmt.Errorf("determine own hostname: %w", err)
			return
		}
		self, err := s.docker.ContainerInspect(ctx, host)
		if err != nil {
			s.forwardImageErr = fmt.Errorf(
				"cannot determine the agent's own image (inspect %q: %w) — "+
					"port-forward runs sidecars from it; set -forward-image to override", host, err)
			return
		}
		// Config is a pointer and the daemon is not contractually obliged to
		// populate it; every other inspect call site here guards it. Without the
		// guard a malformed response panics the RPC goroutine inside a sync.Once,
		// so the failure is both a crash and permanently cached.
		if self.Config == nil || self.Config.Image == "" {
			s.forwardImageErr = fmt.Errorf(
				"cannot determine the agent's own image (inspect %q returned no image) — "+
					"port-forward runs sidecars from it; set -forward-image to override", host)
			return
		}
		s.forwardImageVal = self.Config.Image
	})
	if s.forwardImageErr != nil {
		return "", s.forwardImageErr
	}
	return s.forwardImageVal, nil
}

// forwardWriter adapts an io.Writer onto the forward stream, copying out of the
// caller's reused buffer first (CONTRACT.md §6 buffer-safety rule).
type forwardWriter struct {
	stream pb.Agent_PortForwardServer
	count  *atomic.Int64
}

func (w *forwardWriter) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)
	if err := w.stream.Send(&pb.ForwardServerMessage{
		Payload: &pb.ForwardServerMessage_Data{Data: buf},
	}); err != nil {
		return 0, err
	}
	w.count.Add(int64(len(p)))
	return len(p), nil
}

func forwardErrorMsg(text string) *pb.ForwardServerMessage {
	return &pb.ForwardServerMessage{Payload: &pb.ForwardServerMessage_Error{Error: text}}
}

func isNotFoundErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container")
}
