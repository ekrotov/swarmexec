// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package version holds build and protocol version information.
package version

// Version is the agent build version, overridable at build time via
// -ldflags "-X swarmexec/agent/internal/version.Version=...".
var Version = "dev"

// Protocol is the wire-protocol version (CONTRACT.md §7), overridable at build
// time via -ldflags "-X swarmexec/agent/internal/version.Protocol=...". It is
// kept in sync with the cli's protoVersion so both binaries report the same
// value (Makefile PROTO_VER).
var Protocol = "swarmexec/v1"
