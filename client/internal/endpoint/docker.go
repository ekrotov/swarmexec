// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package endpoint

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/docker/cli/cli/connhelper"
	"github.com/moby/moby/client"
)

// NewClient builds a Docker SDK client for the manager API, honoring
// Docker CLI contexts (--context / $DOCKER_CONTEXT / the config's current
// context) including ssh:// endpoints — which the bare SDK's client.FromEnv
// does not support (REQUIREMENTS §2).
// It also refuses a daemon older than swarmexec supports; see minAPI.
func NewClient(ctx context.Context, contextOverride string) (*client.Client, error) {
	return Resolve(contextOverride).Connect(ctx)
}

// clientForHost builds the client for an ALREADY RESOLVED endpoint. The
// split matters: it is what lets the manager client and the agent tunnel come
// out of one resolution instead of two that can disagree (see Docker).
func clientForHost(host, proxyJump string) (*client.Client, error) {
	var opts []client.Opt // API-version negotiation is the default now
	if strings.HasPrefix(host, "ssh://") {
		Prepare()
		// ssh endpoints need a connection helper (it tunnels the Docker API over
		// ssh, the same way `docker --context <ssh-ctx>` does). Inject the
		// context's ProxyJump (-J) so the API hop goes through the same bastion(s)
		// as the agent tunnel — no ~/.ssh/config needed — plus the connection
		// sharing that lets the API and the agent tunnel ride ONE transport to
		// that bastion instead of opening one each.
		helper, err := connhelper.GetConnectionHelperWithSSHOpts(host, SSHOpts(host, proxyJump))
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

	c, err := client.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to Docker manager API (%s): %w", host, err)
	}
	return c, nil
}

// PingHost builds a throwaway client for host (ssh endpoints get -J
// proxyJump) and verifies it with an Info call — used to test a context before
// saving it.
func PingHost(ctx context.Context, host, proxyJump string) error {
	var opts []client.Opt // API-version negotiation is the default now
	if strings.HasPrefix(host, "ssh://") {
		Prepare()
		// Same flags as the real client, not a second hand-rolled copy: a probe
		// that connects differently from the thing it is probing for is a probe
		// that can pass for a setup which then fails.
		helper, err := connhelper.GetConnectionHelperWithSSHOpts(host, SSHOpts(host, proxyJump))
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
	c, err := client.New(opts...)
	if err != nil {
		return err
	}
	defer c.Close()
	cctx, cancel := PromptAwareTimeout(ctx, 12*time.Second)
	defer cancel()
	_, err = c.Info(cctx, client.InfoOptions{})
	return err
}
