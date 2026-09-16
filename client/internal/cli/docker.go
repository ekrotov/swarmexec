// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/docker/cli/cli/connhelper"
	"github.com/docker/docker/client"
)

// newDockerClient builds a Docker SDK client for the manager API, honoring
// Docker CLI contexts (--context / $DOCKER_CONTEXT / the config's current
// context) including ssh:// endpoints — which the bare SDK's client.FromEnv
// does not support (REQUIREMENTS §2).
// It also refuses a daemon older than swarmexec supports; see minDockerAPI.
func newDockerClient(ctx context.Context, contextOverride string) (*client.Client, error) {
	return resolveEndpoint(contextOverride).connect(ctx)
}

// dockerClientForHost builds the client for an ALREADY RESOLVED endpoint. The
// split matters: it is what lets the manager client and the agent tunnel come
// out of one resolution instead of two that can disagree (see dockerEndpoint).
func dockerClientForHost(host, proxyJump string) (*client.Client, error) {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if strings.HasPrefix(host, "ssh://") {
		// ssh endpoints need a connection helper (it tunnels the Docker API over
		// ssh, the same way `docker --context <ssh-ctx>` does). Inject the
		// context's ProxyJump (-J) so the API hop goes through the same bastion(s)
		// as the agent tunnel — no ~/.ssh/config needed — plus the connection
		// sharing that lets the API and the agent tunnel ride ONE transport to
		// that bastion instead of opening one each.
		helper, err := connhelper.GetConnectionHelperWithSSHOpts(host, sshEndpointOpts(host, proxyJump))
		if err != nil {
			return nil, fmt.Errorf("set up ssh connection to %s: %w", host, err)
		}
		opts = append(opts,
			client.WithHTTPClient(&http.Client{Transport: &http.Transport{DialContext: helper.Dialer}}),
			client.WithHost(helper.Host),
			client.WithDialContext(helper.Dialer),
		)
	} else {
		opts = append(opts, client.WithHost(host))
	}

	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to Docker manager API (%s): %w", host, err)
	}
	return c, nil
}

// pingDockerHost builds a throwaway client for host (ssh endpoints get -J
// proxyJump) and verifies it with an Info call — used to test a context before
// saving it.
func pingDockerHost(ctx context.Context, host, proxyJump string) error {
	opts := []client.Opt{client.WithAPIVersionNegotiation()}
	if strings.HasPrefix(host, "ssh://") {
		// Same flags as the real client, not a second hand-rolled copy: a probe
		// that connects differently from the thing it is probing for is a probe
		// that can pass for a setup which then fails.
		helper, err := connhelper.GetConnectionHelperWithSSHOpts(host, sshEndpointOpts(host, proxyJump))
		if err != nil {
			return fmt.Errorf("set up ssh connection: %w", err)
		}
		opts = append(opts,
			client.WithHTTPClient(&http.Client{Transport: &http.Transport{DialContext: helper.Dialer}}),
			client.WithHost(helper.Host),
			client.WithDialContext(helper.Dialer),
		)
	} else {
		opts = append(opts, client.WithHost(host))
	}
	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return err
	}
	defer c.Close()
	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	_, err = c.Info(cctx)
	return err
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func uptime(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
