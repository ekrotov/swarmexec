// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"sort"
	"testing"

	"github.com/docker/docker/api/types/swarm"
)

func svc(name string, taskNets, specNets []string) swarm.Service {
	var s swarm.Service
	s.Spec.Name = name
	for _, n := range taskNets {
		s.Spec.TaskTemplate.Networks = append(s.Spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: n})
	}
	for _, n := range specNets {
		s.Spec.Networks = append(s.Spec.Networks, swarm.NetworkAttachmentConfig{Target: n})
	}
	return s
}

func TestServiceNetworkMembership(t *testing.T) {
	svcs := []swarm.Service{
		svc("web", []string{"frontend"}, nil),
		svc("api", []string{"frontend", "backend"}, nil),
		// Legacy service that still uses the deprecated Spec.Networks field.
		svc("legacy", nil, []string{"backend"}),
		// A network referenced twice (both spec locations) must not double-count.
		svc("both", []string{"backend"}, []string{"backend"}),
		// Empty target is ignored.
		svc("noop", []string{""}, nil),
	}
	got := serviceNetworkMembership(svcs)
	for _, m := range got {
		sort.Strings(m)
	}
	want := map[string][]string{
		"frontend": {"api", "web"},
		"backend":  {"api", "both", "legacy"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("serviceNetworkMembership = %v, want %v", got, want)
	}
}

func TestNetworkType(t *testing.T) {
	cases := []struct {
		n    swarmNetwork
		want string
	}{
		{swarmNetwork{Ingress: true}, "ingress"},
		{swarmNetwork{Internal: true}, "internal"},
		{swarmNetwork{Attachable: true}, "attachable"},
		{swarmNetwork{}, "-"},
		// Ingress wins when several flags are set.
		{swarmNetwork{Ingress: true, Internal: true, Attachable: true}, "ingress"},
	}
	for _, c := range cases {
		if got := networkType(c.n); got != c.want {
			t.Errorf("networkType(%+v) = %q, want %q", c.n, got, c.want)
		}
	}
}
