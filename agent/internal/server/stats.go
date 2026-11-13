// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"time"

	"swarmexec/agent/internal/auth"
	"swarmexec/agent/internal/statsampler"
	"swarmexec/internal/pb"
)

// Stats reports resource usage of the containers on this node. Container stats
// are node-local — the manager API knows what the scheduler BOOKED, not what is
// actually being used — so the cli queries every node and aggregates.
//
// The reading itself comes from a background sampler (see internal/statsampler),
// not from this call: a CPU percentage is a delta between two readings, and
// several clients polling the same node should not each trigger their own scan.
// The first response after a quiet period therefore carries memory but no CPU,
// flagged by cpu_ready.
func (s *Server) Stats(ctx context.Context, req *pb.StatsRequest) (*pb.StatsResponse, error) {
	if _, err := s.authorizeUnary(ctx, auth.Request{Action: "stats.read"}); err != nil {
		return nil, err
	}

	samples, sampledAt, cpuReady := s.statsCache.Get()
	nodeCPUs, nodeMem := s.statsCache.Capacity(ctx)

	// An empty id list means "everything you have"; a non-empty one is the cli
	// telling us which containers it is actually displaying.
	want := map[string]bool{}
	for _, id := range req.GetContainerIds() {
		want[id] = true
	}

	out := &pb.StatsResponse{
		CpuReady:             cpuReady,
		NodeCpus:             nodeCPUs,
		NodeMemoryTotalBytes: nodeMem,
	}
	if !sampledAt.IsZero() {
		out.SampledAt = sampledAt.UTC().Format(time.RFC3339)
	}
	for id, smp := range samples {
		if len(want) > 0 && !want[id] {
			continue
		}
		out.Stats = append(out.Stats, statsProto(id, smp, nodeMem))
	}
	return out, nil
}

// statsProto renders one reading for the wire. nodeMem resolves the memory
// limit: docker reports the node's total for a container with no limit of its
// own, and the client must not read that as "this container may use 64 GB".
func statsProto(id string, s statsampler.Sample, nodeMem int64) *pb.ContainerStats {
	out := &pb.ContainerStats{
		ContainerId:   id,
		MemoryBytes:   s.MemBytes,
		CpuLimitCores: s.CPULimit,
		MemoryLimited: s.MemIsOwn,
		Health:        s.Health,
	}
	switch {
	case s.MemIsOwn:
		out.MemoryLimitBytes = s.MemLimit
	case nodeMem > 0:
		out.MemoryLimitBytes = nodeMem
	default:
		out.MemoryLimitBytes = s.MemLimit // the frame's value, node total in practice
	}
	if s.CPUUsable {
		out.CpuPercent = s.CPUPercent
	}
	return out
}
