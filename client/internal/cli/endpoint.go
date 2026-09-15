// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"net"

	"github.com/docker/docker/client"

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
