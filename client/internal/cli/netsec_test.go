// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"sort"
	"testing"

	"github.com/docker/docker/api/types/mount"
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

func secretSvc(name string, secretRefs ...swarm.SecretReference) swarm.Service {
	var s swarm.Service
	s.Spec.Name = name
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{}
	for i := range secretRefs {
		s.Spec.TaskTemplate.ContainerSpec.Secrets = append(s.Spec.TaskTemplate.ContainerSpec.Secrets, &secretRefs[i])
	}
	return s
}

func TestServiceSecretMembership(t *testing.T) {
	svcs := []swarm.Service{
		secretSvc("web", swarm.SecretReference{SecretID: "sid1", SecretName: "db-pw"}),
		secretSvc("api", swarm.SecretReference{SecretID: "sid1", SecretName: "db-pw"}, swarm.SecretReference{SecretID: "sid2", SecretName: "api-key"}),
		// A service with no container spec must not panic.
		{Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "bare"}}},
	}
	got := serviceSecretMembership(svcs)
	for _, m := range got {
		sort.Strings(m)
	}
	// Keyed by both ID and name so listSecrets can match either.
	want := map[string][]string{
		"sid1":    {"api", "web"},
		"db-pw":   {"api", "web"},
		"sid2":    {"api"},
		"api-key": {"api"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("serviceSecretMembership = %v, want %v", got, want)
	}
}

func volSvc(name string, volumeMounts ...string) swarm.Service {
	var s swarm.Service
	s.Spec.Name = name
	s.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{}
	for _, v := range volumeMounts {
		s.Spec.TaskTemplate.ContainerSpec.Mounts = append(s.Spec.TaskTemplate.ContainerSpec.Mounts,
			mount.Mount{Type: mount.TypeVolume, Source: v})
	}
	return s
}

func TestServiceVolumeMounts(t *testing.T) {
	svcs := []swarm.Service{
		volSvc("db", "pgdata"),
		volSvc("web", "pgdata", "assets"), // shared volume + own
		// A bind mount (not a volume) is ignored.
		func() swarm.Service {
			s := volSvc("logger")
			s.Spec.TaskTemplate.ContainerSpec.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/var/log"}}
			return s
		}(),
		// No container spec must not panic.
		{Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "bare"}}},
	}
	got := serviceVolumeMounts(svcs)
	want := map[string]bool{"pgdata": true, "assets": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("serviceVolumeMounts = %v, want %v", got, want)
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
		{swarmNetwork{Driver: "overlay"}, "overlay"},
		{swarmNetwork{Driver: "bridge"}, "bridge"},
		{swarmNetwork{}, "-"},
		// Attachable wins when several flags are set (matches the colour priority).
		{swarmNetwork{Ingress: true, Internal: true, Attachable: true}, "attachable"},
		{swarmNetwork{Ingress: true, Internal: true}, "internal"},
	}
	for _, c := range cases {
		if got := networkType(c.n); got != c.want {
			t.Errorf("networkType(%+v) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestServicesExcluding(t *testing.T) {
	got := servicesExcluding([]string{"web", "db", "cache"}, []string{"db"})
	want := []string{"cache", "web"} // sorted, db removed
	if len(got) != len(want) {
		t.Fatalf("servicesExcluding = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("servicesExcluding = %v, want %v", got, want)
		}
	}
	// Nothing excluded → all, sorted.
	if all := servicesExcluding([]string{"b", "a"}, nil); all[0] != "a" || all[1] != "b" {
		t.Errorf("servicesExcluding(no exclude) = %v, want [a b]", all)
	}
}
