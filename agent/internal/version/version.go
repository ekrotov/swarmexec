// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package version holds build and protocol version information.
package version

import "swarmexec/internal/pb"

// Version is the agent build version, overridable at build time via
// -ldflags "-X swarmexec/agent/internal/version.Version=...".
var Version = "dev"

// Protocol is the wire-protocol version (CONTRACT.md §7). A constant, not a
// build flag: it comes from the generated package the client compiles too,
// so the two binaries cannot disagree about it.
const Protocol = pb.ProtocolVersion
