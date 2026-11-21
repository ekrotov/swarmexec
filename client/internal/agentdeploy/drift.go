// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agentdeploy

import (
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"swarmexec/internal/deploy"
)

// Drift compares the agent service running in a cluster against the shape
// Spec writes, and returns what differs.
//
// Why this exists: on 2026-11-20 `doctor` reported "all agents healthy" on
// four clusters whose agent service was missing the /sys mount that v1.30.0
// added. Every check it ran — reachable, version, protocol — passed, because
// an agent from the right build answering on the right port is exactly what a
// stale spec looks like from the outside. The spec is the one thing `init`
// writes and nothing afterwards verifies.
//
// What it deliberately does NOT compare is everything the operator chose:
// image, ports, policy, service name, replicas, the prune-schedule labels.
// Those are decisions, and a diagnosis that calls a decision a defect teaches
// people to ignore it. What is left is structure — the mounts, the secret,
// the mode, the restart and update policy — which `init` has no reason to
// write differently on two clusters, so a difference there is drift.
func Drift(svc swarm.Service) []Finding {
	cs := svc.Spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return []Finding{{Field: "container", Want: "a container spec", Got: "none"}}
	}
	var out []Finding
	out = append(out, driftMounts(cs.Mounts)...)
	out = append(out, driftSecret(cs.Secrets)...)
	out = append(out, driftMode(svc.Spec)...)
	return out
}

// Finding is one difference between the deployed spec and the written one.
type Finding struct {
	Field string `json:"field"`
	Want  string `json:"want"`
	Got   string `json:"got"`
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: expected %s, found %s", f.Field, f.Want, f.Got)
}

// DriftHint is what an operator does about a Finding. It names the cost
// because `init --force` rebuilds the spec from flags and drops anything
// edited into the service by hand.
const DriftHint = "run `swarmexec init --force` to rewrite the agent service " +
	"(it rebuilds the spec from flags, so hand edits to the service are lost)"

// driftMounts checks the two binds the agent cannot work without: the Docker
// socket it proxies into, and the node's sysfs it reads link MTUs from.
func driftMounts(have []mount.Mount) []Finding {
	var out []Finding
	if !hasBind(have, deploy.SocketPath, deploy.SocketPath, false) {
		out = append(out, Finding{
			Field: "mount " + deploy.SocketPath,
			Want:  "bind to " + deploy.SocketPath,
			Got:   mountsAt(have, deploy.SocketPath),
		})
	}
	// Added in v1.30.0. Without it the agent reports its own namespace's MTU,
	// which is 1500 and wrong, instead of saying it does not know.
	if !hasBind(have, deploy.HostSysfsPath, deploy.HostSysfsMount, true) {
		out = append(out, Finding{
			Field: "mount " + deploy.HostSysfsPath,
			Want:  "read-only bind to " + deploy.HostSysfsMount,
			Got:   mountsAt(have, deploy.HostSysfsPath),
		})
	}
	return out
}

// driftSecret checks that the agent still gets credentials.
//
// It is deliberately not a check for OUR secret. `init` mounts the shared
// secret, but agent/deploy/agent-stack.yml provisions the same agent over
// mTLS with swarmexec_ca/cert/key, and a check written around init alone
// would report drift on every manually deployed cluster — the fastest way to
// teach an operator to skip a section. So: our secret is verified down to its
// file name, any other secret is somebody else's deployment and left alone,
// and no secret at all is the one case that is wrong under either model.
func driftSecret(have []*swarm.SecretReference) []Finding {
	named := false
	for _, s := range have {
		if s == nil || s.SecretName != deploy.SecretName {
			continue
		}
		named = true
		if s.File != nil && s.File.Name == deploy.SecretName {
			return nil
		}
	}
	if named {
		return []Finding{{
			Field: "secret " + deploy.SecretName,
			Want:  "mounted as a file named " + deploy.SecretName,
			Got:   "mounted under a different name",
		}}
	}
	if len(have) > 0 {
		return nil // mTLS or another deployment's credentials: not ours to judge
	}
	return []Finding{{
		Field: "secrets",
		Want:  "the shared secret (`init`) or the mTLS certificates (stack file)",
		Got:   "none",
	}}
}

// driftMode checks the three policy choices that make the agent a per-node
// service that comes back: global, restart always, and stop-first updates so
// two agents never fight over the host port.
func driftMode(spec swarm.ServiceSpec) []Finding {
	var out []Finding
	if spec.Mode.Global == nil {
		out = append(out, Finding{Field: "mode", Want: "global", Got: modeName(spec.Mode)})
	}
	if rp := spec.TaskTemplate.RestartPolicy; rp == nil || rp.Condition != swarm.RestartPolicyConditionAny {
		got := "unset"
		if rp != nil {
			got = string(rp.Condition)
		}
		out = append(out, Finding{Field: "restart condition", Want: "any", Got: got})
	}
	if uc := spec.UpdateConfig; uc == nil || uc.Order != swarm.UpdateOrderStopFirst {
		got := "unset"
		if uc != nil {
			got = string(uc.Order)
		}
		out = append(out, Finding{Field: "update order", Want: string(swarm.UpdateOrderStopFirst), Got: got})
	}
	return out
}

func hasBind(have []mount.Mount, source, target string, readOnly bool) bool {
	for _, m := range have {
		if m.Type == mount.TypeBind && m.Source == source && m.Target == target && m.ReadOnly == readOnly {
			return true
		}
	}
	return false
}

// mountsAt describes what is mounted from a source, so the report says what
// is there and not only what is missing.
func mountsAt(have []mount.Mount, source string) string {
	var at []string
	for _, m := range have {
		if m.Source == source {
			s := string(m.Type) + " to " + m.Target
			if m.ReadOnly {
				s += " (read-only)"
			}
			at = append(at, s)
		}
	}
	return orNone(strings.Join(at, ", "))
}

func modeName(m swarm.ServiceMode) string {
	switch {
	case m.Replicated != nil:
		return "replicated"
	case m.ReplicatedJob != nil:
		return "replicated-job"
	case m.GlobalJob != nil:
		return "global-job"
	default:
		return "unset"
	}
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}
