package server

import (
	"context"

	"swarmexec/agent/internal/version"
	"swarmexec/internal/pb"
)

// Version reports the agent's build and protocol version. It is a low-privilege
// probe (transport authentication still applies) used by `swarmexec doctor` and
// for client/agent skew detection.
func (s *Server) Version(_ context.Context, _ *pb.VersionRequest) (*pb.VersionResponse, error) {
	return &pb.VersionResponse{
		Version:      version.Version,
		ProtoVersion: version.Protocol,
	}, nil
}
