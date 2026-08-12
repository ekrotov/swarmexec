// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/docker/cli/cli/connhelper/commandconn"
	"github.com/docker/cli/cli/connhelper/ssh"

	"swarmexec/client/internal/dockerctx"
)

// sshProxyDialer returns a dialer that tunnels TCP connections to the swarm
// nodes over the same ssh:// host as the Docker context. It returns nil (dial
// directly) when the context is not an ssh endpoint.
//
// Why: with an ssh:// Docker context the Docker API is tunnelled over ssh, but
// the agents' gRPC endpoints are not — swarmexec would dial node:9443 directly,
// which usually isn't routable from the operator's machine (the nodes live
// behind the bastion). Tunnelling agent traffic over the same ssh host makes
// exec/logs/doctor/ui work like the Docker API does.
func sshProxyDialer(contextOverride string) (func(context.Context, string) (net.Conn, error), error) {
	host, err := dockerctx.ResolveHost(contextOverride)
	if err != nil {
		// Best-effort: a command that actually needs Docker will fail later with
		// a clearer error from newDockerClient.
		return nil, nil //nolint:nilerr // intentional: don't fail config resolution here
	}
	if !strings.HasPrefix(host, "ssh://") {
		return nil, nil
	}
	sp, err := ssh.ParseURL(host)
	if err != nil {
		return nil, fmt.Errorf("parse ssh docker context %q: %w", host, err)
	}
	jump := dockerctx.ResolveProxyJump(contextOverride)
	return func(ctx context.Context, addr string) (net.Conn, error) {
		// commandconn runs ssh via exec (no shell), so args need no quoting.
		conn, cerr := commandconn.New(ctx, "ssh", sshForwardArgs(sp, addr, jump)...)
		if cerr != nil {
			return nil, fmt.Errorf("ssh tunnel to %s via %s: %w", addr, sp.Host, cerr)
		}
		return conn, nil
	}, nil
}

// sshForwardArgs builds `ssh [-l user] [-p port] [-J jump] -W <target> -- <host>`,
// which forwards this process's stdio to target through the ssh host (via the
// jump host(s) when set) — exactly the net.Conn commandconn wraps.
func sshForwardArgs(sp *ssh.Spec, target, proxyJump string) []string {
	var args []string
	if sp.User != "" {
		args = append(args, "-l", sp.User)
	}
	if sp.Port != "" {
		args = append(args, "-p", sp.Port)
	}
	if pj := strings.TrimSpace(proxyJump); pj != "" {
		args = append(args, "-J", pj)
	}
	args = append(args, "-W", target, "--", sp.Host)
	return args
}

// sshExtraFlags returns extra ssh CLI flags for the resolved context — currently
// the ProxyJump (-J). Shared by the Docker-API connhelper and the agent tunnel so
// both hop through the same bastion(s).
func sshExtraFlags(contextOverride string) []string {
	if pj := strings.TrimSpace(dockerctx.ResolveProxyJump(contextOverride)); pj != "" {
		return []string{"-J", pj}
	}
	return nil
}
