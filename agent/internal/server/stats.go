// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// Stats reports resource usage of the containers on this node. Container stats
// are node-local — the manager API knows what the scheduler BOOKED, not what is
// actually being used — so the cli queries every node and aggregates.
//
// The reading itself comes from a background sampler (see containerStatsCache),
// not from this call: a CPU percentage is a delta between two readings, and
// several clients polling the same node should not each trigger their own scan.
// The first response after a quiet period therefore carries memory but no CPU,
// flagged by cpu_ready.
func (s *Server) Stats(ctx context.Context, req *pb.StatsRequest) (*pb.StatsResponse, error) {
	identity, err := s.identityFn(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "client identity unavailable: %v", err)
	}
	decision := s.authz.Authorize(ctx, auth.Request{Action: "stats.read", Identity: identity})
	s.audit.AuthDecision(identity, "", "", decision.Allow, decision.Reason)
	if !decision.Allow {
		s.metrics.AuthDenied()
		return nil, status.Errorf(codes.PermissionDenied, "authorization denied: %s", decision.Reason)
	}

	samples, sampledAt, cpuReady := s.statsCache.get()
	nodeCPUs, nodeMem := s.statsCache.capacity(ctx)

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
func statsProto(id string, s containerSample, nodeMem int64) *pb.ContainerStats {
	out := &pb.ContainerStats{
		ContainerId:   id,
		MemoryBytes:   s.memBytes,
		CpuLimitCores: s.cpuLimit,
		MemoryLimited: s.memIsOwn,
	}
	switch {
	case s.memIsOwn:
		out.MemoryLimitBytes = s.memLimit
	case nodeMem > 0:
		out.MemoryLimitBytes = nodeMem
	default:
		out.MemoryLimitBytes = s.memLimit // the frame's value, node total in practice
	}
	if s.cpuUsable {
		out.CpuPercent = s.cpuPct
	}
	return out
}
