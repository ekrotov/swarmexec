// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentcall

import (
	"context"
	"sort"
	"time"

	"swarmexec/client/internal/fanout"
	"swarmexec/client/internal/resolve"
	"swarmexec/internal/pb"
)

// Volumes are node-local: the manager cannot list them, so every node's agent
// is asked and the answers are aggregated by name.

// Volume is a named volume aggregated across the nodes that hold it.
type Volume struct {
	Name    string
	Driver  string
	Nodes   []resolve.Node    // nodes that hold a copy of this volume
	Labels  map[string]string // volume labels (same across nodes for a named volume)
	Created time.Time         // earliest creation time across nodes; zero if unknown
}

// VolumeConsumer is a container (and its service) that mounts a volume.
type VolumeConsumer struct {
	Service   string
	Container string
	Node      string
}

// NodeResult is the outcome of one per-node operation.
type NodeResult struct {
	Node resolve.Node
	Err  error
}

// Volumes queries every node's agent in parallel and aggregates volumes by
// name. Unreachable nodes are returned in the errors map (keyed by node name).
func (c *Client) Volumes(ctx context.Context, nodes []resolve.Node, connectTimeout time.Duration) ([]Volume, map[string]error) {
	type res struct {
		node resolve.Node
		vols []*pb.VolumeInfo
		err  error
	}
	out := make([]res, len(nodes))
	for i, n := range nodes {
		out[i] = res{node: n, err: fanout.ErrSkipped}
	}
	_ = fanout.Nodes(ctx, nodes, func(ctx context.Context, i int, n resolve.Node) {
		vs, err := c.listNodeVolumes(ctx, n, false, connectTimeout)
		out[i] = res{node: n, vols: vs, err: err}
	})

	byName := map[string]*Volume{}
	errs := map[string]error{}
	for _, r := range out {
		if r.err != nil {
			errs[r.node.Name] = r.err
			continue
		}
		for _, v := range r.vols {
			sv := byName[v.Name]
			if sv == nil {
				sv = &Volume{Name: v.Name, Driver: v.Driver, Labels: v.GetLabels()}
				byName[v.Name] = sv
			}
			sv.Nodes = append(sv.Nodes, r.node)
			// Keep the earliest creation time across the nodes holding the volume.
			if t, err := time.Parse(time.RFC3339, v.GetCreatedAt()); err == nil {
				if sv.Created.IsZero() || t.Before(sv.Created) {
					sv.Created = t
				}
			}
		}
	}
	vols := make([]Volume, 0, len(byName))
	for _, sv := range byName {
		sort.Slice(sv.Nodes, func(i, j int) bool { return sv.Nodes[i].Name < sv.Nodes[j].Name })
		vols = append(vols, *sv)
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	return vols, errs
}

// volumeSizeTimeout bounds a with_size ListVolumes call: the agent runs a
// du-style disk-usage scan that can take a while, far longer than a connect.
const volumeSizeTimeout = 2 * time.Minute

func (c *Client) listNodeVolumes(ctx context.Context, n resolve.Node, withSize bool, connectTimeout time.Duration) ([]*pb.VolumeInfo, error) {
	// One context for dial AND the RPC, cancelled only when we're done. Cancelling
	// the dial context early (the old pattern) tears down the connection while a
	// slow with_size scan is still running — over the ssh tunnel that surfaces as
	// "context canceled" on the agent. With size we also need a generous deadline.
	timeout := connectTimeout
	if withSize && volumeSizeTimeout > timeout {
		timeout = volumeSizeTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := c.Conn(cctx, n.DialHost)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := conn.Agent().ListVolumes(cctx, &pb.ListVolumesRequest{WithSize: withSize})
	if err != nil {
		return nil, c.Explain(ctx, err)
	}
	return resp.Volumes, nil
}

// VolumeSizes returns volume name -> total on-disk bytes, summed across the
// nodes that hold it. Computing sizes is expensive (du-style), so this is a
// separate opt-in pass. Volumes with no known size on any node are absent from
// the map (callers render them as "-"). Unreachable nodes are skipped.
func (c *Client) VolumeSizes(ctx context.Context, nodes []resolve.Node, connectTimeout time.Duration) map[string]int64 {
	per := make([][]*pb.VolumeInfo, len(nodes))
	_ = fanout.Nodes(ctx, nodes, func(ctx context.Context, i int, n resolve.Node) { // best effort: unreached nodes are skipped
		vs, err := c.listNodeVolumes(ctx, n, true, connectTimeout)
		if err != nil {
			return
		}
		per[i] = vs
	})
	sizes := map[string]int64{}
	for _, vs := range per {
		for _, v := range vs {
			// Skip when the agent didn't report a size (older agent that predates
			// size reporting) or it's not available (non-local driver).
			if !v.GetSizeKnown() || v.GetSizeBytes() < 0 {
				continue
			}
			sizes[v.GetName()] += v.GetSizeBytes()
		}
	}
	return sizes
}

// VolumeUsage maps each volume name to the containers that mount it, by
// asking every node's agent for its containers and their named volumes. Nodes
// whose agent is unreachable are skipped (best-effort: usage may be partial).
func (c *Client) VolumeUsage(ctx context.Context, nodes []resolve.Node, connectTimeout time.Duration) map[string][]VolumeConsumer {
	type res struct {
		node       resolve.Node
		containers []*pb.ContainerInfo
	}
	out := make([]res, len(nodes))
	_ = fanout.Nodes(ctx, nodes, func(ctx context.Context, i int, n resolve.Node) { // best effort, like unreachable nodes
		cs, err := c.listNodeContainers(ctx, n, connectTimeout)
		if err != nil {
			return // leave out[i].containers nil
		}
		out[i] = res{node: n, containers: cs}
	})

	usage := map[string][]VolumeConsumer{}
	for _, r := range out {
		for _, ci := range r.containers {
			for _, vol := range ci.GetVolumes() {
				usage[vol] = append(usage[vol], VolumeConsumer{
					Service:   ci.GetService(),
					Container: ci.GetName(),
					Node:      r.node.Name,
				})
			}
		}
	}
	for _, cs := range usage {
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].Service != cs[j].Service {
				return cs[i].Service < cs[j].Service
			}
			return cs[i].Container < cs[j].Container
		})
	}
	return usage
}

func (c *Client) listNodeContainers(ctx context.Context, n resolve.Node, connectTimeout time.Duration) ([]*pb.ContainerInfo, error) {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := c.Conn(dctx, n.DialHost)
	cancel()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := conn.Agent().ListContainers(ctx, &pb.ListRequest{})
	if err != nil {
		return nil, c.Explain(ctx, err)
	}
	return resp.Containers, nil
}

// skippedResults pre-fills a per-node result slice with fanout.ErrSkipped, so
// a node the fan-out never reached (its context ended first) reports a failure
// rather than reading as done.
func skippedResults(nodes []resolve.Node) []NodeResult {
	out := make([]NodeResult, len(nodes))
	for i, n := range nodes {
		out[i] = NodeResult{Node: n, Err: fanout.ErrSkipped}
	}
	return out
}

// RemoveVolume removes the volume on each node concurrently.
func (c *Client) RemoveVolume(ctx context.Context, nodes []resolve.Node, name string, force bool, connectTimeout time.Duration) []NodeResult {
	out := skippedResults(nodes)
	_ = fanout.Nodes(ctx, nodes, func(ctx context.Context, i int, n resolve.Node) {
		out[i] = NodeResult{Node: n, Err: c.removeNodeVolume(ctx, n, name, force, connectTimeout)}
	})
	return out
}

func (c *Client) removeNodeVolume(ctx context.Context, n resolve.Node, name string, force bool, connectTimeout time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := c.Conn(dctx, n.DialHost)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Agent().RemoveVolume(ctx, &pb.RemoveVolumeRequest{Name: name, Force: force})
	return c.Explain(ctx, err)
}

// CreateVolume creates the volume on each target node concurrently (volumes
// are node-local).
func (c *Client) CreateVolume(ctx context.Context, nodes []resolve.Node, req *pb.CreateVolumeRequest, connectTimeout time.Duration) []NodeResult {
	out := skippedResults(nodes)
	_ = fanout.Nodes(ctx, nodes, func(ctx context.Context, i int, n resolve.Node) {
		out[i] = NodeResult{Node: n, Err: c.createNodeVolume(ctx, n, req, connectTimeout)}
	})
	return out
}

func (c *Client) createNodeVolume(ctx context.Context, n resolve.Node, req *pb.CreateVolumeRequest, connectTimeout time.Duration) error {
	dctx, cancel := context.WithTimeout(ctx, connectTimeout)
	conn, err := c.Conn(dctx, n.DialHost)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Agent().CreateVolume(ctx, req)
	return c.Explain(ctx, err)
}
