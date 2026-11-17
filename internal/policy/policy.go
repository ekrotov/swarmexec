// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package policy is the agent's optional rule file: which actions a request
// may take, by action, operator identity, service, port, exec user and volume.
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
//
// "kill" and "inspect" are deliberately not "container.kill" /
// "container.inspect": a policy written before they existed that allows
// "container.*" (meaning the listing and the event watch) must not silently
// start allowing an operator to stop workloads, or to read a container's
// environment (Config.Env, where credentials routinely live). Only "*" covers
// them — a rule that says "everything" means it.
var Actions = []string{
	"container.list", "container.events", "inspect", "kill",
	"exec", "logs", "portforward", "stats.read",
	"image.list", "image.prune", "image.prune.all",
	"volume.list", "volume.create", "volume.remove", "network.inspect",
	"reclaim.read", "container.prune", "buildcache.prune", "network.prune", "volume.prune",
}

// Conditions says which of a rule's conditions an action can ever carry. The
// agent fills a Request field only where the RPC has the matching fact: a
// service for the container RPCs, a port for portforward, a user for exec, a
// volume name for volume.create and volume.remove. A condition on an action
// that never carries it can never hold, so Parse refuses such a rule instead
// of accepting one that silently matches nothing.
type Conditions struct {
	Services, Ports, Users, Volumes bool
}

// carries is the table; agent/internal/server's TestRequestFieldsMatchPolicyTable
// checks it against what every handler really puts in its Request.
var carries = map[string]Conditions{
	"exec":             {Services: true, Users: true},
	"logs":             {Services: true},
	"portforward":      {Services: true, Ports: true},
	"container.events": {Services: true},
	"kill":             {Services: true},
	"inspect":          {Services: true},
	"volume.create":    {Volumes: true},
	"volume.remove":    {Volumes: true},
}

// Carries reports which conditions action can carry (the zero value for an
// action that carries none, such as image.prune or stats.read).
func Carries(action string) Conditions { return carries[action] }

// Request is what a rule is matched against.
type Request struct {
	Action   string
	Identity string
	Service  string
	Port     uint32
	// User is the exec user as requested ("" without -u). EffectiveUser is the
	// user the process will run as: the requested one, else the image's, else
	// root. A rule's users match either.
	User          string
	EffectiveUser string
	Volume        string
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
	volumes    []string
	reason     string
}

type fileRule struct {
	Allow      []string `yaml:"allow"`
	Deny       []string `yaml:"deny"`
	Identities []string `yaml:"identities"`
	Services   []string `yaml:"services"`
	Ports      []uint32 `yaml:"ports"`
	Users      []string `yaml:"users"`
	Volumes    []string `yaml:"volumes"`
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
	allowByDefault, err := parseDefault(f.Default)
	if err != nil {
		return nil, err
	}
	p := &Policy{allowByDefault: allowByDefault}
	for i, fr := range f.Rules {
		r, err := parseRule(i+1, fr, identityVerified)
		if err != nil {
			return nil, err
		}
		p.rules = append(p.rules, r)
	}
	return p, nil
}

// parseDefault reads the required "default" key: true for allow.
func parseDefault(def string) (bool, error) {
	switch def {
	case "allow":
		return true, nil
	case "deny":
		return false, nil
	case "":
		return false, errors.New(`policy: "default" is required — allow or deny, for requests no rule matches`)
	default:
		return false, fmt.Errorf(`policy: default must be allow or deny, not %q`, def)
	}
}

// parseRule checks rule n (1-based): one direction, known actions, valid
// patterns, and identities only where the agent can verify them.
func parseRule(n int, fr fileRule, identityVerified bool) (rule, error) {
	r := rule{n: n, identities: fr.Identities, services: fr.Services, ports: fr.Ports, users: fr.Users, volumes: fr.Volumes, reason: fr.Reason}
	switch {
	case len(fr.Allow) > 0 && len(fr.Deny) > 0:
		return rule{}, fmt.Errorf("policy rule %d: has both allow and deny; a rule decides one way", n)
	case len(fr.Allow) > 0:
		r.allow, r.actions = true, fr.Allow
	case len(fr.Deny) > 0:
		r.actions = fr.Deny
	default:
		return rule{}, fmt.Errorf("policy rule %d: names no actions (allow: [...] or deny: [...])", n)
	}
	for _, a := range r.actions {
		if !matchesAnyAction(a) {
			return rule{}, fmt.Errorf("policy rule %d: %q is not an action — known: %s", n, a, strings.Join(Actions, ", "))
		}
	}
	for _, pat := range slices.Concat(r.identities, r.services, r.users, r.volumes) {
		if _, err := path.Match(pat, ""); err != nil {
			return rule{}, fmt.Errorf("policy rule %d: bad pattern %q: %v", n, pat, err)
		}
	}
	if err := r.checkConditions(); err != nil {
		return rule{}, err
	}
	if len(r.identities) > 0 && !identityVerified {
		return rule{}, fmt.Errorf("policy rule %d matches on identities, but this agent cannot verify identities: "+
			"with a shared secret and no client CA every operator presents the same credential and names "+
			"themselves — run the agent with -ca-cert and client certificates, or drop the identities", n)
	}
	return r, nil
}

// checkConditions refuses a condition that none of the rule's actions can
// carry: it would never hold, so the rule would silently match nothing.
func (r rule) checkConditions() error {
	var matched []string
	for _, a := range Actions {
		if anyGlob(r.actions, a) {
			matched = append(matched, a)
		}
	}
	conds := []struct {
		name string
		set  bool
		has  func(Conditions) bool
	}{
		{"services", len(r.services) > 0, func(c Conditions) bool { return c.Services }},
		{"ports", len(r.ports) > 0, func(c Conditions) bool { return c.Ports }},
		{"users", len(r.users) > 0, func(c Conditions) bool { return c.Users }},
		{"volumes", len(r.volumes) > 0, func(c Conditions) bool { return c.Volumes }},
	}
	for _, c := range conds {
		if !c.set || slices.ContainsFunc(matched, func(a string) bool { return c.has(Carries(a)) }) {
			continue
		}
		var carrying []string
		for _, a := range Actions {
			if c.has(Carries(a)) {
				carrying = append(carrying, a)
			}
		}
		return fmt.Errorf("policy rule %d: %s never applies to %s — only %s carry it",
			r.n, c.name, strings.Join(matched, ", "), strings.Join(carrying, ", "))
	}
	return nil
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
		(len(r.users) == 0 || anyGlob(r.users, userForms(q.User, q.EffectiveUser)...)) &&
		(len(r.volumes) == 0 || (q.Volume != "" && anyGlob(r.volumes, q.Volume)))
}

// userForms are the strings a rule's users patterns are tried against: for the
// requested and the effective user, the whole "user:group" and its user part,
// numeric ids without leading zeros ("00" is root's uid 0). The agent cannot
// map a name to an id inside the image, so "root" and "0" are different forms:
// list both.
func userForms(users ...string) []string {
	var out []string
	for _, u := range users {
		usr, _, _ := strings.Cut(u, ":")
		out = append(out, normID(u), normID(usr))
	}
	return out
}

// normID strips leading zeros from the numeric parts of "user[:group]".
func normID(u string) string {
	parts := strings.Split(u, ":")
	for i, p := range parts {
		if t := strings.TrimLeft(p, "0"); p != "" && strings.Trim(p, "0123456789") == "" {
			if t == "" {
				t = "0"
			}
			parts[i] = t
		}
	}
	return strings.Join(parts, ":")
}

func anyGlob(pats []string, ss ...string) bool {
	for _, p := range pats {
		for _, s := range ss {
			if ok, _ := path.Match(p, s); ok {
				return true
			}
		}
	}
	return false
}
