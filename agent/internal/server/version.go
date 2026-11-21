// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"time"

	"swarmexec/agent/internal/version"
	"swarmexec/internal/pb"
)

// Version reports the agent's build and protocol version. It is a low-privilege
// probe (transport authentication still applies) used by `swarmexec doctor` and
// for client/agent skew detection.
//
// It also carries this node's wall clock, which is the only place in the
// protocol that reveals it. The time is read as late as possible — right here,
// not at dial or in an interceptor — because every millisecond between reading
// it and putting it on the wire is an error the client cannot distinguish from
// real drift.
func (s *Server) Version(_ context.Context, _ *pb.VersionRequest) (*pb.VersionResponse, error) {
	return &pb.VersionResponse{
		Version:      version.Version,
		ProtoVersion: version.Protocol,
		Now:          time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}
