// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package swarmops

import (
	"reflect"
	"sort"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/swarm"
)

// specLines characterization: every rendered field, in its order.
func TestSpecLinesFullSpec(t *testing.T) {
	three := uint64(3)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Labels: map[string]string{"tier": "web"}},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: "nginx:1", Command: []string{"nginx", "-g"}, Args: []string{"daemon off;"},
				Env:     []string{"A=1", "B=2"},
				Secrets: []*swarm.SecretReference{{SecretName: "tls"}},
				Mounts:  []mount.Mount{{Type: mount.TypeVolume, Source: "data", Target: "/data"}},
			},
			Networks: []swarm.NetworkAttachmentConfig{
				{Target: "id1", Aliases: []string{"www"}},
				{Target: "unknown-id"},
			},
			Placement: &swarm.Placement{
				Constraints: []string{"node.role==worker"},
				Preferences: []swarm.PlacementPreference{{Spread: &swarm.SpreadOver{SpreadDescriptor: "node.labels.az"}}, {}},
			},
			Resources: &swarm.ResourceRequirements{
				Limits:       &swarm.Limit{NanoCPUs: 1_500_000_000, MemoryBytes: 512},
				Reservations: &swarm.Resources{NanoCPUs: 250_000_000, MemoryBytes: 128},
			},
		},
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &three}},
		EndpointSpec: &swarm.EndpointSpec{Ports: []swarm.PortConfig{{Protocol: network.TCP, TargetPort: 80, PublishedPort: 8080}}},
	}
	got := specLines(spec, map[string]string{"id1": "front"})
	want := []string{
		"image: nginx:1",
		"command: nginx -g",
		"args: daemon off;",
		"env: A=1",
		"env: B=2",
		"secret: tls",
		"mount: " + FormatServiceMount(spec.TaskTemplate.ContainerSpec.Mounts[0]),
		"mode: replicated 3",
		"label: tier=web",
		"network: front",
		"alias: front/www",
		"network: unknown-id",
		"constraint: node.role==worker",
		"spread: node.labels.az",
		"port: " + FormatServicePort(spec.EndpointSpec.Ports[0]),
		"cpu-limit: 1.50",
		"mem-limit: 512",
		"cpu-reservation: 0.25",
		"mem-reservation: 128",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("specLines =\n%q\nwant\n%q", got, want)
	}
}

func TestSpecLinesModesAndEmpties(t *testing.T) {
	two := uint64(2)
	cases := []struct {
		name string
		spec swarm.ServiceSpec
		want []string
	}{
		{"empty spec", swarm.ServiceSpec{}, nil},
		{"global", swarm.ServiceSpec{Mode: swarm.ServiceMode{Global: &swarm.GlobalService{}}}, []string{"mode: global"}},
		{"replicated without count", swarm.ServiceSpec{Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{}}}, nil},
		{"replicated job", swarm.ServiceSpec{Mode: swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{TotalCompletions: &two}}},
			[]string{"mode: replicated-job 2"}},
		{"global job", swarm.ServiceSpec{Mode: swarm.ServiceMode{GlobalJob: &swarm.GlobalJob{}}}, []string{"mode: global-job"}},
		{"bare container", swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{Image: "x"}}},
			[]string{"image: x"}},
		{"zero resources", swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{Resources: &swarm.ResourceRequirements{
			Limits: &swarm.Limit{}, Reservations: &swarm.Resources{}}}}, nil},
		{"empty placement and endpoint", swarm.ServiceSpec{EndpointSpec: &swarm.EndpointSpec{},
			TaskTemplate: swarm.TaskSpec{Placement: &swarm.Placement{}}}, nil},
	}
	for _, c := range cases {
		if got := specLines(c.spec, nil); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSpecLinesLabels(t *testing.T) {
	got := specLines(swarm.ServiceSpec{Annotations: swarm.Annotations{Labels: map[string]string{"b": "2", "a": "1"}}}, nil)
	sort.Strings(got)
	if want := []string{"label: a=1", "label: b=2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %q", got)
	}
}
