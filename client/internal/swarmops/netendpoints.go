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
	"google.golang.org/protobuf/proto"

	"swarmexec/client/internal/agentcall"
	"swarmexec/client/internal/agenterr"
	"swarmexec/client/internal/fanout"
	"swarmexec/internal/dockerfmt"
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

// analyzeNetwork is one network's analysis: the manager's live addresses,
// then every node's view scanned against them, then the totals.
func analyzeNetwork(id string, tasks []TaskAddr, nodes []agentcall.NodeNetworkEndpoints, owners ownerIndex) *NetEndpoints {
	live := collectLive(id, tasks)
	s := newNetScan(id, live, collectVIPs(id, nodes), owners)
	e := &NetEndpoints{NetworkID: id, Running: live.running}
	for _, n := range nodes {
		e.Nodes = append(e.Nodes, s.scanNode(n))
	}
	e.Answered, e.Unknown, e.TooOld = tallyStates(e.Nodes)
	e.Addrs, e.LB = s.addrCounts()
	e.Stale = s.stale.sorted()
	e.DupIPs, e.DupMACs = duplicates(s.ipOwners), duplicates(s.macOwners)
	return e
}

// liveTasks is the manager's side of one network.
type liveTasks struct {
	ips      map[string]bool // addresses held by live tasks
	expected map[string]int  // running tasks per node ID
	running  int             // running tasks in all
}

// collectLive gathers the live task addresses and running counts on network id.
func collectLive(id string, tasks []TaskAddr) liveTasks {
	l := liveTasks{ips: map[string]bool{}, expected: map[string]int{}}
	for _, t := range tasks {
		if t.NetworkID != id {
			continue
		}
		for _, ip := range t.IPs {
			l.ips[ip] = true
		}
		if t.Running {
			l.running++
			l.expected[t.NodeID]++
		}
	}
	return l
}

// collectVIPs is every service VIP on network id that an answering node
// reports.
func collectVIPs(id string, nodes []agentcall.NodeNetworkEndpoints) map[string]bool {
	vips := map[string]bool{}
	for _, n := range nodes {
		ni := netFind(n.Resp, id)
		if n.Err != nil || ni == nil {
			continue
		}
		for _, s := range ni.GetServices() {
			if v := StripMask(s.GetVip()); v != "" {
				vips[v] = true
			}
		}
	}
	return vips
}

// tallyStates counts the nodes whose view is known, the rest, and of the
// rest the agents too old for the RPC.
func tallyStates(nodes []NodeEndpoints) (answered, unknown, tooOld int) {
	for _, n := range nodes {
		switch {
		case n.State.Known():
			answered++
		case n.State == NodeAgentTooOld:
			unknown++
			tooOld++
		default:
			unknown++
		}
	}
	return answered, unknown, tooOld
}

// addrBook records, per address (IP or MAC), the endpoints holding it:
// endpoint key -> display name.
type addrBook map[string]map[string]string

// note records that endpoint key (named name) holds addr; an empty address
// is ignored, an unnamed endpoint is shown by its short key.
func (b addrBook) note(addr, key, name string) {
	if addr == "" {
		return
	}
	if b[addr] == nil {
		b[addr] = map[string]string{}
	}
	if name == "" {
		name = dockerfmt.ShortID(key)
	}
	b[addr][key] = name
}

// staleSet merges the stale candidates the nodes report, by endpoint key.
type staleSet map[string]*StaleEndpoint

// add merges one node's sighting of a stale endpoint: the first sighting
// fixes IP, service and endpoint ID; later ones fill in what was missing
// (a later owner or MAC wins).
func (m staleSet) add(key string, s StaleEndpoint, seenBy string) {
	cur := m[key]
	if cur == nil {
		cur = &StaleEndpoint{IP: s.IP, Name: s.Name, Service: s.Service, EndpointID: s.EndpointID}
		m[key] = cur
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

// sorted is the merged list ordered by IP (then name), each with SeenBy
// sorted and the owner falling back to the host IP; nil when empty.
func (m staleSet) sorted() []StaleEndpoint {
	var out []StaleEndpoint
	for _, s := range m {
		sort.Strings(s.SeenBy)
		if s.Owner == "" {
			s.Owner = s.OwnerIP
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return lessIP(out[i].IP, out[j].IP, out[i].Name, out[j].Name) })
	return out
}

// netScan accumulates one network's state while the nodes are scanned.
type netScan struct {
	id     string
	live   liveTasks
	vips   map[string]bool
	owners ownerIndex

	stale               staleSet
	ipOwners, macOwners addrBook
	taskIPs, lbIPs      map[string]bool // distinct task / load-balancer IPs seen
}

func newNetScan(id string, live liveTasks, vips map[string]bool, owners ownerIndex) *netScan {
	return &netScan{id: id, live: live, vips: vips, owners: owners, stale: staleSet{},
		ipOwners: addrBook{}, macOwners: addrBook{}, taskIPs: map[string]bool{}, lbIPs: map[string]bool{}}
}

// alive: a live task or a service VIP holds ip.
func (s *netScan) alive(ip string) bool { return s.live.ips[ip] || s.vips[ip] }

func (s *netScan) addLB(ip string) {
	if ip != "" {
		s.lbIPs[ip] = true
	}
}

// addrCounts is the number of distinct task endpoint IPs (load-balancer
// addresses excluded) and of distinct load-balancer IPs.
func (s *netScan) addrCounts() (tasks, lbs int) {
	tasks = len(s.taskIPs)
	for ip := range s.lbIPs {
		if s.taskIPs[ip] {
			tasks--
		}
	}
	return tasks, len(s.lbIPs)
}

// scanNode is one node's numbers for the network; the node's endpoints are
// recorded in the scan along the way.
func (s *netScan) scanNode(n agentcall.NodeNetworkEndpoints) NodeEndpoints {
	ne := NodeEndpoints{NodeID: n.Node.ID, Node: n.Node.Name, Expected: s.live.expected[n.Node.ID]}
	if n.Err != nil {
		ne.State, ne.Err = nodeState(n.Err), n.Err.Error()
		return ne
	}
	ni := netFind(n.Resp, s.id)
	switch {
	case ni == nil:
		ne.State = NodeNotOnNetwork
		return ne
	case ni.GetError() != "":
		ne.State, ne.Err = NodeNetworkError, ni.GetError()
		return ne
	}
	ne.State = NodeEndpointsOK
	nodeStale := map[string]bool{}
	s.scanLocal(&ne, ni, n.Node.Name, nodeStale)
	s.scanNetworkDB(&ne, ni, n.Node.Name, nodeStale)
	ne.Stale = len(nodeStale)
	return ne
}

// scanLocal counts the endpoints attached on the node itself and records
// the stale ones (their keys into nodeStale).
func (s *netScan) scanLocal(ne *NodeEndpoints, ni *pb.NetworkEndpointsInfo, node string, nodeStale map[string]bool) {
	macs := map[string]bool{}
	for _, l := range ni.GetLocalEndpoints() {
		ip := StripMask(l.GetIpv4Address())
		key := endpointKey(l.GetEndpointId(), ip, l.GetName())
		mac := strings.ToLower(l.GetMacAddress())
		if mac != "" {
			macs[mac] = true
			s.macOwners.note(mac, key, l.GetName())
		}
		s.ipOwners.note(ip, key, l.GetName())
		if l.GetLoadBalancer() {
			ne.LB++
			s.addLB(ip)
			continue
		}
		ne.Containers++
		if ip == "" {
			continue
		}
		s.taskIPs[ip] = true
		orphan := strings.HasPrefix(l.GetContainerId(), "ep-")
		if s.alive(ip) || (!orphan && !taskContainerName(l.GetName())) {
			// Alive, or a standalone container on an attachable network
			// (the manager knows only tasks).
			continue
		}
		nodeStale[key] = true
		s.stale.add(key, StaleEndpoint{IP: ip, MAC: mac, Name: l.GetName(),
			EndpointID: l.GetEndpointId(), Owner: node, Local: true, Orphan: orphan}, node)
	}
	ne.MACs = len(macs)
}

// scanNetworkDB walks the node's networkdb copy of the whole network and
// records the stale task entries (their keys into nodeStale).
func (s *netScan) scanNetworkDB(ne *NodeEndpoints, ni *pb.NetworkEndpointsInfo, node string, nodeStale map[string]bool) {
	peerName := map[string]string{}
	for _, p := range ni.GetPeers() {
		peerName[p.GetIp()] = p.GetName()
	}
	for _, svc := range ni.GetServices() {
		for _, t := range svc.GetTasks() {
			ne.Known++
			if key, stale := s.dbTask(svc.GetName(), t, peerName, node); stale {
				nodeStale[key] = true
			}
		}
	}
}

// dbTask records one networkdb task entry filed under service, as seen by
// node; it returns the entry's key and whether it is a stale candidate.
func (s *netScan) dbTask(service string, t *pb.NetworkTaskEndpoint, peerName map[string]string, node string) (string, bool) {
	ip := StripMask(t.GetEndpointIp())
	key := endpointKey(t.GetEndpointId(), ip, t.GetName())
	s.ipOwners.note(ip, key, t.GetName())
	if service == "" {
		// The daemon files load balancers (and standalone containers)
		// under the empty service: never stale here.
		if lbTaskName(t.GetName()) {
			s.addLB(ip)
		}
		return key, false
	}
	if ip == "" {
		return key, false
	}
	s.taskIPs[ip] = true
	if s.alive(ip) {
		return key, false
	}
	ownerIP := t.GetHostIp()
	s.stale.add(key, StaleEndpoint{IP: ip, Name: t.GetName(), Service: service, EndpointID: t.GetEndpointId(),
		OwnerIP: ownerIP, Owner: s.owners.name(ownerIP, peerName[ownerIP])}, node)
	return key, true
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

// EndpointScope narrows the endpoint analysis to the part of the cluster a
// restricted caller may see (the MCP server); the TUI analyses everything.
// The analysis itself stays the same: ScopeEndpoints filters its inputs, so a
// hidden service is as absent from the counts, the stale candidates and the
// duplicates as if it did not run at all.
type EndpointScope struct {
	// Visible reports whether a task's service may be counted ("" when the
	// service is unknown). Tasks of other services are dropped, and their
	// addresses count as a hidden service's.
	Visible func(service string) bool
	// Hidden reports whether a service name an endpoint carries (its
	// networkdb service, or the service in its task container name) is a
	// service that exists and is hidden. A name that is neither visible nor
	// hidden (a service since removed) keeps its endpoint: a leftover no live
	// task explains is exactly what a stale candidate is.
	Hidden func(service string) bool
}

// ScopeEndpoints drops from both views what belongs to a hidden service:
// manager tasks whose service is not visible, and node endpoints filed under
// or named after a hidden service or holding one of its task addresses (per
// network). Load balancers and unattributable endpoints stay. The node
// responses are copied, never changed in place. With nothing hidden, the
// inputs come back with the same content.
func ScopeEndpoints(tasks []TaskAddr, nodes []agentcall.NodeNetworkEndpoints, sc EndpointScope) ([]TaskAddr, []agentcall.NodeNetworkEndpoints) {
	f := endpointFilter{hidden: sc.Hidden, hiddenIPs: map[string]map[string]bool{}}
	var kept []TaskAddr
	for _, t := range tasks {
		if sc.Visible(t.Service) {
			kept = append(kept, t)
			continue
		}
		if f.hiddenIPs[t.NetworkID] == nil {
			f.hiddenIPs[t.NetworkID] = map[string]bool{}
		}
		for _, ip := range t.IPs {
			f.hiddenIPs[t.NetworkID][ip] = true
		}
	}
	out := make([]agentcall.NodeNetworkEndpoints, 0, len(nodes))
	for _, n := range nodes {
		if n.Err != nil || n.Resp == nil {
			out = append(out, n)
			continue
		}
		resp, _ := proto.Clone(n.Resp).(*pb.NetworkEndpointsResponse)
		for _, ni := range resp.GetNetworks() {
			f.scopeNetwork(ni)
		}
		out = append(out, agentcall.NodeNetworkEndpoints{Node: n.Node, Resp: resp})
	}
	return kept, out
}

// endpointFilter is ScopeEndpoints' rule for one node response: what a hidden
// service is called, and the addresses its tasks hold per network.
type endpointFilter struct {
	hidden    func(service string) bool
	hiddenIPs map[string]map[string]bool // network id -> ip
}

// hiddenName reports whether svc — or, when it is empty, the service in the
// task container name — is a hidden service.
func (f endpointFilter) hiddenName(svc, name string) bool {
	if svc == "" {
		svc = TaskServiceName(name)
	}
	return svc != "" && f.hidden(svc)
}

// scopeNetwork drops ni's local and task endpoints of hidden services, in
// place (ni is a copy). Load balancers stay.
func (f endpointFilter) scopeNetwork(ni *pb.NetworkEndpointsInfo) {
	hid := f.hiddenIPs[ni.GetId()]
	var locals []*pb.NetworkLocalEndpoint
	for _, l := range ni.GetLocalEndpoints() {
		if !l.GetLoadBalancer() && (f.hiddenName("", l.GetName()) || hid[StripMask(l.GetIpv4Address())]) {
			continue
		}
		locals = append(locals, l)
	}
	ni.LocalEndpoints = locals
	for _, s := range ni.GetServices() {
		var ts []*pb.NetworkTaskEndpoint
		for _, t := range s.GetTasks() {
			lb := s.GetName() == "" && lbTaskName(t.GetName())
			if !lb && (f.hiddenName(s.GetName(), t.GetName()) || hid[StripMask(t.GetEndpointIp())]) {
				continue
			}
			ts = append(ts, t)
		}
		s.Tasks = ts
	}
}
