// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package secscan

import (
	"sort"
)

// This file adds the second half of the scan: risks that belong to the CLUSTER
// rather than to any one service spec. An unencrypted overlay network is not a
// property of the services on it, and an unlocked manager is a property of no
// service at all — yet both are things an operator has to answer for. The
// service analyzers in analyzers.go are unchanged and keep their own registry;
// nothing here affects the badges in the tree.
//
// The input is deliberately NOT the raw docker API types. Two of the facts
// these analyzers reason about — which services attach to a network, and
// whether a network's driver options mean "encrypted" — are already derived in
// the cli, with rules that are easy to get subtly wrong (a service references a
// network by id OR by name; "encrypted" with an empty value means on). Deriving
// them a second time here is how a security report ends up disagreeing with the
// screen, so the caller passes in what it already knows.

// SubjectKind is the sort of thing a finding is about. The report groups by it.
type SubjectKind string

const (
	SubjectService SubjectKind = "service"
	SubjectNetwork SubjectKind = "network"
	SubjectSecret  SubjectKind = "secret"
	SubjectConfig  SubjectKind = "config"
	SubjectNode    SubjectKind = "node"
	SubjectSwarm   SubjectKind = "swarm"
)

// Subject names one thing a finding is about.
type Subject struct {
	Kind SubjectKind
	Name string
}

// SubjectFinding is a finding together with what it concerns. Service findings
// carry their service as the subject; cluster findings carry a network, a
// secret, a node, or the swarm itself.
type SubjectFinding struct {
	Subject Subject
	Finding Finding
}

// Network is a swarm network reduced to what a security check reads.
type Network struct {
	Name       string
	Driver     string
	Scope      string
	Ingress    bool
	Internal   bool
	Attachable bool
	Encrypted  bool     // data-plane encryption is on
	Services   []string // services attached, by name
}

// Credential is a secret or a config: only its name and who uses it. Secret
// VALUES are never readable through the Docker API and must never appear here.
type Credential struct {
	Name     string
	Services []string
}

// Node is one swarm node. AgentVersion and AgentError are filled in only when
// the caller actually asked the agents; see Cluster.AgentsChecked, which is
// what separates "no agent answered" from "nobody asked".
type Node struct {
	Name          string
	Role          string
	EngineVersion string
	AgentVersion  string
	AgentProto    string
	AgentError    string

	// AgentTooOld marks an agent that predates the version RPC entirely. It is
	// not the same as unreachable: the node answered, and what it said is that
	// it is older than any version this client can ask about.
	AgentTooOld bool
}

// SwarmInfo is the cluster-wide swarm configuration.
type SwarmInfo struct {
	AutoLockManagers bool
}

// Cluster is everything the cluster-level analyzers read. A nil Swarm means the
// swarm configuration could not be read — analyzers must then say so rather
// than assume a default, because the default they would assume ("autolock off")
// is itself a finding.
type Cluster struct {
	Networks []Network
	Secrets  []Credential
	Configs  []Credential
	Nodes    []Node
	Swarm    *SwarmInfo

	// AgentsChecked records whether the caller queried the agents at all. An
	// unchecked agent and an agent that failed to answer are different facts,
	// and reporting the first as the second would invent a problem, while
	// reporting the second as the first would hide one.
	AgentsChecked bool

	// ClientVersion and ClientProto are the build running the scan, for agent
	// skew. Empty means unknown, and the checks then stay quiet rather than
	// comparing against nothing.
	ClientVersion string
	ClientProto   string
}

// ClusterAnalyzer inspects the cluster as a whole. Like Analyzer it must be
// side-effect free and must tolerate zero values throughout — a partially
// gathered Cluster is normal, not exceptional.
type ClusterAnalyzer interface {
	AnalyzeCluster(c Cluster) []SubjectFinding
}

// clusterRegistry is the ordered set of cluster analyzers ScanCluster runs.
var clusterRegistry = []ClusterAnalyzer{
	networkEncryptionAnalyzer{},
	autolockAnalyzer{},
	agentVersionAnalyzer{},
	unusedSecretAnalyzer{},
	unusedConfigAnalyzer{},
}

// RegisterCluster appends a cluster analyzer, for extensions outside this
// package. Not safe for concurrent use with ScanCluster.
func RegisterCluster(a ClusterAnalyzer) { clusterRegistry = append(clusterRegistry, a) }

// ClusterChecks returns what the cluster analyzers look for, in registry order,
// so a report can state what was examined rather than leaving "nothing found"
// to stand on its own.
func ClusterChecks() []string {
	out := make([]string, 0, len(clusterRegistry))
	for _, a := range clusterRegistry {
		if d, ok := a.(Describer); ok {
			out = append(out, d.Describe())
		}
	}
	return out
}

// ScanCluster runs every cluster analyzer and returns the findings, highest
// severity first and stable within a severity.
func ScanCluster(c Cluster) []SubjectFinding {
	var out []SubjectFinding
	for _, a := range clusterRegistry {
		out = append(out, a.AnalyzeCluster(c)...)
	}
	sortSubjectFindings(out)
	return out
}

// sortSubjectFindings orders by severity (worst first), then by subject kind,
// name and rule. The order is total and depends on nothing but the findings
// themselves, so an unchanged cluster yields the same sequence every time —
// which is what lets two reports be diffed against each other.
func sortSubjectFindings(fs []SubjectFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Finding.Severity != b.Finding.Severity {
			return a.Finding.Severity > b.Finding.Severity
		}
		if a.Subject.Kind != b.Subject.Kind {
			return a.Subject.Kind < b.Subject.Kind
		}
		if a.Subject.Name != b.Subject.Name {
			return a.Subject.Name < b.Subject.Name
		}
		return a.Finding.Rule < b.Finding.Rule
	})
}
