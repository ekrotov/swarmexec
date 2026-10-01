// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package pb

// ProtocolVersion names the wire protocol both binaries speak. It is a
// property of this generated code, not of a build: the client and the agent
// compile it from the same package, so they cannot report different values,
// and no build flag can make them. doctor compares it across the fleet
// (CONTRACT.md §7).
//
// Hand-written next to the generated files on purpose; buf writes only
// swarmexec.pb.go and swarmexec_grpc.pb.go.
const ProtocolVersion = "swarmexec/v1"
