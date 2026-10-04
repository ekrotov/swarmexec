// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package secscan runs a set of static security analyzers over a Docker Swarm
// service spec and returns the findings. It is intentionally small and
// extensible: add a new Analyzer to the registry (see analyzers.go) or via
// Register, and every surface that calls Scan picks it up — the UI does not need
// to know about individual rules.
package secscan

import (
	"sort"

	"github.com/moby/moby/api/types/swarm"
)

// Severity ranks a finding. Higher is worse; the UI tints and orders by it.
type Severity int

const (
	SevLow Severity = iota
	SevMedium
	SevHigh
)

func (s Severity) String() string {
	switch s {
	case SevHigh:
		return "high"
	case SevMedium:
		return "medium"
	default:
		return "low"
	}
}

// Finding is one identified security risk for a service. Detail must never
// contain a secret value — only names and explanations.
type Finding struct {
	Rule     string // stable analyzer id, e.g. "root-user"
	Title    string // short label, e.g. "runs as root"
	Detail   string // one-line explanation (no secret values)
	Severity Severity
}

// Analyzer inspects a service spec and returns zero or more findings. An
// analyzer must be side-effect free and tolerate a nil ContainerSpec.
type Analyzer interface {
	Analyze(svc swarm.Service) []Finding
}

// registry is the ordered set of analyzers Scan runs. Extend it in analyzers.go
// or at runtime via Register to add a new class of risk.
var registry = []Analyzer{
	// High-severity first only for readability; Scan sorts by severity anyway.
	dockerSocketAnalyzer{},
	capabilityAnalyzer{},
	hostNetworkAnalyzer{},
	confinementAnalyzer{},
	rootUserAnalyzer{},
	secretEnvAnalyzer{},
	// Informational (SevLow): true of almost every service, so they never mark
	// a row on their own — see Actionable.
	resourceLimitAnalyzer{},
	imagePinAnalyzer{},
}

// Register appends an analyzer to the registry, for extensions outside this
// package. Not safe for concurrent use with Scan; call it during init/setup.
func Register(a Analyzer) { registry = append(registry, a) }

// Describer lets an analyzer say, in a few words, what it looks for. The UI
// lists these so "no risks found" can state what was actually checked, and so
// that list cannot drift from the registry the way a hardcoded one does.
// Analyzers that do not implement it are simply left out of the list.
type Describer interface{ Describe() string }

// Checks returns what the registered analyzers look for, in registry order.
func Checks() []string {
	out := make([]string, 0, len(registry))
	for _, a := range registry {
		if d, ok := a.(Describer); ok {
			out = append(out, d.Describe())
		}
	}
	return out
}

// Scan runs every analyzer over svc and returns the findings, highest severity
// first (stable within a severity, ordered by rule then title).
func Scan(svc swarm.Service) []Finding {
	var out []Finding
	for _, a := range registry {
		out = append(out, a.Analyze(svc)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity > out[j].Severity
		}
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// MaxSeverity returns the highest severity among findings; SevLow for none.
func MaxSeverity(fs []Finding) Severity {
	m := SevLow
	for _, f := range fs {
		if f.Severity > m {
			m = f.Severity
		}
	}
	return m
}

// Actionable reports whether findings warrant flagging the service in the UI:
// at least one finding above SevLow. Low findings are informational and hold
// for nearly every service (an unset User is the Swarm default), so badging
// them would mark almost every row and destroy the at-a-glance signal. They are
// still returned by Scan and listed once a service is flagged for another reason.
func Actionable(fs []Finding) bool {
	return MaxSeverity(fs) > SevLow
}

// containerSpec is the (possibly nil) container spec of a service's task.
func containerSpec(svc swarm.Service) *swarm.ContainerSpec {
	return svc.Spec.TaskTemplate.ContainerSpec
}
