// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package stackfile renders a swarm stack as compose-shaped YAML, so a deployed
// stack and a stack file can be put side by side.
//
// The whole design rests on one decision: BOTH sides are reduced by the same
// function, from the same Go type. A stack file is loaded and run through
// docker's own compose converter, which yields exactly the swarm.ServiceSpec
// values that `docker stack deploy` would submit; a deployed service already IS
// a swarm.ServiceSpec. So there is one normalizer, not two, and the two sides
// cannot drift into disagreeing about what a field means — which is the way a
// comparison tool starts lying.
//
// What this deliberately does NOT do is compare text. A deployed stack carries
// things the file never had — resolved image digests, the daemon's own defaults
// for restart and update policy, the stack namespace prefixed onto every
// network, secret and volume name. Diffing the two as text would report dozens
// of differences for a stack that is exactly in sync, which is worse than no
// tool at all: it trains the reader to ignore the output. Normalizing means the
// diff answers the question actually being asked — "would deploying this file
// change anything?" — not "are these two files spelled the same".
package stackfile

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/moby/moby/api/types/swarm"
)

// Stack is a whole stack, reduced to what can be compared and restored.
type Stack struct {
	Name     string
	Services map[string]*Service
	Networks map[string]*Network
	Secrets  map[string]*Resource
	Configs  map[string]*Resource
	Volumes  map[string]*Resource

	// Notes are what this rendering could not carry. They are not differences
	// and not errors — they are the parts of the truth the format cannot hold,
	// and leaving them out would let an export be mistaken for a backup.
	Notes []string
}

// Service is one service in compose shape. Field order here is the order it
// renders in, chosen so the things an operator changes most sit at the top.
type Service struct {
	Image       string                `yaml:"image,omitempty"`
	Command     []string              `yaml:"command,omitempty"`
	Entrypoint  []string              `yaml:"entrypoint,omitempty"`
	Environment map[string]string     `yaml:"environment,omitempty"`
	User        string                `yaml:"user,omitempty"`
	WorkingDir  string                `yaml:"working_dir,omitempty"`
	Hostname    string                `yaml:"hostname,omitempty"`
	Init        *bool                 `yaml:"init,omitempty"`
	ReadOnly    bool                  `yaml:"read_only,omitempty"`
	StopSignal  string                `yaml:"stop_signal,omitempty"`
	StopGrace   string                `yaml:"stop_grace_period,omitempty"`
	Isolation   string                `yaml:"isolation,omitempty"`
	CapAdd      []string              `yaml:"cap_add,omitempty"`
	CapDrop     []string              `yaml:"cap_drop,omitempty"`
	Sysctls     map[string]string     `yaml:"sysctls,omitempty"`
	DNS         []string              `yaml:"dns,omitempty"`
	DNSSearch   []string              `yaml:"dns_search,omitempty"`
	ExtraHosts  []string              `yaml:"extra_hosts,omitempty"`
	Groups      []string              `yaml:"group_add,omitempty"`
	Ports       []any                 `yaml:"ports,omitempty"`
	Volumes     []string              `yaml:"volumes,omitempty"`
	Networks    map[string]*NetAttach `yaml:"networks,omitempty"`
	Secrets     []*Mounted            `yaml:"secrets,omitempty"`
	Configs     []*Mounted            `yaml:"configs,omitempty"`
	Healthcheck *Healthcheck          `yaml:"healthcheck,omitempty"`
	Labels      map[string]string     `yaml:"labels,omitempty"`
	Logging     *Logging              `yaml:"logging,omitempty"`
	Deploy      *Deploy               `yaml:"deploy,omitempty"`
}

// NetAttach is a service's attachment to one network. Rendered as an empty
// mapping when it carries nothing, which is how compose spells a plain
// attachment.
type NetAttach struct {
	Aliases []string `yaml:"aliases,omitempty"`
}

// Mounted is a secret or config as a service consumes it.
type Mounted struct {
	Source string `yaml:"source"`
	Target string `yaml:"target,omitempty"`
	UID    string `yaml:"uid,omitempty"`
	GID    string `yaml:"gid,omitempty"`
	// A number, not a string: compose's schema requires one, and an exported
	// file has to load again. Both sides carry the raw mode the API reports, so
	// the comparison is numeric however the operator spelled it in YAML.
	Mode uint32 `yaml:"mode,omitempty"`
}

type Healthcheck struct {
	Test        []string `yaml:"test,omitempty"`
	Interval    string   `yaml:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	Retries     uint64   `yaml:"retries,omitempty"`
	StartPeriod string   `yaml:"start_period,omitempty"`
	Disable     bool     `yaml:"disable,omitempty"`
}

type Logging struct {
	Driver  string            `yaml:"driver,omitempty"`
	Options map[string]string `yaml:"options,omitempty"`
}

type Deploy struct {
	Mode          string            `yaml:"mode,omitempty"`
	Replicas      *uint64           `yaml:"replicas,omitempty"`
	Labels        map[string]string `yaml:"labels,omitempty"`
	EndpointMode  string            `yaml:"endpoint_mode,omitempty"`
	Placement     *Placement        `yaml:"placement,omitempty"`
	Resources     *Resources        `yaml:"resources,omitempty"`
	RestartPolicy *RestartPolicy    `yaml:"restart_policy,omitempty"`
	UpdateConfig  *UpdateConfig     `yaml:"update_config,omitempty"`
	Rollback      *UpdateConfig     `yaml:"rollback_config,omitempty"`
}

type Placement struct {
	Constraints []string               `yaml:"constraints,omitempty"`
	Preferences []*PlacementPreference `yaml:"preferences,omitempty"`
	MaxReplicas uint64                 `yaml:"max_replicas_per_node,omitempty"`
}

// PlacementPreference is a mapping in compose ("- spread: node.labels.zone"),
// not a string. Rendering it as a string produced a file that would not load.
type PlacementPreference struct {
	Spread string `yaml:"spread,omitempty"`
}

type Resources struct {
	Limits       *ResourceSet `yaml:"limits,omitempty"`
	Reservations *ResourceSet `yaml:"reservations,omitempty"`
}

type ResourceSet struct {
	CPUs   string `yaml:"cpus,omitempty"`
	Memory string `yaml:"memory,omitempty"`
	PIDs   int64  `yaml:"pids,omitempty"`
}

type RestartPolicy struct {
	Condition   string  `yaml:"condition,omitempty"`
	Delay       string  `yaml:"delay,omitempty"`
	MaxAttempts *uint64 `yaml:"max_attempts,omitempty"`
	Window      string  `yaml:"window,omitempty"`
}

type UpdateConfig struct {
	Parallelism     *uint64 `yaml:"parallelism,omitempty"`
	Delay           string  `yaml:"delay,omitempty"`
	FailureAction   string  `yaml:"failure_action,omitempty"`
	Monitor         string  `yaml:"monitor,omitempty"`
	MaxFailureRatio float32 `yaml:"max_failure_ratio,omitempty"`
	Order           string  `yaml:"order,omitempty"`
}

// Port is compose's long port syntax, used for anything the short "8080:80"
// form cannot express. Host-mode publishing is the case that forced it: written
// as a string it does not load again.
type Port struct {
	Target    uint32 `yaml:"target"`
	Published uint32 `yaml:"published,omitempty"`
	Protocol  string `yaml:"protocol,omitempty"`
	Mode      string `yaml:"mode,omitempty"`
}

// Network is a stack network as compose declares it.
type Network struct {
	Driver     string            `yaml:"driver,omitempty"`
	DriverOpts map[string]string `yaml:"driver_opts,omitempty"`
	Attachable bool              `yaml:"attachable,omitempty"`
	Internal   bool              `yaml:"internal,omitempty"`
	Labels     map[string]string `yaml:"labels,omitempty"`
	External   bool              `yaml:"external,omitempty"`
	Name       string            `yaml:"name,omitempty"`
}

// Resource is a secret, config or volume declaration. Everything a deployed
// stack can tell us about these is a name — the values are either write-only
// (secrets) or live outside the cluster (volume contents) — so an exported
// stack declares them external and says so in the notes.
type Resource struct {
	External bool              `yaml:"external,omitempty"`
	Name     string            `yaml:"name,omitempty"`
	Labels   map[string]string `yaml:"labels,omitempty"`
}

// dur renders a duration the way compose writes one, and nothing at all for
// zero — a zero duration is "unset", and printing "0s" on one side and nothing
// on the other would be a difference that is not one.
func dur(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

func durPtr(d *time.Duration) string {
	if d == nil {
		return ""
	}
	return dur(*d)
}

// envMap turns docker's "KEY=VALUE" slice into a mapping. A mapping is both how
// compose writes it and what makes a diff readable: as a list, inserting one
// variable shifts every line after it and the diff shows the whole block.
//
// A bare "KEY" with no "=" means "take it from the daemon's environment", which
// compose writes as a key with no value; it is kept distinct from "KEY=".
func envMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i >= 0 {
			out[e[:i]] = e[i+1:]
		} else {
			out[e] = ""
		}
	}
	return out
}

// stripNamespace removes the stack's own prefix from a name. Everything inside
// a stack is created as "<stack>_<name>", and the file spells it without the
// prefix — so leaving it on would make every network, secret and volume differ.
func stripNamespace(ns, name string) string {
	return strings.TrimPrefix(name, ns+"_")
}

// sortedKeys is used everywhere a list is rendered, because a diff is only
// readable if the two sides agree on order. Docker returns most of these in
// whatever order the daemon happens to hold them.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bytesValue renders a memory amount the way a stack file spells it, preferring
// exact binary units so "1073741824" reads back as "1G" rather than a number
// nobody can check at a glance.
func bytesValue(n int64) string {
	if n == 0 {
		return ""
	}
	units := []struct {
		suffix string
		size   int64
	}{{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}}
	for _, u := range units {
		if n%u.size == 0 {
			return fmt.Sprintf("%d%s", n/u.size, u.suffix)
		}
	}
	return fmt.Sprintf("%d", n)
}

// nanoCPUs renders a CPU amount as compose writes it: cores, not nano-cores.
func nanoCPUs(n int64) string {
	if n == 0 {
		return ""
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", float64(n)/1e9), "0"), ".")
}

// swarmDefaults are the values the DAEMON fills in when a stack file says
// nothing. They are elided on both sides, which is the difference between a
// useful diff and a useless one: a file that omits restart_policy and a
// deployment carrying the daemon's default for it are in sync, and a tool that
// reports that as a change will be ignored within a day.
//
// Eliding cannot hide a real difference. A value that differs from the default
// is rendered on whichever side has it, so it still shows. The only thing lost
// is "the file states the default explicitly" versus "the file omits it", which
// is not a difference in what the cluster does.
var (
	defaultRestartCondition = string(swarm.RestartPolicyConditionAny)
	defaultRestartDelay     = 5 * time.Second
	defaultUpdateParallel   = uint64(1)
	defaultUpdateFailure    = "pause"
	defaultUpdateOrder      = "stop-first"
	defaultEndpointMode     = string(swarm.ResolutionModeVIP)
)
