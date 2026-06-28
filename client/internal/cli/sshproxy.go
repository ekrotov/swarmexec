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
	return func(ctx context.Context, addr string) (net.Conn, error) {
		// commandconn runs ssh via exec (no shell), so args need no quoting.
		conn, cerr := commandconn.New(ctx, "ssh", sshForwardArgs(sp, addr)...)
		if cerr != nil {
			return nil, fmt.Errorf("ssh tunnel to %s via %s: %w", addr, sp.Host, cerr)
		}
		return conn, nil
	}, nil
}

// sshForwardArgs builds `ssh [-l user] [-p port] -W <target> -- <host>`, which
// forwards this process's stdio to target through the ssh host — exactly the
// net.Conn commandconn wraps.
func sshForwardArgs(sp *ssh.Spec, target string) []string {
	var args []string
	if sp.User != "" {
		args = append(args, "-l", sp.User)
	}
	if sp.Port != "" {
		args = append(args, "-p", sp.Port)
	}
	args = append(args, "-W", target, "--", sp.Host)
	return args
}
