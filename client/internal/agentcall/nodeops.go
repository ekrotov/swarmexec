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

// Node-local housekeeping: agent version, image footprint, disk reclaim. Like
// volumes, none of it is visible to the manager.

// NodeVersion is one node's answer to the Version RPC.
type NodeVersion struct {
	Node    resolve.Node
	Version string
	Proto   string
	Err     error
}

// Versions queries every node's agent Version RPC in parallel. A node the
// fan-out never reached carries fanout.ErrSkipped.
func (c *Client) Versions(ctx context.Context, nodes []resolve.Node, connectTimeout time.Duration) []NodeVersion {
	out := make([]NodeVersion, len(nodes))
	for i, n := range nodes {
		out[i] = NodeVersion{Node: n, Err: fanout.ErrSkipped}
	}
	_ = fanout.Nodes(ctx, nodes, func(ctx context.Context, i int, n resolve.Node) {
		v, p, err := c.Version(ctx, n, connectTimeout)
		out[i] = NodeVersion{Node: n, Version: v, Proto: p, Err: err}
	})
	return out
}

// Version asks one node's agent for its build and protocol version. The error
// is returned as the agent sent it, unexplained: callers classify it.
func (c *Client) Version(ctx context.Context, n resolve.Node, connectTimeout time.Duration) (version, proto string, err error) {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := c.Conn(dctx, n.DialHost)
	cancel()
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	resp, err := conn.Agent().Version(ctx, &pb.VersionRequest{})
	if err != nil {
		return "", "", err
	}
	return resp.Version, resp.ProtoVersion, nil
}

// NodeImages is one node's image footprint, as the node view renders it.
type NodeImages struct {
	Total int64 // every image on the node

	// Reclaimable, split the way the operator has to choose:
	//   Dangling — untagged leftovers of a rebuild. Nothing can start from them.
	//   Unused   — tagged, but no RUNNING container uses them. On a swarm node
	//              that includes any service scaled to zero or between restarts,
	//              which would then have to pull again.
	Dangling int64
	Unused   int64

	Count int
}

// Reclaimable is what the safe sweep would free.
func (n NodeImages) Reclaimable() int64 { return n.Dangling }

// Per-node image call budgets. Listing is cheap; a prune can take a while on a
// node with a lot of layers, so it gets its own budget.
const (
	imageListTimeout  = 15 * time.Second
	imagePruneTimeout = 5 * time.Minute
)

// Images lists one node's image footprint.
func (c *Client) Images(ctx context.Context, n resolve.Node, connectTimeout time.Duration) (NodeImages, error) {
	cctx, cancel := context.WithTimeout(ctx, max(connectTimeout, imageListTimeout))
	defer cancel()
	conn, err := c.Conn(cctx, n.DialHost)
	if err != nil {
		return NodeImages{}, err
	}
	defer conn.Close()
	resp, err := conn.Agent().ListImages(cctx, &pb.ListImagesRequest{})
	if err != nil {
		return NodeImages{}, c.Explain(ctx, err)
	}
	return NodeImages{
		Total:    resp.GetTotalBytes(),
		Dangling: resp.GetDanglingBytes(),
		Unused:   resp.GetUnusedBytes(),
		Count:    len(resp.GetImages()),
	}, nil
}

// PruneImages reclaims image space on one node. all=true is the destructive
// mode; see the proto for what it includes.
func (c *Client) PruneImages(ctx context.Context, n resolve.Node, all bool, connectTimeout time.Duration) (reclaimed int64, deleted int, err error) {
	cctx, cancel := context.WithTimeout(ctx, max(connectTimeout, imagePruneTimeout))
	defer cancel()
	conn, err := c.Conn(cctx, n.DialHost)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	resp, err := conn.Agent().PruneImages(cctx, &pb.PruneImagesRequest{All: all})
	if err != nil {
		return 0, 0, c.Explain(ctx, err)
	}
	return resp.GetReclaimedBytes(), len(resp.GetDeleted()), nil
}

// Disk-reclaim budgets: the figures are cached on the agent, measure_now runs
// a full measurement, and a prune may take minutes.
const (
	reclaimTimeout      = 15 * time.Second
	reclaimMeasureLimit = 3 * time.Minute
	nodePruneTimeout    = 10 * time.Minute
)

// ReclaimStats asks one node what a prune would free there.
func (c *Client) ReclaimStats(ctx context.Context, n resolve.Node, measureNow bool, connectTimeout time.Duration) (*pb.ReclaimStatsResponse, error) {
	timeout := max(connectTimeout, reclaimTimeout)
	if measureNow {
		timeout = max(timeout, reclaimMeasureLimit)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := c.Conn(cctx, n.DialHost)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := conn.Agent().ReclaimStats(cctx, &pb.ReclaimStatsRequest{MeasureNow: measureNow})
	if err != nil {
		return nil, c.Explain(ctx, err)
	}
	return resp, nil
}

// Prune runs a prune on one node.
func (c *Client) Prune(ctx context.Context, n resolve.Node, req *pb.PruneRequest, connectTimeout time.Duration) (*pb.PruneResponse, error) {
	cctx, cancel := context.WithTimeout(ctx, max(connectTimeout, nodePruneTimeout))
	defer cancel()
	conn, err := c.Conn(cctx, n.DialHost)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := conn.Agent().Prune(cctx, req)
	if err != nil {
		return nil, c.Explain(ctx, err)
	}
	return resp, nil
}
