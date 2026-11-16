// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"swarmexec/agent/internal/auth"
	"swarmexec/internal/pb"
)

// An overlay network's state is per node: each daemon keeps its own copy of
// the network's endpoints (networkdb) and programs the kernel's forwarding
// tables from it. A node that missed a departure keeps the old IP/MAC entries
// — the manager has long forgotten them, so only the node can show them.
// NetworkEndpoints reports that copy through the Docker API alone (the kernel
// FDB itself would need NET_ADMIN in the host's namespaces, which the agent
// deliberately does not have); a client compares it with the manager's tasks.

// maxNetworkIDs bounds an explicit network_ids list: each one is an inspect
// against the daemon, and no real cluster has more swarm networks than this.
const maxNetworkIDs = 256

// Docker keys a network's load-balancer endpoint as "lb-<network>", and the
// routing mesh's as "ingress-sbox".
const (
	lbEndpointPrefix  = "lb-"
	ingressSandboxKey = "ingress-sbox"
)

// NetworkEndpoints reports, per swarm-scope network this node has joined (or
// per requested network), the endpoints attached here and the daemon's
// networkdb view of the whole network.
func (s *Server) NetworkEndpoints(ctx context.Context, req *pb.NetworkEndpointsRequest) (*pb.NetworkEndpointsResponse, error) {
	if _, err := s.authorizeUnary(ctx, auth.Request{Action: "network.inspect"}); err != nil {
		return nil, err
	}

	ids, explicit, err := s.networkEndpointTargets(ctx, req.GetNetworkIds())
	if err != nil {
		return nil, err
	}

	out := &pb.NetworkEndpointsResponse{}
	seen := map[string]bool{}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, dockerStatus(err, "inspect networks")
		}
		res, err := s.docker.NetworkInspect(ctx, id, client.NetworkInspectOptions{Verbose: true, Scope: "swarm"})
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, dockerStatus(err, "inspect networks")
		case err != nil && !explicit && cerrdefs.IsNotFound(err):
			// Removed between the listing and the inspect: nothing to report.
			continue
		case err != nil:
			msg := "inspect network: " + err.Error()
			if cerrdefs.IsNotFound(err) {
				msg = "network not found on this node"
			}
			s.log.Warn("NetworkInspect failed", "network", id, "err", err)
			out.Networks = append(out.Networks, &pb.NetworkEndpointsInfo{Id: id, Error: msg})
			continue
		}
		in := res.Network
		if seen[in.ID] {
			continue // asked by ID and by name, or listed twice
		}
		seen[in.ID] = true
		switch {
		case in.Scope != "swarm":
			// The daemon was asked for swarm scope; this guards one that ignores it.
			if explicit {
				out.Networks = append(out.Networks, &pb.NetworkEndpointsInfo{Id: id, Error: "not a swarm-scope network"})
			}
			continue
		case !explicit && !networkJoined(in):
			// A manager lists every swarm network in the cluster, and inspecting one
			// this node never instantiated returns the bare cluster object.
			continue
		}
		out.Networks = append(out.Networks, networkEndpointsInfo(in))
	}
	slices.SortStableFunc(out.Networks, func(a, b *pb.NetworkEndpointsInfo) int {
		return cmp.Or(cmp.Compare(a.GetName(), b.GetName()), cmp.Compare(a.GetId(), b.GetId()))
	})
	return out, nil
}

// networkEndpointTargets is what to inspect: the requested networks
// (deduplicated, explicit=true), or every swarm-scope network the daemon lists.
func (s *Server) networkEndpointTargets(ctx context.Context, requested []string) (ids []string, explicit bool, err error) {
	if len(requested) > 0 {
		if len(requested) > maxNetworkIDs {
			return nil, false, status.Errorf(codes.InvalidArgument, "too many network_ids (%d, limit %d)", len(requested), maxNetworkIDs)
		}
		for _, id := range requested {
			id = strings.TrimSpace(id)
			if id == "" {
				return nil, false, status.Error(codes.InvalidArgument, "empty network id")
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		return ids, true, nil
	}
	list, err := s.docker.NetworkList(ctx, client.NetworkListOptions{Filters: make(client.Filters).Add("scope", "swarm")})
	if err != nil {
		s.log.Error("NetworkList failed", "err", err)
		return nil, false, dockerStatus(err, "list networks")
	}
	for _, n := range list.Items {
		if n.Scope == "swarm" && !slices.Contains(ids, n.ID) {
			ids = append(ids, n.ID)
		}
	}
	return ids, false, nil
}

// networkJoined reports whether this daemon has the network instantiated: an
// instantiated overlay lists at least this node among its peers, and any local
// endpoint or networkdb entry means it is in use here.
func networkJoined(in network.Inspect) bool {
	return len(in.Peers) > 0 || len(in.Containers) > 0 || len(in.Services) > 0
}

// networkEndpointsInfo shapes one inspected network for the wire, sorted so a
// client can diff two answers line by line.
func networkEndpointsInfo(in network.Inspect) *pb.NetworkEndpointsInfo {
	out := &pb.NetworkEndpointsInfo{Id: in.ID, Name: in.Name, Driver: in.Driver, Ingress: in.Ingress}
	for key, ep := range in.Containers {
		out.LocalEndpoints = append(out.LocalEndpoints, &pb.NetworkLocalEndpoint{
			ContainerId:  key,
			Name:         ep.Name,
			EndpointId:   ep.EndpointID,
			Ipv4Address:  prefixString(ep.IPv4Address.IsValid(), ep.IPv4Address.String),
			Ipv6Address:  prefixString(ep.IPv6Address.IsValid(), ep.IPv6Address.String),
			MacAddress:   macString(ep.MacAddress),
			LoadBalancer: strings.HasPrefix(key, lbEndpointPrefix) || key == ingressSandboxKey,
		})
	}
	slices.SortFunc(out.LocalEndpoints, func(a, b *pb.NetworkLocalEndpoint) int {
		return cmp.Or(cmp.Compare(a.GetName(), b.GetName()), cmp.Compare(a.GetContainerId(), b.GetContainerId()))
	})
	for name, svc := range in.Services {
		se := &pb.NetworkServiceEndpoints{Name: name, Vip: prefixString(svc.VIP.IsValid(), svc.VIP.String)}
		for _, t := range svc.Tasks {
			se.Tasks = append(se.Tasks, &pb.NetworkTaskEndpoint{
				Name:       t.Name,
				EndpointId: t.EndpointID,
				EndpointIp: prefixString(t.EndpointIP.IsValid(), t.EndpointIP.String),
				HostIp:     t.Info["Host IP"],
			})
		}
		slices.SortFunc(se.Tasks, func(a, b *pb.NetworkTaskEndpoint) int {
			return cmp.Or(cmp.Compare(a.GetName(), b.GetName()), cmp.Compare(a.GetEndpointId(), b.GetEndpointId()))
		})
		out.Services = append(out.Services, se)
	}
	slices.SortFunc(out.Services, func(a, b *pb.NetworkServiceEndpoints) int {
		return cmp.Compare(a.GetName(), b.GetName())
	})
	peers := slices.Clone(in.Peers)
	slices.SortFunc(peers, func(a, b network.PeerInfo) int {
		return cmp.Or(a.IP.Compare(b.IP), cmp.Compare(a.Name, b.Name))
	})
	for _, p := range peers {
		out.Peers = append(out.Peers, &pb.NetworkPeer{Name: p.Name, Ip: prefixString(p.IP.IsValid(), p.IP.String)})
	}
	return out
}

// prefixString renders an address or prefix, or "" for the zero value — which
// netip would otherwise print as "invalid Prefix" / "invalid IP".
func prefixString(valid bool, str func() string) string {
	if !valid {
		return ""
	}
	return str()
}

func macString(mac network.HardwareAddr) string {
	if len(mac) == 0 {
		return ""
	}
	return mac.String()
}
