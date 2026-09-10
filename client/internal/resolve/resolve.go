// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

// Package resolve turns an operator-supplied target (service, service.slot,
// task ID, or container ID) into a concrete dial endpoint: the node address to
// connect the agent on, plus the full container ID to exec into. It talks to
// the Swarm manager via the Docker SDK, behind the DockerClient interface so the
// logic is unit-testable with a fake. See REQUIREMENTS §4.
package resolve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/system"

	"swarmexec/client/internal/secscan"
)

// DockerClient is the subset of the Docker SDK the resolver needs. The real
// *client.Client satisfies it; tests supply a fake.
type DockerClient interface {
	ServiceList(ctx context.Context, options types.ServiceListOptions) ([]swarm.Service, error)
	TaskList(ctx context.Context, options types.TaskListOptions) ([]swarm.Task, error)
	NodeInspectWithRaw(ctx context.Context, nodeID string) (swarm.Node, []byte, error)
	NodeList(ctx context.Context, options types.NodeListOptions) ([]swarm.Node, error)
	Info(ctx context.Context) (system.Info, error)
}

// Node is a swarm node the cli can dial an agent on.
type Node struct {
	ID       string
	Name     string // hostname
	DialHost string // host/IP to reach the agent on
}

// Nodes lists the ready swarm nodes with the address to dial each agent on.
func (r *Resolver) Nodes(ctx context.Context) ([]Node, error) {
	nodes, err := r.cli.NodeList(ctx, types.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	var out []Node
	for _, n := range nodes {
		if n.Status.State != swarm.NodeStateReady {
			continue // a down node's agent is unreachable anyway
		}
		out = append(out, Node{
			ID:       n.ID,
			Name:     n.Description.Hostname,
			DialHost: r.dialHost(ctx, n),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// AddrMode selects how a node's dial address is derived.
type AddrMode int

const (
	// AddrHostname dials the node's reported hostname.
	AddrHostname AddrMode = iota
	// AddrIP dials the node's advertised IP address.
	AddrIP
)

// Endpoint is the resolved dial target.
type Endpoint struct {
	DialHost    string // host (or IP) to connect the agent on
	ContainerID string // full container ID to exec into
	NodeID      string
	NodeName    string
}

// Candidate describes one running task that could satisfy a target. Used both
// for multi-replica disambiguation and for `ps`.
type Candidate struct {
	Service     string
	Slot        int
	TaskID      string
	NodeID      string
	NodeName    string // node hostname
	NodeAddr    string // node advertised IP address
	DialHost    string
	ContainerID string
	Uptime      time.Duration
}

// Service is a swarm service with its running/desired task counts, for the
// containers overview — which lists every service, including those with zero
// running tasks (which Candidates omits).
type Service struct {
	Name     string
	Running  int
	Desired  int
	Global   bool
	Mode     string // replicated | global | replicated-job | global-job
	Image    string // container image, digest stripped for display
	ImageRef string // full container image as pinned in the spec (digest kept)
	Ports    string // published ports, e.g. "*:80->80/tcp"; "" if none

	// Stack is the stack this service belongs to, from the
	// com.docker.stack.namespace label `docker stack deploy` sets. Empty for a
	// standalone service (one created with `docker service create`).
	Stack string

	// UpdateState is the swarm rolling-update state, e.g. "updating", "paused",
	// "rollback_started". Empty when no update is in flight; "completed" /
	// "rollback_completed" once one finished. Lets the UI flag a service that is
	// mid-update.
	UpdateState string

	// Risks holds the static security-scan findings for this service (empty when
	// none). Computed from the spec during the list fetch, so the UI can mark a
	// risky service and list its findings without a second call.
	Risks []secscan.Finding
}

// stackNamespaceLabel is the label `docker stack deploy` stamps on every service
// it creates, naming the stack. It is the only link between a service and its
// stack — swarm has no stack object.
const stackNamespaceLabel = "com.docker.stack.namespace"

// AmbiguousError is returned when a bare service name has more than one running
// task and the caller must disambiguate (slot or interactive pick).
type AmbiguousError struct {
	Service    string
	Candidates []Candidate
}

func (e *AmbiguousError) Error() string {
	slots := make([]string, 0, len(e.Candidates))
	for _, c := range e.Candidates {
		slots = append(slots, fmt.Sprintf("%s.%d", e.Service, c.Slot))
	}
	return fmt.Sprintf("service %q has %d running tasks, specify a slot (%s) or pick interactively",
		e.Service, len(e.Candidates), strings.Join(slots, ", "))
}

// Resolver resolves targets against a Swarm manager.
type Resolver struct {
	cli      DockerClient
	addrMode AddrMode

	// Raft peer addresses by node ID, fetched once per Resolver and used to
	// recover the address of a node whose Status.Addr is unusable. See peerAddr.
	peersOnce sync.Once
	peers     map[string]string
}

// New builds a Resolver.
func New(cli DockerClient, mode AddrMode) *Resolver {
	return &Resolver{cli: cli, addrMode: mode}
}

var slotRe = regexp.MustCompile(`^(.+)\.(\d+)$`)

// Request describes what to resolve.
type Request struct {
	Target   string // raw target argument
	NodeHint string // --node override, for container-id targets
}

// Resolve maps a Request to an Endpoint. For a bare service name with multiple
// running tasks it returns *AmbiguousError so the caller can prompt or error.
func (r *Resolver) Resolve(ctx context.Context, req Request) (*Endpoint, error) {
	target := strings.TrimSpace(req.Target)
	if target == "" {
		return nil, fmt.Errorf("empty target")
	}

	// service.slot — only when the base name actually names a service.
	if m := slotRe.FindStringSubmatch(target); m != nil {
		if svc, err := r.findService(ctx, m[1]); err == nil && svc != nil {
			slot, _ := strconv.Atoi(m[2])
			return r.resolveServiceSlot(ctx, svc, slot)
		}
	}

	// bare service name
	if svc, err := r.findService(ctx, target); err == nil && svc != nil {
		return r.resolveService(ctx, svc)
	}

	// task ID
	if ep, ok, err := r.resolveTaskID(ctx, target); err != nil {
		return nil, err
	} else if ok {
		return ep, nil
	}

	// container ID — needs a node hint (manager can't always map id→node).
	return r.resolveContainerID(ctx, target, req.NodeHint)
}

func (r *Resolver) findService(ctx context.Context, name string) (*swarm.Service, error) {
	f := filters.NewArgs(filters.Arg("name", name))
	svcs, err := r.cli.ServiceList(ctx, types.ServiceListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	// The name filter is a substring match; require an exact ServiceName.
	for i := range svcs {
		if svcs[i].Spec.Name == name {
			return &svcs[i], nil
		}
	}
	return nil, nil
}

// serviceNames returns a map of service ID -> service name for all services.
func (r *Resolver) serviceNames(ctx context.Context) (map[string]string, error) {
	svcs, err := r.cli.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(svcs))
	for _, s := range svcs {
		m[s.ID] = s.Spec.Name
	}
	return m, nil
}

func (r *Resolver) runningTasks(ctx context.Context, serviceID string) ([]swarm.Task, error) {
	f := filters.NewArgs()
	if serviceID != "" {
		f.Add("service", serviceID)
	}
	f.Add("desired-state", "running")
	return r.cli.TaskList(ctx, types.TaskListOptions{Filters: f})
}

func (r *Resolver) resolveService(ctx context.Context, svc *swarm.Service) (*Endpoint, error) {
	cands, err := r.candidatesForService(ctx, svc)
	if err != nil {
		return nil, err
	}
	switch len(cands) {
	case 0:
		return nil, fmt.Errorf("no running task for service %q", svc.Spec.Name)
	case 1:
		return r.endpointFromCandidate(cands[0]), nil
	default:
		return nil, &AmbiguousError{Service: svc.Spec.Name, Candidates: cands}
	}
}

func (r *Resolver) resolveServiceSlot(ctx context.Context, svc *swarm.Service, slot int) (*Endpoint, error) {
	cands, err := r.candidatesForService(ctx, svc)
	if err != nil {
		return nil, err
	}
	var match []Candidate
	for _, c := range cands {
		if c.Slot == slot {
			match = append(match, c)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("no running task for %s.%d", svc.Spec.Name, slot)
	case 1:
		return r.endpointFromCandidate(match[0]), nil
	default:
		// Multiple running tasks in one slot (e.g. mid rolling-update); newest wins.
		sort.Slice(match, func(i, j int) bool { return match[i].Uptime < match[j].Uptime })
		return r.endpointFromCandidate(match[0]), nil
	}
}

func (r *Resolver) resolveTaskID(ctx context.Context, id string) (*Endpoint, bool, error) {
	f := filters.NewArgs(filters.Arg("id", id))
	tasks, err := r.cli.TaskList(ctx, types.TaskListOptions{Filters: f})
	if err != nil {
		return nil, false, err
	}
	if len(tasks) == 0 {
		return nil, false, nil
	}
	c, err := r.taskToCandidate(ctx, tasks[0])
	if err != nil {
		return nil, false, err
	}
	if c.ContainerID == "" {
		return nil, false, fmt.Errorf("task %s has no container yet", id)
	}
	return r.endpointFromCandidate(*c), true, nil
}

func (r *Resolver) resolveContainerID(ctx context.Context, id, nodeHint string) (*Endpoint, error) {
	if nodeHint == "" {
		// Best effort: scan running tasks for a matching container id.
		tasks, err := r.runningTasks(ctx, "")
		if err == nil {
			for _, t := range tasks {
				if cid := containerID(t); cid != "" && strings.HasPrefix(cid, id) {
					c, err := r.taskToCandidate(ctx, t)
					if err != nil {
						return nil, err
					}
					ep := r.endpointFromCandidate(*c)
					ep.ContainerID = cid
					return ep, nil
				}
			}
		}
		return nil, fmt.Errorf("cannot resolve node for container %s: pass --node <hostname|ip|nodeID>", id)
	}

	// With a hint, derive the dial host from the node if it resolves, else dial
	// the hint directly.
	host := nodeHint
	if node, _, err := r.cli.NodeInspectWithRaw(ctx, nodeHint); err == nil {
		host = r.dialHost(ctx, node)
	}
	return &Endpoint{DialHost: host, ContainerID: id}, nil
}

func (r *Resolver) candidatesForService(ctx context.Context, svc *swarm.Service) ([]Candidate, error) {
	tasks, err := r.runningTasks(ctx, svc.ID)
	if err != nil {
		return nil, err
	}
	var cands []Candidate
	for _, t := range tasks {
		if containerID(t) == "" {
			continue // task scheduled but container not up yet
		}
		c, err := r.taskToCandidate(ctx, t)
		if err != nil {
			return nil, err
		}
		c.Service = svc.Spec.Name
		cands = append(cands, *c)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Slot < cands[j].Slot })
	return cands, nil
}

// ErrTargetGone reports that the service a FollowTarget names no longer exists,
// so there is nothing left to follow (as opposed to a replacement that is still
// being scheduled). Callers use it to stop following instead of waiting.
var ErrTargetGone = errors.New("service no longer exists")

// FollowTarget identifies a logical replica to keep following across container
// replacements (rolling update, restart, reschedule). Slot pins a replicated
// service's slot; for a global service (Slot 0) NodeID pins the node. With
// neither, the newest running task of the service wins.
type FollowTarget struct {
	Service string
	Slot    int
	NodeID  string
}

// Successor returns the running container that currently satisfies a
// FollowTarget — the replacement after a swap, or the same container while it is
// still up. found is false with a nil error when the service exists but has no
// matching running task yet (the replacement is still scheduling); it returns
// ErrTargetGone when the service itself is gone.
func (r *Resolver) Successor(ctx context.Context, t FollowTarget) (Candidate, bool, error) {
	svc, err := r.findService(ctx, t.Service)
	if err != nil {
		return Candidate{}, false, err
	}
	if svc == nil {
		return Candidate{}, false, ErrTargetGone
	}
	cands, err := r.candidatesForService(ctx, svc)
	if err != nil {
		return Candidate{}, false, err
	}
	var best *Candidate
	for i := range cands {
		c := &cands[i]
		switch {
		case t.Slot > 0:
			if c.Slot != t.Slot {
				continue
			}
		case t.NodeID != "":
			if c.NodeID != t.NodeID {
				continue
			}
		}
		// Newest wins — during a rolling update a slot can briefly hold both the
		// old and the new task; the smaller uptime is the replacement.
		if best == nil || c.Uptime < best.Uptime {
			best = c
		}
	}
	if best == nil {
		return Candidate{}, false, nil
	}
	return *best, true, nil
}

// Candidates lists running tasks for `ps`. An empty service lists all services.
func (r *Resolver) Candidates(ctx context.Context, service string) ([]Candidate, error) {
	if service != "" {
		svc, err := r.findService(ctx, service)
		if err != nil {
			return nil, err
		}
		if svc == nil {
			return nil, fmt.Errorf("no such service %q", service)
		}
		return r.candidatesForService(ctx, svc)
	}
	tasks, err := r.runningTasks(ctx, "")
	if err != nil {
		return nil, err
	}
	// Map service IDs to names so each task shows its service (tasks only carry
	// the service ID, not the name).
	nameByID, err := r.serviceNames(ctx)
	if err != nil {
		return nil, err
	}
	var cands []Candidate
	for _, t := range tasks {
		if containerID(t) == "" {
			continue
		}
		c, err := r.taskToCandidate(ctx, t)
		if err != nil {
			return nil, err
		}
		c.Service = nameByID[t.ServiceID]
		cands = append(cands, *c)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Service != cands[j].Service {
			return cands[i].Service < cands[j].Service
		}
		return cands[i].Slot < cands[j].Slot
	})
	return cands, nil
}

// Services lists every swarm service with its running/desired task counts.
// It asks the manager for the ServiceStatus shortcut (Status: true) so it does
// not have to list tasks per service; if the manager does not populate it, it
// falls back to counting tasks. Results are sorted by name.
func (r *Resolver) Services(ctx context.Context) ([]Service, error) {
	svcs, err := r.cli.ServiceList(ctx, types.ServiceListOptions{Status: true})
	if err != nil {
		return nil, err
	}
	out := make([]Service, 0, len(svcs))
	for _, s := range svcs {
		svc := Service{
			Name:     s.Spec.Name,
			Global:   s.Spec.Mode.Global != nil,
			Mode:     serviceMode(s.Spec.Mode),
			Image:    serviceImage(s.Spec.TaskTemplate.ContainerSpec),
			ImageRef: serviceImageRef(s.Spec.TaskTemplate.ContainerSpec),
			Ports:    servicePorts(servicePortConfigs(s)),
			Stack:    s.Spec.Labels[stackNamespaceLabel],
		}
		if st := s.ServiceStatus; st != nil {
			svc.Running = int(st.RunningTasks)
			svc.Desired = int(st.DesiredTasks)
		} else {
			svc.Running, svc.Desired = r.serviceCounts(ctx, s)
		}
		if us := s.UpdateStatus; us != nil {
			svc.UpdateState = string(us.State)
		}
		svc.Risks = secscan.Scan(s)
		out = append(out, svc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// serviceCounts computes running/desired task counts when the manager did not
// return a ServiceStatus. Desired is the replica count for a replicated
// service, or the number of not-shutdown tasks for a global one; running counts
// tasks actually in the running state with a container.
func (r *Resolver) serviceCounts(ctx context.Context, s swarm.Service) (running, desired int) {
	if rep := s.Spec.Mode.Replicated; rep != nil && rep.Replicas != nil {
		desired = int(*rep.Replicas)
	}
	tasks, err := r.cli.TaskList(ctx, types.TaskListOptions{
		Filters: filters.NewArgs(filters.Arg("service", s.ID)),
	})
	if err != nil {
		return running, desired
	}
	global := s.Spec.Mode.Global != nil
	for _, t := range tasks {
		if global && t.DesiredState != swarm.TaskStateShutdown {
			desired++
		}
		if t.Status.State == swarm.TaskStateRunning && containerID(t) != "" {
			running++
		}
	}
	return running, desired
}

// serviceMode maps a swarm service mode to the docker service ls MODE string.
func serviceMode(m swarm.ServiceMode) string {
	switch {
	case m.Global != nil:
		return "global"
	case m.GlobalJob != nil:
		return "global-job"
	case m.ReplicatedJob != nil:
		return "replicated-job"
	default:
		return "replicated"
	}
}

// serviceImage returns the service's container image with any @sha256 digest
// stripped, matching how docker service ls renders the IMAGE column.
func serviceImage(cs *swarm.ContainerSpec) string {
	if cs == nil {
		return ""
	}
	img := cs.Image
	if i := strings.IndexByte(img, '@'); i >= 0 {
		img = img[:i]
	}
	return img
}

// serviceImageRef returns the service's container image exactly as pinned in the
// spec — keeping any @sha256 digest (what the tasks actually run), so callers can
// resolve a :latest tag back to its real version.
func serviceImageRef(cs *swarm.ContainerSpec) string {
	if cs == nil {
		return ""
	}
	return cs.Image
}

// servicePortConfigs returns the service's published ports, preferring the
// realized endpoint and falling back to the spec.
func servicePortConfigs(s swarm.Service) []swarm.PortConfig {
	if len(s.Endpoint.Ports) > 0 {
		return s.Endpoint.Ports
	}
	if s.Spec.EndpointSpec != nil {
		return s.Spec.EndpointSpec.Ports
	}
	return nil
}

// servicePorts formats published ports like docker service ls, e.g.
// "*:8080->80/tcp" for ingress and "8080->80/tcp" for host mode.
func servicePorts(ports []swarm.PortConfig) string {
	var out []string
	for _, p := range ports {
		if p.PublishedPort == 0 {
			continue
		}
		proto := string(p.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		prefix := ""
		if p.PublishMode == swarm.PortConfigPublishModeIngress || p.PublishMode == "" {
			prefix = "*:"
		}
		out = append(out, fmt.Sprintf("%s%d->%d/%s", prefix, p.PublishedPort, p.TargetPort, proto))
	}
	return strings.Join(out, ", ")
}

func (r *Resolver) taskToCandidate(ctx context.Context, t swarm.Task) (*Candidate, error) {
	node, _, err := r.cli.NodeInspectWithRaw(ctx, t.NodeID)
	if err != nil {
		return nil, fmt.Errorf("inspect node %s: %w", t.NodeID, err)
	}
	c := &Candidate{
		Slot:        t.Slot,
		TaskID:      t.ID,
		NodeID:      t.NodeID,
		NodeName:    node.Description.Hostname,
		NodeAddr:    node.Status.Addr,
		DialHost:    r.dialHost(ctx, node),
		ContainerID: containerID(t),
	}
	if !t.Status.Timestamp.IsZero() {
		c.Uptime = time.Since(t.Status.Timestamp)
	}
	return c, nil
}

func (r *Resolver) endpointFromCandidate(c Candidate) *Endpoint {
	return &Endpoint{
		DialHost:    c.DialHost,
		ContainerID: c.ContainerID,
		NodeID:      c.NodeID,
		NodeName:    c.NodeName,
	}
}

func (r *Resolver) dialHost(ctx context.Context, node swarm.Node) string {
	addr := node.Status.Addr
	host := node.Description.Hostname
	switch r.addrMode {
	case AddrIP:
		if usableAddr(addr) {
			return addr
		}
		// The leader reports Status.Addr "0.0.0.0". Swarm fills that field from
		// the remote address it observes for a node's agent session, and the
		// leader has no such remote connection to itself — so this is normal
		// reporting, not a misconfigured --advertise-addr, and it moves to
		// whichever node wins the next election. The node's real address is
		// still in the raft peer list, so prefer that over degrading to the
		// hostname: a hostname the operator cannot resolve is exactly what
		// -addr-mode ip was chosen to avoid.
		if p := r.peerAddr(ctx, node.ID); p != "" {
			return p
		}
		return host
	default:
		if host != "" {
			return host
		}
		if usableAddr(addr) {
			return addr
		}
		if p := r.peerAddr(ctx, node.ID); p != "" {
			return p
		}
		return addr
	}
}

// peerAddr returns the node's host address as reported by the swarm raft peer
// list, or "" when it is not listed or the lookup failed. Only managers appear
// there — which covers every node that can report "0.0.0.0", since that is a
// property of the leader and leaders are always managers.
//
// The list is fetched at most once per Resolver: it costs one Info call, and a
// Resolver is short-lived (one command), so this never serves a stale address
// across invocations.
func (r *Resolver) peerAddr(ctx context.Context, nodeID string) string {
	r.peersOnce.Do(func() {
		info, err := r.cli.Info(ctx)
		if err != nil {
			return // leaves r.peers nil; lookups below return ""
		}
		r.peers = make(map[string]string, len(info.Swarm.RemoteManagers))
		for _, p := range info.Swarm.RemoteManagers {
			if h := hostOnly(p.Addr); usableAddr(h) {
				r.peers[p.NodeID] = h
			}
		}
	})
	return r.peers[nodeID]
}

// hostOnly strips the port from a peer address ("10.0.1.5:2377" -> "10.0.1.5",
// "[fd00::5]:2377" -> "fd00::5"), tolerating an address that carries no port.
func hostOnly(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// usableAddr reports whether addr is a routable dial target. The Swarm
// wildcard/empty advertise addresses route to the local host, not the node.
func usableAddr(addr string) bool {
	switch strings.TrimSpace(addr) {
	case "", "0.0.0.0", "::", "[::]":
		return false
	}
	return true
}

// containerID extracts the full container ID from a task, tolerating the nil
// ContainerStatus of a task whose container is not up yet.
func containerID(t swarm.Task) string {
	if t.Status.ContainerStatus == nil {
		return ""
	}
	return t.Status.ContainerStatus.ContainerID
}
