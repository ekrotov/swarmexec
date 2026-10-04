// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"net"

	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"

	"swarmexec/client/internal/dockerctx"
)

// swarmexec reaches a cluster over TWO channels, and both of them have to lead
// to the same place:
//
//   - the manager API, through the Docker client;
//   - the node agents' gRPC, which the manager knows nothing about, over its
//     own ssh tunnel to the same bastion.
//
// They used to be built from two separate lookups — newDockerClient resolved
// one context name, sshProxyDialer resolved another — and the two could
// disagree. In the ui they did: switching cluster re-pointed the Docker client
// and left the agent tunnel aimed at the previous cluster's bastion, so the tree
// kept working (that comes from the manager) while exec, logs, port-forward,
// stats and volumes all went dark. Measured: 3/3 agents when started on a
// context directly, 0/3 after switching to the same context in the ui.
//
// dockerEndpoint is the fix in one sentence: resolve ONCE, build both from that.
// Divergence is then not unlikely, it is unconstructible — there is no second
// lookup left to disagree with the first.
type dockerEndpoint struct {
	// Context is the context name this was resolved from, "" when the endpoint
	// came from DOCKER_HOST or the current-context default. Carried so callers
	// that want to NAME the cluster (a report header, a filename) use the same
	// answer everything else used.
	Context string

	// Host is the resolved endpoint: "ssh://…", "unix://…", "tcp://…".
	Host string

	// ProxyJump is the context's ssh hops, so the API and the agent tunnel take
	// the same route.
	ProxyJump string

	// err is a resolution failure, carried rather than returned. It surfaces
	// from client(), which is where it surfaced before this type existed — a
	// bad context has always been reported as a connection failure, not as a
	// config error, and moving it would change what every command prints. The
	// agent dialer keeps today's behaviour too: an unresolvable endpoint means
	// no tunnel, and whatever needs Docker fails next with the clearer message.
	err error
}

// resolveEndpoint works out where a run is pointed. contextName is the
// explicit choice ("" means "whatever docker would use").
func resolveEndpoint(contextName string) dockerEndpoint {
	host, err := dockerctx.ResolveHost(contextName)
	if err != nil {
		return dockerEndpoint{Context: contextName, err: err}
	}
	return dockerEndpoint{
		Context:   contextName,
		Host:      host,
		ProxyJump: dockerctx.ResolveProxyJump(contextName),
	}
}

// minDockerAPI is the oldest Docker Engine API swarmexec supports, and 1.40 is
// Docker Engine 19.03 (2019).
//
// Three reasons for this number rather than a lower one. It is where the docker
// client library is heading — the moby/moby line we will move to refuses
// anything below it outright — so stating it now makes that migration a
// non-event instead of a surprise. Nothing older is tested against. And the
// alternative is what we had: no floor at all, a library fallback to API 1.24
// (Docker 1.12, 2016), and a tool that connects and then fails one view at a
// time, which is the failure mode this project keeps having to undo.
//
// Two features need MORE than this from the node's own daemon — resource usage
// and the per-node image view, which want 1.41 and 1.42. Those degrade rather
// than fail: they stay empty and say so. The floor here is about whether
// swarmexec can do its job at all.
const minDockerAPI = "1.40"

// minDockerEngine is the release that speaks minDockerAPI, for an error message
// an operator can act on. Nobody knows their API version by heart.
const minDockerEngine = "19.03"

// connect builds the manager client and refuses a daemon too old to serve it.
//
// The check lives here because this is the one door: every command reaches the
// manager through it, so none of them can forget. It costs one Ping, which the
// client's version negotiation performs on first contact anyway.
func (e dockerEndpoint) connect(ctx context.Context) (*client.Client, error) {
	c, err := e.client()
	if err != nil {
		return nil, err
	}
	if err := checkAPIVersion(ctx, c); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// checkAPIVersion refuses a daemon below minDockerAPI.
//
// A Ping that fails for any OTHER reason is deliberately not treated as a
// version problem: the daemon may simply be unreachable, and reporting that as
// "too old" would send the operator looking in the wrong place. The real error
// surfaces from whatever call comes next.
func checkAPIVersion(ctx context.Context, c *client.Client) error {
	ping, err := c.Ping(ctx, client.PingOptions{})
	if err != nil || ping.APIVersion == "" {
		return nil
	}
	if versions.LessThan(ping.APIVersion, minDockerAPI) {
		return fmt.Errorf(
			"this Docker daemon speaks API %s; swarmexec needs at least API %s (Docker Engine %s or newer)",
			ping.APIVersion, minDockerAPI, minDockerEngine)
	}
	return nil
}

// client builds the manager API client for this endpoint.
func (e dockerEndpoint) client() (*client.Client, error) {
	if e.err != nil {
		return nil, e.err
	}
	return dockerClientForHost(e.Host, e.ProxyJump)
}

// agentDialer is how agent gRPC connections reach the nodes, or nil to dial
// them directly.
//
// With an ssh:// endpoint the Docker API is tunnelled over ssh but the agents'
// endpoints are not, and the nodes usually live behind the bastion — so agent
// traffic goes over the same ssh host, through the same jump hosts. Built from
// the same resolution the Docker client above was built from, which is the
// whole point of this type.
func (e dockerEndpoint) agentDialer() (func(context.Context, string) (net.Conn, error), error) {
	if e.err != nil {
		return nil, nil
	}
	return sshDialerForHost(e.Host, e.ProxyJump)
}
