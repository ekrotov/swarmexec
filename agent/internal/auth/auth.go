// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package auth defines the authorization hook invoked before every exec and a
// default policy. The interface is intentionally narrow so a richer policy
// (per-user/per-service allowlists, deny root, command allowlists) can be
// dropped in without touching the exec bridge.
package auth

import "context"

// Request is everything the authorizer needs to decide on a single action.
type Request struct {
	// Action is the operation being authorized: "exec", "logs", "portforward",
	// "volume.list", "volume.remove", "stats.read", "image.list",
	// "image.prune" or "image.prune.all" — the last two are distinct so a policy
	// can allow reclaiming untagged layers without allowing the sweep that also
	// removes images a stopped service still needs.
	Action string
	// Port is the target TCP port inside the container for "portforward".
	// Forwarding is a distinct exposure class from exec — it lifts an internal
	// service port onto the operator's workstation, where every other process
	// on that machine can reach it — so a policy can deny it independently.
	Port uint32
	// Volume is the target volume name for volume.* actions.
	Volume string
	// Identity is the client certificate Common Name (the operator identity).
	Identity string
	// ContainerID is the full target container ID.
	ContainerID string
	// Service is the resolved swarm service name, or empty if not a swarm task.
	Service string
	// Cmd is the requested command and arguments.
	Cmd []string
	// User is the requested user ("1000:1000", "root", or empty).
	User string
	// TTY indicates whether a TTY was requested.
	TTY bool
}

// Decision is the outcome of an authorization check.
type Decision struct {
	// Allow reports whether the exec may proceed.
	Allow bool
	// Reason is a short, audit-friendly explanation (always set).
	Reason string
}

// Authorizer decides whether a given client may start a given exec. It is
// called after mTLS has already verified the client certificate chain, so the
// Identity in the request is authenticated.
type Authorizer interface {
	Authorize(ctx context.Context, req Request) Decision
}

// AllowAll is the v1 default policy: any client whose certificate was verified
// against the trusted CA (i.e. anyone who reaches this far) is allowed. mTLS
// verification has already happened at the transport layer.
type AllowAll struct{}

// Authorize implements Authorizer.
func (AllowAll) Authorize(_ context.Context, _ Request) Decision {
	// Authentication (mTLS client cert or shared secret) is enforced at the
	// transport layer before this runs, so the default policy allows everyone
	// who got this far, regardless of which auth mode is in effect.
	return Decision{Allow: true, Reason: "default allow-all policy (transport authentication enforced)"}
}

// Ensure AllowAll satisfies the interface at compile time.
var _ Authorizer = AllowAll{}
