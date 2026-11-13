// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/moby/moby/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/config"
	"swarmexec/internal/pb"
)

// healthcheckFlag selects the container health probe: `/agent -healthcheck`,
// run by the Docker daemon inside the agent's container (agent/Dockerfile
// HEALTHCHECK). The image is distroless — no shell, no curl — so the probe has
// to live in the binary.
const healthcheckFlag = "-healthcheck"

// Probe bounds. The whole probe stays under the HEALTHCHECK --timeout (10s), so
// a slow probe reports its own reason instead of being killed without one.
const (
	healthTotalTimeout = 8 * time.Second
	healthRPCTimeout   = 3 * time.Second
)

// healthcheckMode reports whether args ask for the health probe, and returns
// the remaining args (the probe's own flags, if any).
func healthcheckMode(args []string) ([]string, bool) {
	for i, a := range args {
		if a == healthcheckFlag || a == "-"+healthcheckFlag {
			rest := append([]string{}, args[:i]...)
			return append(rest, args[i+1:]...), true
		}
	}
	return nil, false
}

// runHealthcheck runs the probe and returns the process exit code: 0 healthy,
// 1 unhealthy. One line of output either way; Docker keeps it in the
// container's health log (docker inspect), which is where an operator looks
// first.
func runHealthcheck(args []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), healthTotalTimeout)
	defer cancel()
	msg, err := healthcheck(ctx, args, readPID1Cmdline)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	fmt.Println("healthy:", msg)
	return 0
}

// healthcheck resolves the agent's configuration the way the agent did and
// probes it: the gRPC listener first, then the Docker socket.
func healthcheck(ctx context.Context, args []string, cmdline func() ([]byte, error)) (string, error) {
	agentArgs, sidecar := probeArgs(args, cmdline)
	if sidecar {
		// A port-forward sidecar runs from the agent's image and so inherits its
		// HEALTHCHECK (the agent turns it off when it creates one; this covers a
		// sidecar created any other way). It has no listener and no socket, and
		// its netns is the TARGET's — probing 127.0.0.1 there would knock on the
		// operator's own service.
		return "port-forward sidecar; nothing to check", nil
	}
	cfg, err := config.Parse(agentArgs, io.Discard)
	if err != nil {
		return "", fmt.Errorf("resolve agent config: %w", err)
	}
	addr, err := probeAddr(cfg.ListenAddr)
	if err != nil {
		return "", err
	}

	listener, err := probeListener(ctx, addr, cfg.CACert != "")
	if err != nil {
		return "", err
	}
	if err := probeDocker(ctx, cfg.DockerHost); err != nil {
		return "", err
	}
	return listener + "; docker reachable", nil
}

// probeArgs is the agent command line the probe resolves its config from.
//
// Docker runs a HEALTHCHECK as a fresh process with the container's
// environment but NOT its command line, and the agent is configured by both:
// `swarmexec init` passes -port, the stack files -listen. So the probe reads
// the agent's argv from /proc/1/cmdline — the agent is the container's PID 1 —
// and parses it with the agent's own parser, so the port it dials is the port
// the agent bound. Flags given to the probe itself win; a PID 1 that is not
// the agent (no /proc, a foreign wrapper) leaves environment and defaults.
//
// sidecar is true when PID 1 is a port-forward sidecar.
func probeArgs(own []string, cmdline func() ([]byte, error)) (args []string, sidecar bool) {
	if len(own) > 0 {
		return own, false
	}
	raw, err := cmdline()
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	argv := splitCmdline(raw)
	// docker-init / tini (service `init: true`) runs the agent as
	// "<init> -- /agent args…".
	if len(argv) > 0 && filepath.Base(argv[0]) != "agent" {
		for i, a := range argv {
			if a == "--" {
				argv = argv[i+1:]
				break
			}
		}
	}
	if len(argv) == 0 || filepath.Base(argv[0]) != "agent" {
		return nil, false
	}
	args = argv[1:]
	if _, ok := forwardTarget(args); ok {
		return nil, true
	}
	if _, ok := forwardMuxTarget(args); ok {
		return nil, true
	}
	return args, false
}

// splitCmdline splits /proc/<pid>/cmdline (NUL-separated, NUL-terminated).
func splitCmdline(raw []byte) []string {
	raw = bytes.TrimRight(raw, "\x00")
	if len(raw) == 0 {
		return nil
	}
	parts := bytes.Split(raw, []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

func readPID1Cmdline() ([]byte, error) { return os.ReadFile("/proc/1/cmdline") }

// probeAddr turns the agent's listen address into the address to dial: a
// wildcard (":9443", "0.0.0.0:9443", "[::]:9443") is reached over loopback, a
// specific address as given.
func probeAddr(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen address %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// probeListener checks that the agent's TLS listener answers.
//
// InsecureSkipVerify is right here and only here: the probe runs inside the
// agent's own container and dials the agent's own loopback. There is no
// network in between to be on, and nothing it sends is secret.
//
// Without a client CA (the self-signed, shared-secret mode `swarmexec init`
// deploys) it calls Version. The probe holds no secret on purpose — it should
// not need the credential to see whether the process is alive — so on an agent
// with a secret the answer is the shared-secret layer's Unauthenticated. Either
// answer is a gRPC status produced by the serving agent: the listener accepted,
// TLS completed, HTTP/2 framed, the interceptor chain ran. Anything else
// (refused, reset, timeout) is not an answer.
//
// With a client CA the agent requires a client certificate, which the probe
// does not have, so no RPC can get through; there it checks that the TLS
// handshake completes, or that the agent refuses the missing certificate with
// a TLS alert — which only a live listener sends.
func probeListener(ctx context.Context, addr string, clientCertRequired bool) (string, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: true}
	if clientCertRequired {
		return probeTLS(ctx, addr, tlsCfg)
	}

	conn, err := grpc.NewClient("passthrough:///"+addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return "", fmt.Errorf("agent %s: %w", addr, err)
	}
	defer conn.Close()
	rctx, cancel := context.WithTimeout(ctx, healthRPCTimeout)
	defer cancel()
	resp, err := pb.NewAgentClient(conn).Version(rctx, &pb.VersionRequest{})
	switch status.Code(err) {
	case codes.OK:
		return fmt.Sprintf("agent %s answering on %s", resp.GetVersion(), addr), nil
	case codes.Unauthenticated, codes.PermissionDenied:
		return fmt.Sprintf("agent answering on %s (auth layer refused the credential-less probe, as it should)", addr), nil
	default:
		return "", fmt.Errorf("agent not answering on %s: %v", addr, err)
	}
}

// probeTLS completes a TLS handshake with the listener. A TLS alert from the
// peer counts as an answer: it is the agent refusing a client without a
// certificate (TLS 1.2 refuses inside the handshake, TLS 1.3 after it).
func probeTLS(ctx context.Context, addr string, tlsCfg *tls.Config) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, healthRPCTimeout)
	defer cancel()
	d := tls.Dialer{Config: tlsCfg}
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "remote error" {
			return fmt.Sprintf("agent TLS listener answering on %s (refused a client without certificate)", addr), nil
		}
		return "", fmt.Errorf("agent not answering on %s: %w", addr, err)
	}
	_ = conn.Close()
	return fmt.Sprintf("agent TLS listener answering on %s", addr), nil
}

// probeDocker pings the Docker daemon over the agent's own socket.
//
// Part of the probe because it is the one failure a restart fixes and nothing
// else reports: a daemon restarted under live-restore recreates
// /var/run/docker.sock, and the agent's bind mount keeps pointing at the old,
// dead one — the agent stays "running" and every RPC fails. The HEALTHCHECK
// retries make a brief daemon restart harmless: the daemon runs no probes
// while it is down, and it takes six consecutive failures, a minute or more,
// before Swarm replaces the task.
func probeDocker(ctx context.Context, host string) error {
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		return fmt.Errorf("docker client for %s: %w", host, err)
	}
	defer cli.Close()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		return fmt.Errorf("docker not reachable at %s: %w", host, err)
	}
	return nil
}
