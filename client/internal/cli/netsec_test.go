// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"reflect"
	"sort"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
)

func svc(name string, taskNets, specNets []string) swarm.Service {
	var s swarm.Service
	s.Spec.Name = name
	for _, n := range taskNets {
		s.Spec.TaskTemplate.Networks = append(s.Spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: n})
	}
	for _, n := range specNets {
		//lint:ignore SA1019 compat: services created before API 1.44 carry their networks in Spec.Networks
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

func TestServiceAliasesOnNetwork(t *testing.T) {
	var s swarm.Service
	s.Spec.Name = "web"
	// Attached by network ID on the task template, with two aliases.
	s.Spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{
		{Target: "net-id-1", Aliases: []string{"web", "frontend"}},
	}
	// A second attachment referenced by NAME on the deprecated spec field.
	//lint:ignore SA1019 compat: services created before API 1.44 carry their networks in Spec.Networks
	s.Spec.Networks = []swarm.NetworkAttachmentConfig{
		{Target: "backend", Aliases: []string{"api"}},
	}

	cases := []struct {
		name string
		net  swarmNetwork
		want []string
	}{
		{"by id", swarmNetwork{ID: "net-id-1", Name: "frontend-net"}, []string{"web", "frontend"}},
		{"by name", swarmNetwork{ID: "backend-id", Name: "backend"}, []string{"api"}},
		{"not attached", swarmNetwork{ID: "other-id", Name: "other"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serviceAliasesOnNetwork(s, tc.net)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("serviceAliasesOnNetwork = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNetworkEncrypted(t *testing.T) {
	cases := []struct {
		opts map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{}, false},
		{map[string]string{"encrypted": ""}, true},     // docker CLI's --opt encrypted form
		{map[string]string{"encrypted": "true"}, true}, // our explicit form
		{map[string]string{"encrypted": "false"}, false},
		{map[string]string{"encrypted": "0"}, false},
		{map[string]string{"com.docker.network.driver.mtu": "1400"}, false},
	}
	for _, c := range cases {
		if got := networkEncrypted(c.opts); got != c.want {
			t.Errorf("networkEncrypted(%v) = %v, want %v", c.opts, got, c.want)
		}
	}
}

func TestBuildNetworkCreateOptions(t *testing.T) {
	// Full happy path: every option set.
	name, opts, err := buildNetworkCreateOptions(newNetworkOpts{
		Name: "  app-net ", Driver: "", Attachable: true, Encrypted: true,
		Internal: true, IPv6: true, MTU: "1400", Subnet: "10.10.0.0/24",
		Gateway: "10.10.0.1", Labels: map[string]string{"team": "infra"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "app-net" {
		t.Errorf("name = %q, want trimmed %q", name, "app-net")
	}
	if opts.Driver != "overlay" {
		t.Errorf("driver = %q, want default overlay", opts.Driver)
	}
	if !opts.Attachable || !opts.Internal {
		t.Errorf("attachable/internal not set: %+v", opts)
	}
	if opts.Options["encrypted"] != "true" {
		t.Errorf("encrypted opt = %q, want true", opts.Options["encrypted"])
	}
	if opts.Options["com.docker.network.driver.mtu"] != "1400" {
		t.Errorf("mtu opt = %q", opts.Options["com.docker.network.driver.mtu"])
	}
	if opts.EnableIPv6 == nil || !*opts.EnableIPv6 {
		t.Errorf("ipv6 not enabled")
	}
	wantIPAM := []network.IPAMConfig{{Subnet: "10.10.0.0/24", Gateway: "10.10.0.1"}}
	if opts.IPAM == nil || !reflect.DeepEqual(opts.IPAM.Config, wantIPAM) {
		t.Errorf("ipam = %+v, want %+v", opts.IPAM, wantIPAM)
	}
	if opts.Labels["team"] != "infra" {
		t.Errorf("labels = %v", opts.Labels)
	}

	// Minimal: only a name → overlay, no driver options at all.
	_, min, err := buildNetworkCreateOptions(newNetworkOpts{Name: "n"})
	if err != nil {
		t.Fatalf("minimal: %v", err)
	}
	if min.Options != nil {
		t.Errorf("expected nil Options when nothing set, got %v", min.Options)
	}

	// Error cases.
	if _, _, err := buildNetworkCreateOptions(newNetworkOpts{Name: ""}); err == nil {
		t.Error("expected error for empty name")
	}
	if _, _, err := buildNetworkCreateOptions(newNetworkOpts{Name: "n", MTU: "big"}); err == nil {
		t.Error("expected error for non-numeric MTU")
	}
	if _, _, err := buildNetworkCreateOptions(newNetworkOpts{Name: "n", Gateway: "10.0.0.1"}); err == nil {
		t.Error("expected error for gateway without subnet")
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

func TestDropNetwork(t *testing.T) {
	nets := []swarm.NetworkAttachmentConfig{{Target: "a"}, {Target: "b"}, {Target: "a"}}
	got, removed := dropNetwork(nets, func(target string) bool { return target == "a" })
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if len(got) != 1 || got[0].Target != "b" {
		t.Errorf("kept = %v, want [{b}]", got)
	}
	if _, r := dropNetwork(nets, func(string) bool { return false }); r != 0 {
		t.Errorf("removed = %d, want 0 when nothing matches", r)
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

func TestOrphanSecrets(t *testing.T) {
	web := secretSvc("web",
		swarm.SecretReference{SecretID: "sid1", SecretName: "db-pw"},
		swarm.SecretReference{SecretID: "sid2", SecretName: "shared"})
	api := secretSvc("api", swarm.SecretReference{SecretID: "sid2", SecretName: "shared"})
	all := []swarm.Service{web, api}

	got := orphanSecrets(web, all)
	// db-pw is used only by web → orphaned; shared is also used by api → not.
	if len(got) != 1 || got[0].Name != "db-pw" {
		t.Errorf("orphanSecrets(web) = %+v, want just db-pw", got)
	}
	// api's only secret is shared, still used by web → no orphans.
	if o := orphanSecrets(api, all); len(o) != 0 {
		t.Errorf("orphanSecrets(api) = %+v, want none", o)
	}
	// A service with no container spec / no secrets → no orphans.
	if o := orphanSecrets(swarm.Service{Spec: swarm.ServiceSpec{Annotations: swarm.Annotations{Name: "bare"}}}, all); len(o) != 0 {
		t.Errorf("orphanSecrets(bare) = %+v, want none", o)
	}
}
