// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package policy is the agent's optional rule file: which actions a request
// may take, by action, operator identity, service, port and exec user.
//
// What it is, said plainly because the opposite would be worse: a guard
// against ACCIDENTS and a record of intent — no sweeping image prune on
// production, no port-forward to the database, no root shell — not access
// control. Every write a client makes to services, secrets, networks and
// stacks goes to the swarm MANAGER with the operator's own Docker credentials
// and never passes an agent; whoever holds those credentials can deploy a
// service that mounts the Docker socket and is root on every node. A policy
// that forbids exec to a user who may still rebuild the cluster is not a
// fence, and is not sold as one.
//
// Shared by the agent, which evaluates it, and `swarmexec init`, which checks
// a file before deploying it, so a typo fails on the operator's machine and
// not as an agent that will not start on every node.
package policy

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Actions are every action an agent authorizes. A rule may name them exactly
// or with a glob ("volume.*", "*").
var Actions = []string{
	"container.list", "container.events",
	"exec", "logs", "portforward", "stats.read",
	"image.list", "image.prune", "image.prune.all",
	"volume.list", "volume.create", "volume.remove",
}

// Request is what a rule is matched against.
type Request struct {
	Action   string
	Identity string
	Service  string
	Port     uint32
	User     string
}

// Policy is a parsed rule file.
type Policy struct {
	allowByDefault bool
	rules          []rule
}

type rule struct {
	n          int // 1-based, as an operator counts them in the file
	allow      bool
	actions    []string
	identities []string
	services   []string
	ports      []uint32
	users      []string
	reason     string
}

type fileRule struct {
	Allow      []string `yaml:"allow"`
	Deny       []string `yaml:"deny"`
	Identities []string `yaml:"identities"`
	Services   []string `yaml:"services"`
	Ports      []uint32 `yaml:"ports"`
	Users      []string `yaml:"users"`
	Reason     string   `yaml:"reason"`
}

type file struct {
	Default string     `yaml:"default"`
	Rules   []fileRule `yaml:"rules"`
}

// Parse reads a rule file. identityVerified says whether the agent can trust
// the identity on a request: with shared-secret auth and no client CA it is
// whatever the client claims, so rules on identities are refused outright —
// they would read like a restriction and restrict nothing.
func Parse(data []byte, identityVerified bool) (*Policy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a misspelt key is an error, not an ignored rule
	var f file
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	p := &Policy{}
	switch f.Default {
	case "allow":
		p.allowByDefault = true
	case "deny":
	case "":
		return nil, errors.New(`policy: "default" is required — allow or deny, for requests no rule matches`)
	default:
		return nil, fmt.Errorf(`policy: default must be allow or deny, not %q`, f.Default)
	}
	for i, fr := range f.Rules {
		n := i + 1
		r := rule{n: n, identities: fr.Identities, services: fr.Services, ports: fr.Ports, users: fr.Users, reason: fr.Reason}
		switch {
		case len(fr.Allow) > 0 && len(fr.Deny) > 0:
			return nil, fmt.Errorf("policy rule %d: has both allow and deny; a rule decides one way", n)
		case len(fr.Allow) > 0:
			r.allow, r.actions = true, fr.Allow
		case len(fr.Deny) > 0:
			r.actions = fr.Deny
		default:
			return nil, fmt.Errorf("policy rule %d: names no actions (allow: [...] or deny: [...])", n)
		}
		for _, a := range r.actions {
			if !matchesAnyAction(a) {
				return nil, fmt.Errorf("policy rule %d: %q is not an action — known: %s", n, a, strings.Join(Actions, ", "))
			}
		}
		for _, pat := range append(append(append([]string{}, r.identities...), r.services...), r.users...) {
			if _, err := path.Match(pat, ""); err != nil {
				return nil, fmt.Errorf("policy rule %d: bad pattern %q: %v", n, pat, err)
			}
		}
		if len(r.identities) > 0 && !identityVerified {
			return nil, fmt.Errorf("policy rule %d matches on identities, but this agent cannot verify identities: "+
				"with a shared secret and no client CA every operator presents the same credential and names "+
				"themselves — run the agent with -ca-cert and client certificates, or drop the identities", n)
		}
		p.rules = append(p.rules, r)
	}
	return p, nil
}

func matchesAnyAction(pat string) bool {
	for _, a := range Actions {
		if ok, _ := path.Match(pat, a); ok {
			return true
		}
	}
	return false
}

// Decide returns whether req is allowed and why. The first rule whose every
// condition matches decides; with none, the default does.
func (p *Policy) Decide(req Request) (bool, string) {
	for _, r := range p.rules {
		if r.matches(req) {
			verb := "deny"
			if r.allow {
				verb = "allow"
			}
			why := fmt.Sprintf("policy rule %d (%s %s)", r.n, verb, strings.Join(r.actions, ","))
			if r.reason != "" {
				why += ": " + r.reason
			}
			return r.allow, why
		}
	}
	if p.allowByDefault {
		return true, "policy default: allow (no rule matched)"
	}
	return false, "policy default: deny (no rule matched)"
}

// matches: every condition the rule names must hold; within one condition,
// any listed value will do.
func (r rule) matches(q Request) bool {
	return anyGlob(r.actions, q.Action) &&
		(len(r.identities) == 0 || anyGlob(r.identities, q.Identity)) &&
		(len(r.services) == 0 || (q.Service != "" && anyGlob(r.services, q.Service))) &&
		(len(r.ports) == 0 || slices.Contains(r.ports, q.Port)) &&
		(len(r.users) == 0 || anyGlob(r.users, q.User))
}

func anyGlob(pats []string, s string) bool {
	for _, p := range pats {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}
