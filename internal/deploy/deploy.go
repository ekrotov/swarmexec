// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package deploy is the vocabulary the client and the agent must agree on for
// `swarmexec init` to deploy an agent this agent binary can actually run.
//
// CONTRACT.md and the proto govern the bytes on the wire, and they govern them
// well. They did not govern how the agent comes to exist. The client writes the
// agent's command line, its secret mount path, its socket path and its label —
// and every one of those spellings existed twice, once where it is written and
// once where it is parsed, joined by nothing but habit. Renaming an agent flag
// passed `go build`, passed `go test`, passed review, and then broke
// `swarmexec init` against every cluster at deploy time. It was the only
// coupling in the system with no compile-time and no wire-time check.
//
// This package is that second contract, written down. Both halves import it:
// agent/internal/config registers the deployed flags from these names, and its
// round-trip test parses what AgentArgs produces. A rename is now a compile
// error on one side or a red test on the other, never a broken deployment.
//
// It holds ONLY what both sides touch. An agent flag the client never sets
// (-ca-cert, -max-streams …) stays where it is: moving it here would advertise
// a contract that does not exist. -metrics-addr moved in when init learned to
// set it.
package deploy

import (
	"fmt"
	"time"
)

// Identity and layout of a deployed agent.
const (
	// ServiceName is the swarm service `swarmexec init` creates.
	ServiceName = "swarmexec_agent"

	// SecretName is the Docker secret holding the shared secret, and SecretPath
	// is where the agent reads it inside the container. The two must agree: the
	// client mounts the secret under this name, and the agent is told to read
	// exactly this path.
	SecretName = "swarmexec_agent_secret"
	SecretPath = "/run/secrets/" + SecretName

	// SocketPath is the Docker socket the client bind-mounts into the agent, and
	// SocketURL is the same path as the agent's -docker-host value.
	SocketPath = "/var/run/docker.sock"
	SocketURL  = "unix://" + SocketPath

	// RoleLabel marks the objects swarmexec creates, so an operator (and
	// `swarmexec down`) can find them without guessing from an image name.
	// RoleAgent is on the agent service, RoleForward on a port-forward sidecar:
	// one key, two values, defined together so they cannot drift apart.
	RoleLabel   = "swarmexec.role"
	RoleAgent   = "agent"
	RoleForward = "port-forward"

	// DefaultPort is the agent's gRPC/TLS port (CONTRACT.md §2). The client
	// dials it, `init` publishes it, and the agent listens on it — three places
	// that used to hold three copies of 9443.
	DefaultPort = 9443
)

// The agent flags `swarmexec init` sets. These are the deployment contract: the
// agent registers them from these same constants, so a rename moves both sides
// at once. Flags the client never writes are deliberately absent.
const (
	FlagPort              = "port"
	FlagSelfSigned        = "self-signed"
	FlagAgentSecretFile   = "agent-secret-file"
	FlagDockerHost        = "docker-host"
	FlagDrainTimeout      = "drain-timeout"
	FlagLogFormat         = "log-format"
	FlagAuditDest         = "audit-dest"
	FlagAllowLegacySecret = "allow-legacy-secret"
	FlagMetricsAddr       = "metrics-addr"
)

// DrainTimeout is the graceful-shutdown window a deployed agent gets. Written
// out explicitly rather than left to the agent's own default, so the deployed
// spec says what it does.
const DrainTimeout = 5 * time.Second

// AgentOptions are the parts of the deployed command line that vary.
type AgentOptions struct {
	Port int
	// AllowLegacySecret keeps the pre-channel-binding auth path open. The flag
	// is only emitted when turning it OFF: leaving it out otherwise keeps the
	// deployed command identical to what earlier versions produced, so re-running
	// init does not show a spurious spec change.
	AllowLegacySecret bool
	// MetricsPort, when non-zero, turns on the agent's Prometheus endpoint on
	// that port. Zero writes nothing, so a deployment without metrics keeps the
	// command line earlier versions produced.
	MetricsPort int
}

// AgentArgs is the agent's command line as `swarmexec init` deploys it.
//
// One construction, so the argv that is written and the flags that are parsed
// cannot be edited apart. Its exact output is pinned by a test here, and parsed
// by a test in agent/internal/config.
func AgentArgs(o AgentOptions) []string {
	args := []string{
		fmt.Sprintf("-%s=%d", FlagPort, o.Port),
		"-" + FlagSelfSigned,
		"-" + FlagAgentSecretFile + "=" + SecretPath,
		"-" + FlagDockerHost + "=" + SocketURL,
		"-" + FlagDrainTimeout + "=" + DrainTimeout.String(),
		"-" + FlagLogFormat + "=json",
		"-" + FlagAuditDest + "=stdout",
	}
	if !o.AllowLegacySecret {
		args = append(args, "-"+FlagAllowLegacySecret+"=false")
	}
	if o.MetricsPort > 0 {
		args = append(args, fmt.Sprintf("-%s=:%d", FlagMetricsAddr, o.MetricsPort))
	}
	return args
}
