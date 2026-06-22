// Package resolve turns an operator-supplied target (service, service.slot,
// task ID, or container ID) into a concrete dial endpoint: the node address to
// connect the agent on, plus the full container ID to exec into. It talks to
// the Swarm manager via the Docker SDK, behind the DockerClient interface so the
// logic is unit-testable with a fake. See REQUIREMENTS §4.
package resolve

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
)

// DockerClient is the subset of the Docker SDK the resolver needs. The real
// *client.Client satisfies it; tests supply a fake.
type DockerClient interface {
	ServiceList(ctx context.Context, options types.ServiceListOptions) ([]swarm.Service, error)
	TaskList(ctx context.Context, options types.TaskListOptions) ([]swarm.Task, error)
	NodeInspectWithRaw(ctx context.Context, nodeID string) (swarm.Node, []byte, error)
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
		host = r.dialHost(node)
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
		DialHost:    r.dialHost(node),
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

func (r *Resolver) dialHost(node swarm.Node) string {
	switch r.addrMode {
	case AddrIP:
		if node.Status.Addr != "" {
			return node.Status.Addr
		}
		return node.Description.Hostname
	default:
		if node.Description.Hostname != "" {
			return node.Description.Hostname
		}
		return node.Status.Addr
	}
}

// containerID extracts the full container ID from a task, tolerating the nil
// ContainerStatus of a task whose container is not up yet.
func containerID(t swarm.Task) string {
	if t.Status.ContainerStatus == nil {
		return ""
	}
	return t.Status.ContainerStatus.ContainerID
}
