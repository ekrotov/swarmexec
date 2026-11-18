// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"time"

	"swarmexec/client/internal/fanout"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// Overlay endpoint state is per node: each daemon keeps its own networkdb copy
// of a network's endpoints, and a node that missed a departure keeps stale
// IP/MAC entries the manager no longer knows about. Only the node's agent can
// report that copy (CONTRACT.md §3.9).

// networkEndpointsTimeout bounds one node's NetworkEndpoints call: one inspect
// per swarm network against the local daemon.
const networkEndpointsTimeout = 15 * time.Second

// NodeNetworkEndpoints is one node's answer to NetworkEndpoints. Err is
// explained (agenterr) but keeps the agent's gRPC status: agenterr.TooOld(Err)
// means an agent that predates the RPC — "unknown", never "no endpoints". A
// node the fan-out never reached carries fanout.ErrSkipped.
type NodeNetworkEndpoints struct {
	Node resolve.Node
	Resp *pb.NetworkEndpointsResponse
	Err  error
}

// NodesNetworkEndpoints asks every node's agent for its view of the endpoints
// on networkIDs (empty: every swarm network the node has joined), in parallel.
// Results are in the order of nodes.
func (c *Client) NodesNetworkEndpoints(ctx context.Context, nodes []resolve.Node, networkIDs []string, connectTimeout time.Duration) []NodeNetworkEndpoints {
	out := make([]NodeNetworkEndpoints, len(nodes))
	for i, n := range nodes {
		out[i] = NodeNetworkEndpoints{Node: n, Err: fanout.ErrSkipped}
	}
	_ = PerNode(ctx, nodes, func(ctx context.Context, i int, n resolve.Node) {
		resp, err := c.NetworkEndpoints(ctx, n, networkIDs, connectTimeout)
		out[i] = NodeNetworkEndpoints{Node: n, Resp: resp, Err: err}
	})
	return out
}

// NetworkEndpoints asks one node's agent for its view of the endpoints on
// networkIDs (empty: every swarm network the node has joined). Gated
// (capgate.go): an agent that answered Unimplemented is not asked again for a
// while, and the remembered answer is returned instead.
func (c *Client) NetworkEndpoints(ctx context.Context, n resolve.Node, networkIDs []string, connectTimeout time.Duration) (*pb.NetworkEndpointsResponse, error) {
	return gated(c, ctx, n.DialHost, rpcNetworkEndpoints, func() (*pb.NetworkEndpointsResponse, error) {
		return c.networkEndpoints(ctx, n, networkIDs, connectTimeout)
	})
}

func (c *Client) networkEndpoints(ctx context.Context, n resolve.Node, networkIDs []string, connectTimeout time.Duration) (*pb.NetworkEndpointsResponse, error) {
	cctx, cancel := context.WithTimeout(ctx, max(connectTimeout, networkEndpointsTimeout))
	defer cancel()
	conn, err := c.Conn(cctx, n.DialHost)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := conn.Agent().NetworkEndpoints(cctx, &pb.NetworkEndpointsRequest{NetworkIds: networkIDs})
	conn.done(err)
	if err != nil {
		return nil, c.Explain(ctx, err)
	}
	return resp, nil
}
