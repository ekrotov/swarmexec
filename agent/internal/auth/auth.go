// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package auth defines the authorization hook every RPC passes before it acts
// (Version alone is exempt, CONTRACT.md §7) and its two implementations:
// AllowAll, the default, and Policy, which evaluates the rule file of
// internal/policy (the -policy-file flag). The interface is intentionally
// narrow: Request.Action names what is being asked, and a policy never touches
// the exec bridge or any other handler.
package auth

import "context"

// Request is everything the authorizer needs to decide on a single action.
type Request struct {
	// Action is the operation being authorized: "exec", "logs", "portforward",
	// "volume.list", "volume.remove", "stats.read", "image.list",
	// "image.prune" or "image.prune.all" — the last two are distinct so a policy
	// can allow reclaiming untagged layers without allowing the sweep that also
	// removes images a stopped service still needs. The Prune RPC authorizes
	// each category on its own: "container.prune", "image.prune",
	// "image.prune.all", "buildcache.prune", "network.prune", "volume.prune";
	// its stats are "reclaim.read". NetworkEndpoints is "network.inspect".
	// KillContainer is "kill", InspectContainer "inspect". Copying is two
	// actions, not one: "copy.from" reads a path out of a container,
	// "copy.to" writes one into it. They are separated for the same reason
	// "image.prune" and "image.prune.all" are — a policy must be able to allow
	// pulling a log out without also allowing a write into every container's
	// filesystem, which is the sharpest permission this agent has.
	Action string
	// Port is the target TCP port inside the container for "portforward".
	// Forwarding is a distinct exposure class from exec — it lifts an internal
	// service port onto the operator's workstation, where every other process
	// on that machine can reach it — so a policy can deny it independently.
	Port uint32
	// Volume is the target volume name for volume.* actions.
	Volume string
	// Path is the path inside the container for the copy.* actions: the source
	// for copy.from, the destination directory for copy.to. It is carried for
	// the audit record, which has to name what was copied — no rule matches on
	// it. A path matcher in the policy language would need its own glob
	// semantics and its own fuzzing, and belongs in its own change.
	Path string
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
	// EffectiveUser is who the exec will really run as: User, else the
	// container's configured user, else root. Set by the agent, not the client.
	EffectiveUser string
	// ClientAddr is the peer's network address. It is for the audit record;
	// no policy condition matches on it.
	ClientAddr string
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

// Authorizer decides whether a given client may perform a given action (an
// exec, a log stream, a prune, …). It is called after transport authentication
// — the mTLS client certificate or the shared secret — has succeeded. The
// Identity is verified only when a client certificate was required: with a
// shared secret and no client CA it is whatever the client claims, which is why
// the policy refuses rules on identities there.
type Authorizer interface {
	Authorize(ctx context.Context, req Request) Decision
}

// AllowAll is the default policy, used when no rule file is given: anyone who
// passed transport authentication is allowed.
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
