// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/fanout"
	"swarmexec/internal/pb"
)

// Endpoint bookkeeping compares two views of an overlay network: the
// manager's (which tasks run, and the address each was given) and every
// node's (the endpoints its daemon has attached locally, and its networkdb
// copy of the whole network's endpoints). A node that missed a departure keeps
// the departed endpoint's IP and MAC; the manager has long forgotten it. Those
// leftovers are what AnalyzeEndpoints calls stale candidates.
//
// It is a heuristic, and says so wherever it is shown:
//   - false positives: a task that started or stopped between the two reads
//     (refresh to confirm); a standalone container on an attachable network
//     whose name looks like a task's.
//   - false negatives: a leftover whose IP has since been handed to a new
//     task (the address matches, so it looks alive — the duplicate check may
//     still catch it); leftovers the daemon's networkdb has already dropped
//     while the kernel's FDB still holds them (invisible to the Docker API).

// TaskAddr is one manager task's addresses on one network.
type TaskAddr struct {
	NetworkID string
	NodeID    string
	Service   string // service name ("" if unknown)
	IPs       []string
	// Running: the task is running (counts as a container expected on its
	// node). A task still starting is not running, but its addresses are
	// already allocated and its endpoint may exist, so they count as live.
	Running bool
}

// TaskLister is the manager API slice NetworkTaskAddrs needs.
type TaskLister interface {
	TaskList(ctx context.Context, opts client.TaskListOptions) (client.TaskListResult, error)
	ServiceList(ctx context.Context, opts client.ServiceListOptions) (client.ServiceListResult, error)
}

// NetworkTaskAddrs lists the addresses of every live task on every network,
// from one TaskList (and one ServiceList for the names).
func NetworkTaskAddrs(ctx context.Context, dcli TaskLister) ([]TaskAddr, error) {
	res, err := dcli.TaskList(ctx, client.TaskListOptions{})
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	if svcs, err := dcli.ServiceList(ctx, client.ServiceListOptions{}); err == nil {
		for _, s := range svcs.Items {
			names[s.ID] = s.Spec.Name
		}
	}
	return taskAddrs(res.Items, names), nil
}

// liveTaskState: the task holds its addresses (allocated up to running).
func liveTaskState(s swarm.TaskState) bool {
	switch s {
	case swarm.TaskStateNew, swarm.TaskStateAllocated, swarm.TaskStatePending, swarm.TaskStateAssigned,
		swarm.TaskStateAccepted, swarm.TaskStatePreparing, swarm.TaskStateReady, swarm.TaskStateStarting,
		swarm.TaskStateRunning:
		return true
	}
	return false
}

// taskAddrs is the pure core of NetworkTaskAddrs.
func taskAddrs(tasks []swarm.Task, svcNames map[string]string) []TaskAddr {
	var out []TaskAddr
	for _, t := range tasks {
		if !liveTaskState(t.Status.State) {
			continue
		}
		for _, a := range t.NetworksAttachments {
			if a.Network.ID == "" {
				continue
			}
			ta := TaskAddr{NetworkID: a.Network.ID, NodeID: t.NodeID, Service: svcNames[t.ServiceID],
				Running: t.Status.State == swarm.TaskStateRunning}
			for _, p := range a.Addresses {
				ta.IPs = append(ta.IPs, p.Addr().String())
			}
			out = append(out, ta)
		}
	}
	return out
}

// NodeEndpointState is how far one node could tell us about a network.
type NodeEndpointState int

const (
	NodeEndpointsOK  NodeEndpointState = iota // answered, network joined
	NodeNotOnNetwork                          // answered, network not joined here
	NodeNetworkError                          // answered, but this network could not be inspected
	NodeAgentTooOld                           // agent predates NetworkEndpoints
	NodeNotPermitted                          // the agent policy denies network.inspect
	NodeUnreachable                           // agent error (dial, TLS, Docker)
	NodeNotReached                            // the fan-out never got to it
)

// String is the state as shown to an operator. The too-old hint is the
// image update (no service spec change is needed for an RPC).
func (s NodeEndpointState) String() string {
	switch s {
	case NodeEndpointsOK:
		return "ok"
	case NodeNotOnNetwork:
		return "not on this network"
	case NodeNetworkError:
		return "network inspect failed"
	case NodeAgentTooOld:
		return "agent too old — update the agent image"
	case NodeNotPermitted:
		return "not permitted (agent policy: network.inspect)"
	case NodeUnreachable:
		return "agent unreachable"
	case NodeNotReached:
		return "not reached"
	}
	return "unknown"
}

// Known reports whether the node's view of the network is known (answered
// at all — joined or not).
func (s NodeEndpointState) Known() bool { return s == NodeEndpointsOK || s == NodeNotOnNetwork }

// NodeEndpoints is one node's numbers for one network.
type NodeEndpoints struct {
	NodeID string
	Node   string // hostname
	State  NodeEndpointState
	Err    string // the agent's or network's error, for NodeUnreachable / NodeNetworkError

	Expected   int // running tasks the manager places on this node
	Containers int // local endpoints that are not the load balancer
	LB         int // local load-balancer endpoints
	MACs       int // distinct MAC addresses among the local endpoints
	Known      int // task endpoints in this node's networkdb view (whole network, LB incl.)
	Stale      int // stale candidates in this node's view
}

// StaleEndpoint is an endpoint some node still holds whose address no live
// task has. The same leftover usually shows in several nodes' networkdb
// copies; it is listed once, with SeenBy naming them.
type StaleEndpoint struct {
	IP         string
	MAC        string // known only when a node still has it attached locally
	Name       string // task container name, or the local container's name
	Service    string // the networkdb service it is filed under ("" unknown)
	EndpointID string
	// Owner is the node the endpoint belongs to, by hostname when it can be
	// matched to a node, else the host IP the daemon reported (OwnerIP).
	Owner   string
	OwnerIP string
	SeenBy  []string // nodes whose view holds it, sorted
	Local   bool     // still attached on its node (a local endpoint, not just a networkdb entry)
	Orphan  bool     // a local endpoint with no container ("ep-…")
}

// Duplicate is an IP or MAC held by more than one endpoint.
type Duplicate struct {
	Addr      string
	Endpoints []string // endpoint names (or IDs when unnamed), sorted
}

// NetEndpoints is one network's endpoint analysis across all nodes.
type NetEndpoints struct {
	NetworkID string
	Running   int // the manager's running tasks on the network
	Addrs     int // distinct task endpoint IPs the nodes know (LB excluded)
	LB        int // distinct load-balancer IPs
	Nodes     []NodeEndpoints
	Stale     []StaleEndpoint
	DupIPs    []Duplicate
	DupMACs   []Duplicate

	Answered int // nodes whose view is known
	Unknown  int // nodes that could not answer (any reason)
	TooOld   int // of those, agents too old for the RPC
}

// Warn reports something worth a marker: stale candidates or duplicates.
func (e *NetEndpoints) Warn() bool {
	return e != nil && (len(e.Stale) > 0 || len(e.DupIPs) > 0 || len(e.DupMACs) > 0)
}

// StaleRemedy is the fix, worded as what usually works, not a promise.
const StaleRemedy = "stale entries usually clear once the leftover endpoint is removed — " +
	"`docker network disconnect -f <network> <endpoint name>` on a node that lists it — " +
	"or after restarting Docker on the node that held it; refresh first, a task that just started or stopped shows here for a moment"

// AnalyzeEndpoints compares the manager's task addresses with each node's
// endpoint view, per network. Networks are those the manager has live tasks on
// plus those any node reported; the result is keyed by network ID.
func AnalyzeEndpoints(tasks []TaskAddr, nodes []agentcall.NodeNetworkEndpoints) map[string]*NetEndpoints {
	ids := map[string]bool{}
	for _, t := range tasks {
		ids[t.NetworkID] = true
	}
	for _, n := range nodes {
		if n.Err == nil && n.Resp != nil {
			for _, ni := range n.Resp.GetNetworks() {
				if ni.GetId() != "" {
					ids[ni.GetId()] = true
				}
			}
		}
	}
	owners := newOwnerIndex(nodes)
	out := make(map[string]*NetEndpoints, len(ids))
	for id := range ids {
		out[id] = analyzeNetwork(id, tasks, nodes, owners)
	}
	return out
}

// nodeState classifies a node-level error.
func nodeState(err error) NodeEndpointState {
	switch {
	case errors.Is(err, fanout.ErrSkipped):
		return NodeNotReached
	case agenterr.TooOld(err):
		return NodeAgentTooOld
	case status.Code(err) == codes.PermissionDenied:
		return NodeNotPermitted
	}
	return NodeUnreachable
}

// netFind is network id in a node's response, or nil.
func netFind(resp *pb.NetworkEndpointsResponse, id string) *pb.NetworkEndpointsInfo {
	for _, ni := range resp.GetNetworks() {
		if ni.GetId() == id {
			return ni
		}
	}
	return nil
}

// endpointKey identifies an endpoint across views: its ID, else ip+name.
func endpointKey(id, ip, name string) string {
	if id != "" {
		return id
	}
	return ip + "|" + name
}

// lbTaskName: the daemon names a load-balancer endpoint "<network>-endpoint".
func lbTaskName(name string) bool { return strings.HasSuffix(name, "-endpoint") }

func analyzeNetwork(id string, tasks []TaskAddr, nodes []agentcall.NodeNetworkEndpoints, owners ownerIndex) *NetEndpoints {
	e := &NetEndpoints{NetworkID: id}
	live := map[string]bool{}
	expected := map[string]int{}
	for _, t := range tasks {
		if t.NetworkID != id {
			continue
		}
		for _, ip := range t.IPs {
			live[ip] = true
		}
		if t.Running {
			e.Running++
			expected[t.NodeID]++
		}
	}
	vips := map[string]bool{}
	for _, n := range nodes {
		if ni := netFind(n.Resp, id); n.Err == nil && ni != nil {
			for _, s := range ni.GetServices() {
				if v := StripMask(s.GetVip()); v != "" {
					vips[v] = true
				}
			}
		}
	}

	stale := map[string]*StaleEndpoint{}
	ipOwners := map[string]map[string]string{}  // ip -> endpoint key -> name
	macOwners := map[string]map[string]string{} // mac -> endpoint key -> name
	taskIPs, lbIPs := map[string]bool{}, map[string]bool{}
	note := func(m map[string]map[string]string, addr, key, name string) {
		if addr == "" {
			return
		}
		if m[addr] == nil {
			m[addr] = map[string]string{}
		}
		if name == "" {
			name = shortID(key)
		}
		m[addr][key] = name
	}
	addStale := func(key string, s StaleEndpoint, seenBy string) {
		cur := stale[key]
		if cur == nil {
			cur = &StaleEndpoint{IP: s.IP, Name: s.Name, Service: s.Service, EndpointID: s.EndpointID}
			stale[key] = cur
		}
		if s.MAC != "" {
			cur.MAC = s.MAC
		}
		if cur.Name == "" {
			cur.Name = s.Name
		}
		if s.OwnerIP != "" && cur.OwnerIP == "" {
			cur.OwnerIP = s.OwnerIP
		}
		if s.Owner != "" {
			cur.Owner = s.Owner
		}
		cur.Local = cur.Local || s.Local
		cur.Orphan = cur.Orphan || s.Orphan
		if !contains(cur.SeenBy, seenBy) {
			cur.SeenBy = append(cur.SeenBy, seenBy)
		}
	}

	for _, n := range nodes {
		ne := NodeEndpoints{NodeID: n.Node.ID, Node: n.Node.Name, Expected: expected[n.Node.ID]}
		if n.Err != nil {
			ne.State, ne.Err = nodeState(n.Err), n.Err.Error()
			e.Unknown++
			if ne.State == NodeAgentTooOld {
				e.TooOld++
			}
			e.Nodes = append(e.Nodes, ne)
			continue
		}
		ni := netFind(n.Resp, id)
		switch {
		case ni == nil:
			ne.State = NodeNotOnNetwork
			e.Answered++
			e.Nodes = append(e.Nodes, ne)
			continue
		case ni.GetError() != "":
			ne.State, ne.Err = NodeNetworkError, ni.GetError()
			e.Unknown++
			e.Nodes = append(e.Nodes, ne)
			continue
		}
		e.Answered++
		ne.State = NodeEndpointsOK
		peerName := map[string]string{}
		for _, p := range ni.GetPeers() {
			peerName[p.GetIp()] = p.GetName()
		}
		macs, nodeStale := map[string]bool{}, map[string]bool{}
		for _, l := range ni.GetLocalEndpoints() {
			ip := StripMask(l.GetIpv4Address())
			key := endpointKey(l.GetEndpointId(), ip, l.GetName())
			if mac := strings.ToLower(l.GetMacAddress()); mac != "" {
				macs[mac] = true
				note(macOwners, mac, key, l.GetName())
			}
			note(ipOwners, ip, key, l.GetName())
			if l.GetLoadBalancer() {
				ne.LB++
				if ip != "" {
					lbIPs[ip] = true
				}
				continue
			}
			ne.Containers++
			if ip == "" {
				continue
			}
			taskIPs[ip] = true
			orphan := strings.HasPrefix(l.GetContainerId(), "ep-")
			if live[ip] || vips[ip] || (!orphan && !taskContainerName(l.GetName())) {
				// Alive, or a standalone container on an attachable network
				// (the manager knows only tasks).
				continue
			}
			nodeStale[key] = true
			addStale(key, StaleEndpoint{IP: ip, MAC: strings.ToLower(l.GetMacAddress()), Name: l.GetName(),
				EndpointID: l.GetEndpointId(), Owner: n.Node.Name, Local: true, Orphan: orphan}, n.Node.Name)
		}
		ne.MACs = len(macs)
		for _, s := range ni.GetServices() {
			for _, t := range s.GetTasks() {
				ne.Known++
				ip := StripMask(t.GetEndpointIp())
				key := endpointKey(t.GetEndpointId(), ip, t.GetName())
				note(ipOwners, ip, key, t.GetName())
				if s.GetName() == "" {
					// The daemon files load balancers (and standalone
					// containers) under the empty service: never stale here.
					if lbTaskName(t.GetName()) && ip != "" {
						lbIPs[ip] = true
					}
					continue
				}
				if ip == "" {
					continue
				}
				taskIPs[ip] = true
				if live[ip] || vips[ip] {
					continue
				}
				nodeStale[key] = true
				ownerIP := t.GetHostIp()
				addStale(key, StaleEndpoint{IP: ip, Name: t.GetName(), Service: s.GetName(), EndpointID: t.GetEndpointId(),
					OwnerIP: ownerIP, Owner: owners.name(ownerIP, peerName[ownerIP])}, n.Node.Name)
			}
		}
		ne.Stale = len(nodeStale)
		e.Nodes = append(e.Nodes, ne)
	}
	for ip := range lbIPs {
		delete(taskIPs, ip)
	}
	e.Addrs, e.LB = len(taskIPs), len(lbIPs)
	for _, s := range stale {
		sort.Strings(s.SeenBy)
		if s.Owner == "" {
			s.Owner = s.OwnerIP
		}
		e.Stale = append(e.Stale, *s)
	}
	sort.Slice(e.Stale, func(i, j int) bool { return lessIP(e.Stale[i].IP, e.Stale[j].IP, e.Stale[i].Name, e.Stale[j].Name) })
	e.DupIPs, e.DupMACs = duplicates(ipOwners), duplicates(macOwners)
	return e
}

// duplicates are the addresses held by more than one endpoint.
func duplicates(m map[string]map[string]string) []Duplicate {
	var out []Duplicate
	for addr, eps := range m {
		if len(eps) < 2 {
			continue
		}
		d := Duplicate{Addr: addr}
		for _, name := range eps {
			d.Endpoints = append(d.Endpoints, name)
		}
		sort.Strings(d.Endpoints)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return lessIP(out[i].Addr, out[j].Addr, "", "") })
	return out
}

// lessIP orders dotted addresses numerically where it can (10.0.1.9 before
// 10.0.1.10), else as text; ties break on the names.
func lessIP(a, b, an, bn string) bool {
	if a != b {
		pa, pb := strings.Split(a, "."), strings.Split(b, ".")
		if len(pa) == 4 && len(pb) == 4 {
			for i := range pa {
				if len(pa[i]) != len(pb[i]) {
					return len(pa[i]) < len(pb[i])
				}
				if pa[i] != pb[i] {
					return pa[i] < pb[i]
				}
			}
		}
		return a < b
	}
	return an < bn
}

// taskContainerName: a swarm task's container is named
// "<service>.<slot|node id>.<task id>", the task id 25 lowercase alphanumerics.
func taskContainerName(name string) bool {
	name = strings.TrimPrefix(name, "/")
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || !strings.Contains(name[:i], ".") {
		return false
	}
	id := name[i+1:]
	if len(id) != 25 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// TaskServiceName is the service a task container's name belongs to
// ("web" for "web.1.<task id>"), "" when the name is not a task's.
func TaskServiceName(name string) string {
	if !taskContainerName(name) {
		return ""
	}
	name = strings.TrimPrefix(name, "/")
	name = name[:strings.LastIndexByte(name, '.')]
	return name[:strings.LastIndexByte(name, '.')]
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ownerIndex maps an endpoint's host IP to a node hostname: through the
// node's dial address, or through the networkdb peer name, which is a prefix
// of the node ID.
type ownerIndex struct {
	byAddr map[string]string
	ids    map[string]string // node id -> hostname
}

func newOwnerIndex(nodes []agentcall.NodeNetworkEndpoints) ownerIndex {
	o := ownerIndex{byAddr: map[string]string{}, ids: map[string]string{}}
	for _, n := range nodes {
		if n.Node.DialHost != "" {
			o.byAddr[n.Node.DialHost] = n.Node.Name
		}
		if n.Node.ID != "" {
			o.ids[n.Node.ID] = n.Node.Name
		}
	}
	return o
}

// name is the hostname for hostIP, "" when it cannot be matched.
func (o ownerIndex) name(hostIP, peer string) string {
	if hostIP == "" {
		return ""
	}
	if h := o.byAddr[hostIP]; h != "" {
		return h
	}
	if peer != "" {
		for id, h := range o.ids {
			if strings.HasPrefix(id, peer) || peer == h {
				return h
			}
		}
	}
	return ""
}
