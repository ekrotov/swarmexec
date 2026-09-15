// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"fmt"
	"sort"
	"strings"
)

// networkEncryptionAnalyzer flags overlay networks that carry service traffic
// without data-plane encryption.
//
// Swarm's overlay driver tunnels container traffic between nodes over VXLAN,
// and by default that tunnel is NOT encrypted: anything with a view of the
// wire between two nodes sees the traffic in the clear. Encryption is opt-in
// per network (`--opt encrypted`), which is easy to forget precisely because
// nothing about the running service looks different either way.
//
// Only networks with at least one service attached are flagged. An overlay with
// nothing on it carries nothing, and reporting it would bury the ones that do.
// Ingress is skipped for a harder reason: the routing-mesh network cannot be
// created with encryption through the normal path, so flagging it would put a
// finding in every report that no one can ever clear.
type networkEncryptionAnalyzer struct{}

func (networkEncryptionAnalyzer) Describe() string {
	return "overlay networks carrying service traffic unencrypted"
}

func (networkEncryptionAnalyzer) AnalyzeCluster(c Cluster) []SubjectFinding {
	var out []SubjectFinding
	for _, n := range c.Networks {
		if !strings.EqualFold(n.Driver, "overlay") || n.Ingress || n.Encrypted || len(n.Services) == 0 {
			continue
		}
		out = append(out, SubjectFinding{
			Subject: Subject{Kind: SubjectNetwork, Name: n.Name},
			Finding: Finding{
				Rule:  "network-unencrypted",
				Title: "overlay traffic is not encrypted",
				Detail: fmt.Sprintf(
					"%s carries the traffic of %s between nodes over an unencrypted VXLAN tunnel; "+
						"create it with --opt encrypted to encrypt the data plane",
					n.Name, serviceList(n.Services)),
				Severity: SevMedium,
			},
		})
	}
	return out
}

// autolockAnalyzer reports whether the managers' raft store is locked at rest.
//
// A swarm manager keeps every secret, every config and the cluster CA key in
// its raft store, and the key to that store sits on the same disk unless
// autolock is on. Someone who walks off with a manager's disk has the lot.
// Turning autolock on means a restarted manager needs its unlock key, which is
// a real operational cost — so this is reported as a medium, a decision to take
// knowingly, not a defect.
type autolockAnalyzer struct{}

func (autolockAnalyzer) Describe() string { return "manager autolock (raft encrypted at rest)" }

func (autolockAnalyzer) AnalyzeCluster(c Cluster) []SubjectFinding {
	// Unknown is its own answer. Defaulting to "off" would be the same as
	// inventing the finding, and defaulting to "on" would be a false all-clear
	// on the one thing protecting every secret in the cluster.
	if c.Swarm == nil {
		return []SubjectFinding{{
			Subject: Subject{Kind: SubjectSwarm, Name: "swarm"},
			Finding: Finding{
				Rule:     "autolock-unknown",
				Title:    "autolock could not be determined",
				Detail:   "the swarm configuration could not be read, so whether the managers' raft store is encrypted at rest is unknown",
				Severity: SevLow,
			},
		}}
	}
	if c.Swarm.AutoLockManagers {
		return nil
	}
	return []SubjectFinding{{
		Subject: Subject{Kind: SubjectSwarm, Name: "swarm"},
		Finding: Finding{
			Rule:     "autolock-disabled",
			Title:    "managers are not autolocked",
			Detail:   "the raft store holding every secret, config and the cluster CA key is not encrypted at rest; a manager's disk yields all of it. Enable with docker swarm update --autolock=true, and keep the unlock key — a restarted manager will need it",
			Severity: SevMedium,
		},
	}}
}

// agentVersionAnalyzer reports nodes whose swarmexec agent does not match the
// client, and nodes that did not answer at all.
//
// It matters for a security report because the agent is what enforces
// authorization on every exec, log and port-forward: an agent older than the
// client may not implement a rule the client believes is in force. An agent
// that did not answer is reported too — a node nobody could reach is a gap in
// the report, and silence there must not read as a pass.
type agentVersionAnalyzer struct{}

func (agentVersionAnalyzer) Describe() string { return "agent version skew across nodes" }

func (agentVersionAnalyzer) AnalyzeCluster(c Cluster) []SubjectFinding {
	if !c.AgentsChecked {
		return nil // nobody asked; the report says so in its own gap note
	}
	var out []SubjectFinding
	add := func(n Node, rule, title, detail string, sev Severity) {
		out = append(out, SubjectFinding{
			Subject: Subject{Kind: SubjectNode, Name: n.Name},
			Finding: Finding{Rule: rule, Title: title, Detail: detail, Severity: sev},
		})
	}
	for _, n := range c.Nodes {
		switch {
		case n.AgentTooOld:
			add(n, "agent-too-old", "agent predates this client",
				"the agent is older than the version RPC, so it cannot be asked what it is; it certainly does not implement the newest authorization rules. Redeploy with swarmexec init --force",
				SevMedium)
		case n.AgentError != "":
			add(n, "agent-unreachable", "agent did not answer",
				"this node's agent could not be reached ("+oneLine(n.AgentError)+"), so nothing on it was checked — this is a gap in the report, not a clean result",
				SevLow)
		// A protocol mismatch outranks a version difference: the two ends
		// disagree about the wire contract itself, not merely about which build
		// implements it.
		case c.ClientProto != "" && n.AgentProto != "" && n.AgentProto != c.ClientProto:
			add(n, "agent-proto-mismatch", "agent speaks a different protocol",
				fmt.Sprintf("the agent reports protocol %s against this client's %s; the two disagree about the contract they are enforcing. Redeploy with swarmexec init --force",
					n.AgentProto, c.ClientProto),
				SevHigh)
		// Skipped for an unreleased client, which differs from every released
		// agent by construction — the same exemption `swarmexec doctor` makes,
		// and for the same reason.
		case releasedVersion(c.ClientVersion) && n.AgentVersion != "" && n.AgentVersion != c.ClientVersion:
			add(n, "agent-version-skew", "agent is not the client's version",
				fmt.Sprintf("the agent reports %s while this client is %s; an agent older than the client may not enforce a rule the client assumes. Redeploy with swarmexec init --force",
					n.AgentVersion, c.ClientVersion),
				SevMedium)
		}
	}
	return out
}

// releasedVersion reports whether a client version is one that an agent could
// meaningfully be compared against. A local build calls itself "dev" and will
// never match a released agent, so comparing it would put a finding on every
// node of every cluster a developer ever scans.
func releasedVersion(v string) bool { return v != "" && v != "dev" }

// unusedSecretAnalyzer flags secrets no service references.
//
// A secret that nothing uses is still distributed through the raft store and
// still readable by anything that reaches a manager. It is usually a credential
// someone rotated and never removed — which means the old value is still live
// in the cluster long after everyone believes it is gone. Low, because it is
// not exposed to a running container: it is housekeeping with a sharp edge.
type unusedSecretAnalyzer struct{}

func (unusedSecretAnalyzer) Describe() string { return "secrets and configs no service uses" }

func (unusedSecretAnalyzer) AnalyzeCluster(c Cluster) []SubjectFinding {
	return unusedCredentials(c.Secrets, SubjectSecret, "secret", "unused-secret")
}

// unusedConfigAnalyzer is the same check for configs. Kept separate from
// secrets so a report can tell the two apart at a glance: a stale config is
// clutter, a stale secret is a credential that outlived its rotation.
type unusedConfigAnalyzer struct{}

func (unusedConfigAnalyzer) AnalyzeCluster(c Cluster) []SubjectFinding {
	return unusedCredentials(c.Configs, SubjectConfig, "config", "unused-config")
}

func unusedCredentials(creds []Credential, kind SubjectKind, noun, rule string) []SubjectFinding {
	var out []SubjectFinding
	for _, cr := range creds {
		if len(cr.Services) > 0 {
			continue
		}
		out = append(out, SubjectFinding{
			Subject: Subject{Kind: kind, Name: cr.Name},
			Finding: Finding{
				Rule:     rule,
				Title:    "no service uses this " + noun,
				Detail:   "the " + noun + " still exists in the cluster but no service references it; if it was rotated, the old value is still here",
				Severity: SevLow,
			},
		})
	}
	return out
}

// serviceList renders attached service names for a detail line: a few by name,
// then a count. Naming all of them would put an unbounded line into a table
// cell on a busy network.
func serviceList(svcs []string) string {
	names := append([]string(nil), svcs...)
	sort.Strings(names)
	switch {
	case len(names) == 0:
		return "no services"
	case len(names) == 1:
		return names[0]
	case len(names) <= 3:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	default:
		return fmt.Sprintf("%s and %d more services", strings.Join(names[:2], ", "), len(names)-2)
	}
}

// oneLine flattens whitespace so a detail cannot break the table it lands in.
// Error strings from a dial failure routinely carry newlines.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
