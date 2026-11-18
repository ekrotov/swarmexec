// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package mcpaccess decides what an AI assistant connected through
// `swarmexec mcp` may see and do, and keeps the means of that decision out of
// its sight.
//
// Access is granted per service by service labels under one prefix, the way
// Traefik is configured in Swarm mode:
//
//	swarmexec.mcp.enable: "true"   the service exists for the assistant
//	swarmexec.mcp.logs:   "true"   it may read the logs
//	swarmexec.mcp.exec:   "true"   it may run a command in a container
//
// Two properties matter more than the grants themselves. The labels are never
// shown to the assistant (Strip, Scrub), so it cannot learn how the gate works
// by reading it. And a service it may not see is indistinguishable from one
// that does not exist — that part is enforced by the caller, which asks
// Evaluate and treats an invisible service exactly like a missing one.
package mcpaccess

import (
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/secscan"
)

// Prefix is the label namespace. Everything under it is access control.
const Prefix = "swarmexec.mcp."

// The keys under Prefix that mean something. Anything else under the prefix is
// a typo, and a typo in an access label must be reported rather than guessed
// at: honouring "swarmexec.mcp.exce" as exec would grant what nobody wrote,
// and silently ignoring it would leave the operator wondering why nothing
// works.
const (
	KeyEnable = Prefix + "enable"
	KeyLogs   = Prefix + "logs"
	KeyExec   = Prefix + "exec"
)

// marker is what Reserved and Scrub look for: the prefix without its trailing
// dot, lower-cased, so "Swarmexec.MCP.enable" and a bare "swarmexec.mcp" are
// caught too. Hiding a mis-spelled label is the safe direction.
const marker = "swarmexec.mcp"

// Default is what a service without an enable label gets.
type Default int

const (
	// DefaultNone hides unlabelled services: opt-in, like Traefik's
	// exposedByDefault=false. The default, because a forgotten label should
	// cost a missing service, not an exposed one.
	DefaultNone Default = iota
	// DefaultList makes unlabelled services visible — listed, inspected,
	// their stats read — but grants neither logs nor exec. Spelled "list", not
	// "read" (review M-02): "read" sounded like reading their logs, which is
	// exactly what it does not grant. "read" stays accepted.
	DefaultList
)

// ParseDefault reads the --default flag.
func ParseDefault(s string) (Default, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none":
		return DefaultNone, nil
	case "list", "read": // "read" is the old name of "list"
		return DefaultList, nil
	}
	return DefaultNone, errors.New(`--default must be "none" or "list"`)
}

func (d Default) String() string {
	if d == DefaultList {
		return "list"
	}
	return "none"
}

// Access is what the assistant may do with one service.
type Access struct {
	Visible bool // status, tasks, inspect, stats
	Logs    bool
	Exec    bool
}

// Policy turns a service's labels into an Access.
type Policy struct {
	Default Default
}

// Evaluate decides the access a set of service labels grants, and reports the
// labels under the prefix it could not make sense of. Warnings are for the
// operator's log, never for the assistant.
//
// The rules, in order of strength:
//   - an explicit "false" always wins, over the default and over every other
//     label;
//   - an explicit "true" on enable makes the service visible whatever the
//     default is;
//   - without an enable label the default decides visibility;
//   - logs and exec need an explicit "true" of their own on a visible service.
//     The default never grants them.
//
// A value that is neither true nor false counts as false, because the only
// safe reading of a garbled grant is "not granted".
func (p Policy) Evaluate(labels map[string]string) (Access, []string) {
	var warns []string
	flag := func(key string) (val, set bool) {
		raw, ok := labels[key]
		if !ok {
			return false, false
		}
		b, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			warns = append(warns, key+": value "+strconv.Quote(raw)+" is neither true nor false; treated as false")
			return false, true
		}
		return b, true
	}

	enable, enableSet := flag(KeyEnable)
	logs, _ := flag(KeyLogs)
	exec, _ := flag(KeyExec)

	a := Access{Visible: p.Default == DefaultList}
	if enableSet {
		a.Visible = enable
	}
	if a.Visible {
		a.Logs = logs
		a.Exec = exec
	}

	for _, k := range sortedKeys(labels) {
		if !Reserved(k) {
			continue
		}
		switch k {
		case KeyEnable, KeyLogs, KeyExec:
		default:
			warns = append(warns, k+": unknown access label, ignored (known: enable, logs, exec)")
		}
	}
	return a, warns
}

// Reserved reports whether a label key belongs to the access-control
// namespace, and must therefore never reach the assistant.
func Reserved(key string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), marker)
}

// Strip returns labels without the access-control ones, with credential-shaped
// values masked (a Traefik basic-auth hash, a URL with a password). It always
// returns a copy, so a caller can never strip a map the Docker client still
// holds.
func Strip(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if !Reserved(k) {
			out[k] = MaskValue(k, v)
		}
	}
	return out
}

// Scrub is the last line of defence on every piece of text the assistant
// receives: tool output and error messages alike. Every line that mentions the
// namespace is dropped whole.
//
// The structured paths strip labels before anything is rendered, so on a
// correct path this removes nothing. It exists for the paths nobody thought of:
// a log line in which an application prints its own labels, an error message
// from the Docker API that quotes a filter, a future tool that forgets to call
// Strip. Dropping the line rather than masking the word keeps the redaction
// itself from being a signal.
func Scrub(s string) string {
	if !strings.Contains(strings.ToLower(s), marker) {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	var b strings.Builder
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), marker) {
			continue
		}
		b.WriteString(l)
	}
	return b.String()
}

// Masked replaces a credential's value wherever one is shown.
const Masked = "<masked>"

// MaskEnv masks credentials in KEY=VALUE pairs: the whole value when the key
// names one (MaskValue, a wider rule than the scan's), otherwise just the
// credential-shaped part of it (a URL's password, a JWT). The key stays
// visible: knowing that DB_PASSWORD is set is useful, its value is not ours to
// hand to a model provider.
func MaskEnv(env []string) []string {
	if env == nil {
		return nil
	}
	out := make([]string, len(env))
	for i, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			out[i] = MaskText(kv)
			continue
		}
		out[i] = k + "=" + MaskValue(k, v)
	}
	return out
}

// MaskEnvMap is MaskEnv for the map form compose and the stack model use.
func MaskEnvMap(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = MaskValue(k, v)
	}
	return out
}

// ExposesDockerSocket reports whether a service's containers can reach the
// Docker daemon through a mount. Through the socket a process can start a
// privileged container, or simply relabel any service — including itself — and
// so grant the assistant whatever it likes. No label can make that safe, so
// exec into such a service is refused outright.
//
// It is the security scan's docker-socket rule plus two cases that rule does
// not flag, because for a scan they are a different finding but for this gate
// they are the same hole: a bind of a directory the socket lives in (/, /var,
// /var/run, /run), and the Windows named pipe.
func ExposesDockerSocket(spec swarm.ServiceSpec) bool {
	cs := spec.TaskTemplate.ContainerSpec
	if _, ok := secscan.DockerSocketMount(cs); ok {
		return true
	}
	if cs == nil {
		return false
	}
	for _, m := range cs.Mounts {
		switch m.Type {
		case mount.TypeBind:
			if socketParent(m.Source) {
				return true
			}
		case mount.TypeNamedPipe:
			if strings.Contains(strings.ToLower(m.Source), "docker_engine") {
				return true
			}
		}
	}
	return false
}

// socketParent reports whether a bind source is a directory that contains the
// Docker socket at its usual place.
func socketParent(src string) bool {
	src = strings.TrimSpace(src)
	if src == "" {
		return false
	}
	switch strings.TrimRight(src, "/") {
	case "", "/var", "/var/run", "/run": // "" is the root itself
		return true
	}
	return false
}

// ErrLabelChange is what a write is refused with when it would touch the
// access labels. It names no label: the assistant learns that the change is
// refused, not which part of it would have opened a door.
var ErrLabelChange = errors.New("change refused: it would alter access settings that cannot be changed through this interface")

// GuardSpecChange refuses a proposed service update that adds, changes or
// removes any access label, on the service or on its containers.
//
// It compares the spec BEFORE the change with the spec AFTER it, not the label
// names a request mentions. A request is free to describe its change however
// it likes — replace all labels, merge a map, rewrite the whole spec — and a
// check on its wording is a check an assistant can word its way around. The
// before/after state is what will actually be submitted, so it is the only
// thing worth comparing.
//
// No tool writes yet. This exists so that the first one that does cannot ship
// without it, and it is tested on its own for the same reason.
func GuardSpecChange(before, after swarm.ServiceSpec) error {
	if !sameReserved(before.Labels, after.Labels) {
		return ErrLabelChange
	}
	if !sameReserved(containerLabels(before), containerLabels(after)) {
		return ErrLabelChange
	}
	return nil
}

func containerLabels(s swarm.ServiceSpec) map[string]string {
	if cs := s.TaskTemplate.ContainerSpec; cs != nil {
		return cs.Labels
	}
	return nil
}

// sameReserved reports whether two label sets agree on every access label.
func sameReserved(a, b map[string]string) bool {
	for k, v := range a {
		if !Reserved(k) {
			continue
		}
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	for k := range b {
		if !Reserved(k) {
			continue
		}
		if _, ok := a[k]; !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ReachesDockerAPI reports whether a service's environment points its
// containers at a Docker API over the network (DOCKER_HOST=tcp://…, ssh://…,
// or a unix socket in a path a mount may not show). Such a service can relabel
// any service exactly as one with the socket mounted can, so the exec gate
// treats the two alike.
func ReachesDockerAPI(spec swarm.ServiceSpec) bool {
	cs := spec.TaskTemplate.ContainerSpec
	if cs == nil {
		return false
	}
	for _, kv := range cs.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(strings.TrimSpace(k), "DOCKER_HOST") && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// IsSocketProxyImage reports whether an image is a well-known Docker socket
// proxy. A proxy hands out (a filtered slice of) the Docker API to everything
// on its network, so a service that shares a network with one has the same
// reach as DOCKER_HOST.
func IsSocketProxyImage(image string) bool {
	i := strings.ToLower(image)
	return strings.Contains(i, "socket-proxy") || strings.Contains(i, "docker-proxy") || strings.Contains(i, "dockersocketproxy")
}

// SocketProxyNetworks returns the network targets (ids or names) of every
// service whose image is a Docker socket proxy.
func SocketProxyNetworks(svcs []swarm.Service) map[string]bool {
	out := map[string]bool{}
	for _, s := range svcs {
		if cs := s.Spec.TaskTemplate.ContainerSpec; cs == nil || !IsSocketProxyImage(cs.Image) {
			continue
		}
		for _, n := range s.Spec.TaskTemplate.Networks {
			if t := strings.TrimSpace(n.Target); t != "" {
				out[t] = true
			}
		}
	}
	return out
}

// SharesNetwork reports whether the service is attached to any of nets.
func SharesNetwork(spec swarm.ServiceSpec, nets map[string]bool) bool {
	for _, n := range spec.TaskTemplate.Networks {
		if nets[strings.TrimSpace(n.Target)] {
			return true
		}
	}
	return false
}
